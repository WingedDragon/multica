import assert from "node:assert/strict";
import test from "node:test";
import { Loop } from "./loop.mjs";

const config = {
  baseUrl: "https://loop.example/",
  token: "test-token",
  workspaceId: "workspace-1",
};

function jsonResponse(body, status = 200, statusText = "OK") {
  return {
    ok: status >= 200 && status < 300,
    status,
    statusText,
    json: async () => body,
  };
}

function mockFetch(t, implementation) {
  const originalFetch = globalThis.fetch;
  globalThis.fetch = implementation;
  t.after(() => {
    globalThis.fetch = originalFetch;
  });
}

function assertHeaders(headers, withBody = false) {
  assert.deepEqual(headers, {
    Authorization: "Bearer test-token",
    "X-Workspace-ID": "workspace-1",
    Accept: "application/vnd.octo+json",
    ...(withBody ? { "Content-Type": "application/json" } : {}),
  });
}

test("listOpenTasks sends the open member filter and collects every page", async (t) => {
  const firstPage = Array.from({ length: 100 }, (_, index) => ({
    id: `task-${index + 1}`,
    title: `Task ${index + 1}`,
  }));
  const lastTask = { id: "task-101", title: "Task 101" };
  const requests = [];

  mockFetch(t, async (input, options) => {
    const url = new URL(input);
    requests.push({ url, options });
    const page = Number(url.searchParams.get("page"));
    return jsonResponse({
      data: page === 1 ? firstPage : [lastTask],
      pagination: { total: 101, page, page_size: 100 },
    });
  });

  const loop = new Loop(config);
  const tasks = await loop.listOpenTasks("member+tag");

  assert.deepEqual(tasks, [...firstPage, lastTask]);
  assert.equal(requests.length, 2);
  for (const { url, options } of requests) {
    assert.equal(url.origin, "https://loop.example");
    assert.equal(url.pathname, "/fleet/api/v1/tasks");
    assert.equal(url.searchParams.get("assignee_octo_uid"), "member+tag");
    assert.equal(url.searchParams.get("open_only"), "true");
    assert.equal(url.searchParams.get("page_size"), "100");
    assertHeaders(options.headers);
  }
  assert.deepEqual(
    requests.map(({ url }) => url.searchParams.get("page")),
    ["1", "2"],
  );
});

test("task and comment methods return raw data and paginate comments", async (t) => {
  const task = { id: "task-1", title: "Raw task" };
  const comments = Array.from({ length: 100 }, (_, index) => ({
    id: `comment-${index + 1}`,
    content: `Comment ${index + 1}`,
  }));
  const lastComment = { id: "comment-101", content: "Comment 101" };
  const preview = { agents: [{ id: "expert-1", source: "mention_agent" }] };
  const createdComment = { id: "comment-new", content: "Created" };
  const requests = [];

  mockFetch(t, async (input, options) => {
    const url = new URL(input);
    requests.push({ url, options });

    if (options.method === "GET" && url.pathname === "/fleet/api/v1/tasks/task-1") {
      assertHeaders(options.headers);
      return jsonResponse({ data: task });
    }
    if (
      options.method === "GET" &&
      url.pathname === "/fleet/api/v1/tasks/task-1/comments"
    ) {
      assertHeaders(options.headers);
      const page = Number(url.searchParams.get("page"));
      return jsonResponse({
        data: page === 1 ? comments : [lastComment],
        pagination: { total: 101, page, page_size: 100 },
      });
    }
    if (
      options.method === "POST" &&
      url.pathname === "/fleet/api/v1/tasks/task-1/comments/_trigger_preview"
    ) {
      assertHeaders(options.headers, true);
      assert.deepEqual(JSON.parse(options.body), { content: "Preview me" });
      return jsonResponse({ data: preview });
    }
    if (
      options.method === "POST" &&
      url.pathname === "/fleet/api/v1/tasks/task-1/comments"
    ) {
      assertHeaders(options.headers, true);
      assert.deepEqual(JSON.parse(options.body), { content: "Create me" });
      return jsonResponse({ data: createdComment });
    }
    assert.fail(`Unexpected request: ${options.method} ${url}`);
  });

  const loop = new Loop(config);
  assert.deepEqual(await loop.getTask("task-1"), task);
  assert.deepEqual(await loop.listComments("task-1"), [...comments, lastComment]);
  assert.deepEqual(await loop.previewComment("task-1", "Preview me"), preview);
  assert.deepEqual(await loop.createComment("task-1", "Create me"), createdComment);

  assert.equal(requests.length, 5);
  assert.deepEqual(
    requests
      .filter(({ options }) => options.method === "GET")
      .map(({ url }) => url.searchParams.get("page")),
    [null, "1", "2"],
  );
});

test("transport errors propagate unchanged", async (t) => {
  const failure = new TypeError("connection refused");
  mockFetch(t, async () => {
    throw failure;
  });

  const loop = new Loop(config);
  await assert.rejects(loop.getTask("task-1"), (error) => error === failure);
});

test("HTTP errors retain their status and response body", async (t) => {
  const body = { error: { code: "FORBIDDEN", message: "Not allowed" } };
  mockFetch(t, async () => jsonResponse(body, 403, "Forbidden"));

  const loop = new Loop(config);
  await assert.rejects(loop.getTask("task-1"), (error) => {
    assert.equal(error.name, "LoopApiError");
    assert.equal(error.status, 403);
    assert.deepEqual(error.body, body);
    assert.match(error.message, /Not allowed/);
    return true;
  });
});
