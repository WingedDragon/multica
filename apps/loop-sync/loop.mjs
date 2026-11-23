const TASKS_PATH = "/fleet/api/v1/tasks";
const PAGE_SIZE = 100;

class LoopApiError extends Error {
  constructor(status, statusText, body) {
    const detail = body?.error?.message ?? body?.message;
    const statusLabel = [status, statusText].filter(Boolean).join(" ");
    super(`Loop API request failed (${statusLabel})${detail ? `: ${detail}` : ""}`);
    this.name = "LoopApiError";
    this.status = status;
    this.body = body;
  }
}

export class Loop {
  #baseUrl;
  #token;
  #workspaceId;

  constructor({ baseUrl, token, workspaceId }) {
    this.#baseUrl = baseUrl.replace(/\/+$/, "");
    this.#token = token;
    this.#workspaceId = workspaceId;
  }

  async listOpenTasks(memberId) {
    return this.#listAll(TASKS_PATH, {
      assignee_octo_uid: memberId,
      open_only: true,
    });
  }

  async getTask(id) {
    const response = await this.#request(
      "GET",
      `${TASKS_PATH}/${encodeURIComponent(id)}`,
    );
    return response.data;
  }

  async listComments(id) {
    return this.#listAll(
      `${TASKS_PATH}/${encodeURIComponent(id)}/comments`,
    );
  }

  async previewComment(id, content) {
    const response = await this.#request(
      "POST",
      `${TASKS_PATH}/${encodeURIComponent(id)}/comments/_trigger_preview`,
      { body: { content } },
    );
    return response.data;
  }

  async createComment(id, content) {
    const response = await this.#request(
      "POST",
      `${TASKS_PATH}/${encodeURIComponent(id)}/comments`,
      { body: { content } },
    );
    return response.data;
  }

  async #listAll(path, query = {}) {
    const items = [];

    for (let page = 1; ; page += 1) {
      const response = await this.#request("GET", path, {
        query: { ...query, page, page_size: PAGE_SIZE },
      });
      const { data, pagination } = response;
      if (
        !Array.isArray(data) ||
        !Number.isInteger(pagination?.total) ||
        !Number.isInteger(pagination?.page_size) ||
        pagination.page_size < 1
      ) {
        throw new TypeError("Invalid paginated response from Loop API");
      }

      items.push(...data);
      if (page * pagination.page_size >= pagination.total) return items;
    }
  }

  async #request(method, path, { query, body } = {}) {
    const url = new URL(`${this.#baseUrl}${path}`);
    if (query) {
      for (const [key, value] of Object.entries(query)) {
        url.searchParams.set(key, String(value));
      }
    }

    const headers = {
      Authorization: `Bearer ${this.#token}`,
      "X-Workspace-ID": this.#workspaceId,
      Accept: "application/vnd.octo+json",
    };
    const options = { method, headers };
    if (body !== undefined) {
      headers["Content-Type"] = "application/json";
      options.body = JSON.stringify(body);
    }

    const response = await fetch(url, options);
    const responseBody = await response.json();
    if (!response.ok) {
      throw new LoopApiError(
        response.status,
        response.statusText,
        responseBody,
      );
    }
    return responseBody;
  }
}
