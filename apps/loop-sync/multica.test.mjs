import assert from "node:assert/strict";
import { test } from "node:test";
import { Multica } from "./multica.mjs";

const baseUrl = "https://multica.example/";
const token = "mat_test_token";
const workspaceId = "workspace-123";

function jsonResponse(value, { status = 200, headers = {} } = {}) {
  return new Response(JSON.stringify(value), {
    status,
    headers: { "content-type": "application/json", ...headers },
  });
}

async function withFetch(fetchImpl, fn) {
  const originalFetch = globalThis.fetch;
  globalThis.fetch = fetchImpl;
  try {
    return await fn();
  } finally {
    globalThis.fetch = originalFetch;
  }
}

test("issue and comment methods use the Multica API contract and return raw JSON", async () => {
  const createdIssue = { id: "issue-1", title: "Created", custom: { preserved: true } };
  const fetchedIssue = { id: "issue-1", title: "Fetched", extra: [1, 2] };
  const updatedIssue = { id: "issue-1", title: "Updated", status: "backlog" };
  const createdComment = { id: "comment-1", issue_id: "issue-1", author_type: "member", type: "comment" };
  const fields = { title: "Created", status: "backlog", properties: { priority: { value: 4 } } };
  const requests = [];
  const responses = [
    jsonResponse(createdIssue, { status: 201 }),
    jsonResponse(fetchedIssue),
    jsonResponse(updatedIssue),
    jsonResponse(createdComment, { status: 201 }),
  ];

  await withFetch(async (input, init = {}) => {
    requests.push({ url: String(input), init });
    return responses.shift();
  }, async () => {
    const client = new Multica({ baseUrl, token, workspaceId });

    assert.deepEqual(await client.createIssue(fields), createdIssue);
    assert.deepEqual(await client.getIssue("issue-1"), fetchedIssue);
    assert.deepEqual(await client.updateIssue("issue-1", { status: "backlog", description: "Details" }), updatedIssue);
    assert.deepEqual(await client.createComment("issue-1", "Finished work"), createdComment);
  });

  assert.equal(requests.length, 4);
  assert.deepEqual(requests.map(({ init }) => init.method), ["POST", "GET", "PUT", "POST"]);
  assert.deepEqual(requests.map(({ url }) => new URL(url).pathname), [
    "/api/issues",
    "/api/issues/issue-1",
    "/api/issues/issue-1",
    "/api/issues/issue-1/comments",
  ]);

  for (const { init } of requests) {
    const headers = new Headers(init.headers);
    assert.equal(headers.get("authorization"), `Bearer ${token}`);
    assert.equal(headers.get("x-workspace-id"), workspaceId);
  }
  assert.equal(new Headers(requests[0].init.headers).get("content-type"), "application/json");
  assert.deepEqual(JSON.parse(requests[0].init.body), fields);
  assert.deepEqual(JSON.parse(requests[3].init.body), { content: "Finished work" });
  assert.equal(new Headers(requests[1].init.headers).get("content-type"), null);
});

test("create and update send the caller's fields as JSON", async () => {
  const fields = { title: "Created", status: "todo", nested: { value: true } };
  const updateFields = { description: "Details", assignee_type: null, assignee_id: null };
  const requests = [];

  await withFetch(async (input, init = {}) => {
    requests.push({ url: String(input), init });
    return jsonResponse({ id: "issue-1" }, { status: init.method === "POST" ? 201 : 200 });
  }, async () => {
    const client = new Multica({ baseUrl, token, workspaceId });
    await client.createIssue(fields);
    await client.updateIssue("issue-1", updateFields);
  });

  assert.deepEqual(JSON.parse(requests[0].init.body), fields);
  assert.deepEqual(JSON.parse(requests[1].init.body), updateFields);
});

test("listComments follows every recent-thread cursor and preserves raw comments", async () => {
  const newerThreadComment = { id: "comment-new", content: "newer", created_at: "2025-02-02T12:00:00Z", type: "system" };
  const olderThreadComment = { id: "comment-old", content: "older", created_at: "2025-02-01T12:00:00Z", author_type: "agent" };
  const cursorTime = "2025-02-01T12:00:00.123456789Z";
  const requests = [];
  let call = 0;

  const comments = await withFetch(async (input, init = {}) => {
    requests.push({ url: String(input), init });
    call += 1;
    if (call === 1) {
      return jsonResponse([newerThreadComment], {
        headers: {
          "X-Multica-Next-Before": cursorTime,
          "X-Multica-Next-Before-Id": "root-old",
        },
      });
    }
    return jsonResponse([olderThreadComment]);
  }, async () => {
    const client = new Multica({ baseUrl, token, workspaceId });
    return client.listComments("issue-1");
  });

  assert.deepEqual(comments, [newerThreadComment, olderThreadComment]);
  assert.equal(requests.length, 2);
  const firstUrl = new URL(requests[0].url);
  assert.equal(firstUrl.pathname, "/api/issues/issue-1/comments");
  assert.equal(firstUrl.searchParams.get("recent"), "2000");
  assert.equal(firstUrl.searchParams.has("before"), false);

  const nextUrl = new URL(requests[1].url);
  assert.equal(nextUrl.searchParams.get("recent"), "2000");
  assert.equal(nextUrl.searchParams.get("before"), cursorTime);
  assert.equal(nextUrl.searchParams.get("before_id"), "root-old");
  for (const { init } of requests) {
    const headers = new Headers(init.headers);
    assert.equal(headers.get("authorization"), `Bearer ${token}`);
    assert.equal(headers.get("x-workspace-id"), workspaceId);
  }
});

test("HTTP errors are surfaced with their response status and body", async () => {
  await withFetch(async () => new Response("workspace access denied", { status: 403, statusText: "Forbidden" }), async () => {
    const client = new Multica({ baseUrl, token, workspaceId });
    await assert.rejects(client.getIssue("issue-1"), /403.*workspace access denied/);
  });
});
