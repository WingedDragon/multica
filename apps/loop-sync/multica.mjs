const COMMENT_PAGE_SIZE = 2000;
const NEXT_BEFORE_HEADER = "X-Multica-Next-Before";
const NEXT_BEFORE_ID_HEADER = "X-Multica-Next-Before-Id";

export class Multica {
  constructor({ baseUrl, token, workspaceId }) {
    this.baseUrl = baseUrl.replace(/\/+$/, "");
    this.token = token;
    this.workspaceId = workspaceId;
  }

  async #request(method, path, body) {
    const headers = new Headers();
    if (this.token) headers.set("Authorization", `Bearer ${this.token}`);
    if (this.workspaceId) headers.set("X-Workspace-ID", this.workspaceId);
    if (body !== undefined) headers.set("Content-Type", "application/json");

    const response = await fetch(`${this.baseUrl}${path}`, {
      method,
      headers,
      ...(body === undefined ? {} : { body: JSON.stringify(body) }),
    });

    if (!response.ok) {
      const detail = await response.text();
      const status = `${response.status}${response.statusText ? ` ${response.statusText}` : ""}`;
      throw new Error(`${method} ${path} failed with ${status}${detail ? `: ${detail}` : ""}`);
    }

    return response;
  }

  async #requestJSON(method, path, body) {
    return (await this.#request(method, path, body)).json();
  }

  #issuePath(id) {
    return `/api/issues/${encodeURIComponent(id)}`;
  }

  async createIssue(fields) {
    return this.#requestJSON("POST", "/api/issues", fields);
  }

  async getIssue(id) {
    return this.#requestJSON("GET", this.#issuePath(id));
  }

  async updateIssue(id, fields) {
    return this.#requestJSON("PUT", this.#issuePath(id), fields);
  }

  async listComments(id) {
    const issuePath = `${this.#issuePath(id)}/comments`;
    const comments = [];
    const seenCursors = new Set();
    let before;
    let beforeId;

    while (true) {
      const query = new URLSearchParams({ recent: String(COMMENT_PAGE_SIZE) });
      if (before !== undefined) {
        query.set("before", before);
        query.set("before_id", beforeId);
      }

      const response = await this.#request("GET", `${issuePath}?${query}`);
      const page = await response.json();
      if (!Array.isArray(page)) {
        throw new TypeError(`GET ${issuePath} returned a non-array comment list`);
      }
      comments.push(...page);

      const nextBefore = response.headers.get(NEXT_BEFORE_HEADER);
      const nextBeforeId = response.headers.get(NEXT_BEFORE_ID_HEADER);
      if (!nextBefore && !nextBeforeId) return comments;
      if (!nextBefore || !nextBeforeId) {
        throw new Error(`GET ${issuePath} returned an incomplete comment pagination cursor`);
      }

      const cursor = `${nextBefore}\u0000${nextBeforeId}`;
      if (seenCursors.has(cursor)) {
        throw new Error(`GET ${issuePath} repeated a comment pagination cursor`);
      }
      seenCursors.add(cursor);
      before = nextBefore;
      beforeId = nextBeforeId;
    }
  }

  async createComment(id, content) {
    return this.#requestJSON("POST", `${this.#issuePath(id)}/comments`, { content });
  }
}
