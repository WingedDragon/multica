# GitLab Complete Integration Design

Date: 2026-07-06
Status: approved design direction, pending implementation plan

## Context

Multica already has a GitLab integration that can bind projects, register
project webhooks, mirror merge requests, mirror pipeline status, link merge
requests to Multica issues, and auto-complete issues when a merged MR declared
closing intent.

That implementation deliberately stayed narrow. It does not use a GitHub App
model and it does not need one: GitLab is driven by a deployment-level token,
base URL, webhook secret, and optional GitLab-specific proxy.

The goal now is to complete the GitLab integration with the capabilities the
GitLab token can provide but the current Multica GitLab path has not yet
implemented. The design must keep GitLab isolated from existing GitHub code as
much as possible so future rebases against upstream GitHub integration changes
remain low-conflict.

## Goals

- Keep the selected architecture: GitLab-only expansion, no provider-neutral
  refactor.
- Preserve existing GitHub behavior and avoid changing GitHub handler, query,
  schema, or settings code unless a small shared pure helper is clearly safer.
- Expand GitLab issue-side context to include:
  - full MR refresh from GitLab API;
  - real diff stats and richer MR metadata;
  - approvals and approval rules/state;
  - discussions, notes, and unresolved thread status;
  - pipelines and pipeline jobs;
  - failed job trace summaries;
  - artifact metadata and controlled artifact download/open actions;
  - manual and webhook-triggered refresh diagnostics.
- Keep GitLab token material server-side only.
- Keep webhook ingestion fast and resilient; use refresh/backfill to fill data
  that webhooks do not carry.
- Make implementation work packages independent without reducing final scope.

## Non-Goals

- GitLab OAuth or per-user GitLab tokens.
- GitHub App changes.
- Migrating GitHub PRs and GitLab MRs into a shared `code_host_*` schema.
- Replacing existing GitHub PR/check-suite behavior.
- Syncing GitLab Issues as a second issue tracker. This work is about completing
  the GitLab code-review/MR/CI integration around Multica issues; GitLab Issue
  bidirectional sync would be a separate product integration with different data
  ownership semantics.

## Design Principles

### Isolate GitLab

All new backend behavior should live under:

- `server/internal/integrations/gitlab/`
- `server/internal/handler/gitlab.go` or small GitLab-specific handler files if
  the existing file becomes too large
- `server/pkg/db/queries/gitlab.sql`
- new `gitlab_*` migrations and generated sqlc code
- `packages/core/gitlab/`
- GitLab-specific additions in existing mixed GitHub/GitLab UI surfaces

Do not introduce a generic provider abstraction in this change. It would touch
stable GitHub behavior and increase rebase risk.

### Cache Metadata, Not Large Blobs

The database should store review and CI metadata needed for the UI, plus short
failure summaries. It should not store full traces or artifact binaries.

Job trace reads and artifact downloads go through bounded, permission-checked
backend endpoints.

### Webhook First, Refresh Completes State

Webhook events update fast-changing rows and trigger invalidation. Refresh
paths use the GitLab REST API to backfill data that webhook payloads omit:
changes, approvals, discussions, jobs, trace summaries, and artifact metadata.

Manual refresh endpoints use the same service code as webhook-triggered refresh
so behavior is testable and idempotent.

## Data Model

Use the next available migration number at implementation time. The current tree
contains multiple historical duplicate migration numbers, so the implementer
must inspect `server/migrations/` before naming new files.

### Extend `gitlab_project_binding`

Add lightweight sync diagnostics:

- `last_refresh_at timestamptz`
- `last_refresh_error text`
- `last_event_at timestamptz`
- `last_event_type text`
- `refresh_in_progress_at timestamptz`

These fields power the settings diagnostics panel. They should be updated only
by GitLab refresh/webhook paths.

### Extend `gitlab_merge_request`

The table already has many MR fields. Fill existing zero/default fields from
the GitLab API and add only fields needed for richer UI:

- keep using existing `additions`, `deletions`, `changed_files`, but populate
  them from MR changes/metadata instead of webhook defaults;
- `reviewers jsonb not null default '[]'`
- `assignees jsonb not null default '[]'`
- `labels jsonb not null default '[]'`
- `last_refreshed_at timestamptz`
- `last_refresh_error text`

Use compact JSON arrays for people/labels to avoid premature normalization.
They are display data, not primary query dimensions.

### New `gitlab_mr_approval_state`

One row per mirrored MR:

- `merge_request_id uuid primary key references gitlab_merge_request(id) on delete cascade`
- `workspace_id uuid not null`
- `approved boolean not null default false`
- `approvals_required integer`
- `approvals_left integer`
- `approved_by jsonb not null default '[]'`
- `rules jsonb not null default '[]'`
- `raw_state jsonb`
- `fetched_at timestamptz not null default now()`
- `updated_at timestamptz not null default now()`

The UI reads `approved`, counts, approvers, and rule names. `raw_state` is a
small escape hatch for GitLab version drift and should not be exposed directly.

### New `gitlab_mr_discussion`

One row per GitLab discussion thread:

- `id uuid primary key`
- `workspace_id uuid not null`
- `merge_request_id uuid not null references gitlab_merge_request(id) on delete cascade`
- `gitlab_discussion_id text not null`
- `individual_note boolean not null default false`
- `resolved boolean`
- `created_at timestamptz`
- `updated_at timestamptz`
- `fetched_at timestamptz not null default now()`
- unique `(merge_request_id, gitlab_discussion_id)`

### New `gitlab_mr_note`

One row per GitLab note/comment inside a discussion:

- `id uuid primary key`
- `workspace_id uuid not null`
- `discussion_id uuid not null references gitlab_mr_discussion(id) on delete cascade`
- `merge_request_id uuid not null references gitlab_merge_request(id) on delete cascade`
- `gitlab_note_id bigint not null`
- `author_username text`
- `author_avatar_url text`
- `body text not null`
- `system boolean not null default false`
- `resolved boolean`
- `resolvable boolean`
- `created_at timestamptz`
- `updated_at timestamptz`
- unique `(merge_request_id, gitlab_note_id)`

The UI can show unresolved discussions without merging GitLab comments into
Multica issue comments. These rows must not trigger agent comment workflows.

### New `gitlab_pipeline_job`

One row per GitLab job associated with a mirrored MR pipeline:

- `id uuid primary key`
- `workspace_id uuid not null`
- `merge_request_id uuid not null references gitlab_merge_request(id) on delete cascade`
- `pipeline_id bigint not null`
- `job_id bigint not null`
- `name text not null`
- `stage text`
- `status text not null`
- `ref text`
- `sha text`
- `web_url text`
- `started_at timestamptz`
- `finished_at timestamptz`
- `duration_seconds double precision`
- `queued_duration_seconds double precision`
- `failure_reason text`
- `allow_failure boolean not null default false`
- `artifacts_file_name text`
- `artifacts_file_size bigint`
- `artifacts_expire_at timestamptz`
- `trace_summary text`
- `trace_truncated boolean not null default false`
- `trace_fetched_at timestamptz`
- `fetched_at timestamptz not null default now()`
- unique `(merge_request_id, job_id)`

Only failed or selected diagnostic jobs should fetch trace summaries by default.
The trace summary is a bounded tail/excerpt, not the full trace.

## Backend Components

### GitLab Client

Extend `server/internal/integrations/gitlab/client.go` with typed methods:

- `GetMergeRequest(projectID, iid)`
- `GetMergeRequestChanges(projectID, iid)`
- `GetMergeRequestApprovalState(projectID, iid)`
- `ListMergeRequestDiscussions(projectID, iid)`
- `ListProjectPipelines(projectID, filters)`
- `GetPipeline(projectID, pipelineID)`
- `ListPipelineJobs(projectID, pipelineID)`
- `GetJobTrace(projectID, jobID, limit)`
- `DownloadJobArtifacts(projectID, jobID)`

The client keeps the existing GitLab-specific proxy behavior. Pagination must be
handled in the client or a small helper with:

- max pages;
- context deadline;
- clear error on malformed pagination headers;
- tests for multi-page and page-limit behavior.

### GitLab Sync Service

Add a small GitLab-specific sync service instead of embedding all refresh logic
directly in HTTP handlers.

Responsibilities:

- resolve binding and MR rows;
- call GitLab client methods;
- upsert MR metadata, approvals, discussions, notes, jobs;
- fetch bounded failed-job trace summaries;
- update binding/MR refresh diagnostics;
- publish existing GitLab realtime invalidation events.

Handlers and webhooks should call this service. This is not a generic code host
service.

### Webhook Behavior

Keep current webhook behavior for:

- `Merge Request Hook`
- `Pipeline Hook`

Add support for additional GitLab project webhook event types when useful:

- `Job Hook` to update individual job status and optionally fetch trace summary
  after failure;
- `Note Hook` to refresh discussions/notes for the affected MR.

Unsupported events still return `204`. Malformed supported payloads return
`400`. Webhook auth stays `X-Gitlab-Token` against `GITLAB_WEBHOOK_SECRET`.

Webhook handlers should keep doing minimal trusted upserts before calling the
sync service. If a refresh fails, persist the webhook-derived row and record the
refresh error rather than rolling back the event.

## API Design

Existing endpoints remain:

- `GET /api/workspaces/{id}/gitlab/config`
- `POST /api/workspaces/{id}/gitlab/projects`
- `DELETE /api/workspaces/{id}/gitlab/projects/{bindingId}`
- `GET /api/issues/{id}/gitlab/merge-requests`
- `POST /api/webhooks/gitlab`

Add GitLab-only endpoints:

### `POST /api/workspaces/{id}/gitlab/projects/{bindingId}/refresh`

Admin/owner only.

Refreshes recent GitLab data for the bound project. This includes known MRs and
recent pipelines/jobs for MRs Multica already mirrors. It does not import every
MR in a large project without an issue link unless a bounded backfill window is
explicitly provided.

Response includes refresh summary:

- updated MR count;
- updated job count;
- updated discussion count;
- errors, if partial.

### `POST /api/issues/{id}/gitlab/merge-requests/{mrId}/refresh`

Workspace member with issue access.

Refreshes one MR and returns the updated detail response. This uses Multica's
MR row id, not a raw GitLab iid, so the handler can verify the MR is linked to
the issue.

### `GET /api/issues/{id}/gitlab/merge-requests/{mrId}/details`

Workspace member with issue access.

Returns:

- MR core fields;
- approval state;
- discussions/notes;
- pipelines and jobs;
- artifact metadata;
- refresh diagnostics.

### `GET /api/issues/{id}/gitlab/jobs/{jobId}/trace`

Workspace member with issue access.

Returns a bounded trace summary for a job associated with a GitLab MR linked to
the issue. The backend must enforce:

- issue membership;
- job belongs to an MR linked to that issue;
- max bytes;
- timeout;
- secret redaction.

### `GET /api/issues/{id}/gitlab/jobs/{jobId}/artifacts`

Workspace member with issue access.

Streams or redirects a GitLab job artifact after the same issue/MR/job
relationship checks. The endpoint should not expose `GITLAB_TOKEN`.

## Frontend Design

### Settings GitLab Tab

Keep the current project-binding UI. Expand each bound project row with:

- webhook enabled/manual state;
- last event time and event type;
- last refresh time;
- last refresh error;
- manual refresh button for admins/owners;
- token/API health derived from refresh errors.

Do not add a GitLab master switch unless product explicitly requests a separate
pause-control change. In this design, a bound project means GitLab integration
is enabled for that project.

### Issue Sidebar

Keep the existing mixed GitHub PR / GitLab MR list. Expand GitLab MR rows into
a provider-specific detail view:

- MR state, mergeability/conflict signal, branch, author, reviewers, labels;
- diff stats;
- approval summary;
- pipeline summary;
- failed jobs first;
- trace summary for failed jobs;
- artifact metadata and action;
- unresolved discussions first, with option to show all discussions.

GitLab discussions are displayed as GitLab review context, not as Multica issue
comments. They must not call comment-trigger preview logic and must not enqueue
agents.

### Query Keys and Invalidation

Add query keys under `packages/core/gitlab`:

- `gitlabKeys.config(wsId)`
- `gitlabKeys.mergeRequests(issueId)`
- `gitlabKeys.mergeRequestDetails(issueId, mrId)`
- `gitlabKeys.jobTrace(issueId, jobId)`

Existing `gitlab_merge_request:updated` can invalidate MR list and detail
queries. Add more events only if targeted invalidation is needed; avoid changing
the GitHub event model.

## Permissions and Security

- GitLab token and webhook secret remain server env only.
- Any issue-level GitLab details endpoint must first load the Multica issue via
  existing user access checks.
- Raw GitLab project id, MR iid, pipeline id, and job id are never sufficient
  for access. The server must prove the row belongs to a binding in the issue's
  workspace and that the MR is linked to the issue.
- Trace summaries and GitLab API errors must be redacted before persistence and
  response.
- Artifact access must not leak the private token in URLs or response bodies.
- Do not store artifact binaries in the Multica database.

## Error Handling

GitLab client errors should map to stable categories:

- token invalid;
- token lacks permission;
- project/MR/job not found;
- GitLab unreachable;
- rate limited;
- upstream server error;
- response too large;
- trace/artifact limit exceeded;
- pagination limit exceeded.

Refresh operations are partial-success operations. A failed jobs refresh should
not discard updated MR metadata; the response and diagnostics should show what
failed.

Webhook handling should acknowledge supported events after local payload
validation even when the follow-up refresh fails. The refresh error is recorded
on the binding or MR for operators to inspect.

## Testing Plan

### Backend Client Tests

- project path/id encoding still works;
- MR changes parsing fills additions/deletions/changed files;
- approval state parsing handles required/left/approved users;
- discussions pagination and note mapping;
- pipeline jobs pagination;
- job trace is truncated and redacted;
- artifact endpoint handles GitLab redirect/stream behavior without leaking
  token;
- error mapping for `401`, `403`, `404`, `429`, timeout, malformed JSON, and
  non-JSON snippets.

### Backend Handler Tests

- project refresh updates binding diagnostics;
- MR refresh updates core MR fields, approval, discussions, jobs;
- webhook MR event persists minimal MR even when refresh fails;
- webhook pipeline/job events update jobs and invalidate issue detail;
- note hook refreshes discussions without creating Multica comments;
- issue/MR/job relationship guards prevent cross-issue or cross-workspace trace
  and artifact access;
- partial refresh preserves existing cache and records error;
- manual refresh requires admin/owner where appropriate.

### Frontend Tests

- GitLab settings tab renders refresh state, errors, and admin-only refresh;
- GitLab MR details render approvals, jobs, trace summary, artifacts, and
  unresolved discussions;
- trace/artifact actions call the new GitLab endpoints;
- GitLab discussions do not appear in Multica issue comment lists;
- mixed GitHub/GitLab PR list behavior remains unchanged for GitHub rows.

### Regression Checks

Narrow checks while implementing:

```bash
go test ./internal/integrations/gitlab ./internal/handler
pnpm --filter @multica/core test -- api/schemas.test.ts --run
pnpm --filter @multica/views test -- pull-request-list.test.tsx gitlab-tab.test.tsx --run
pnpm typecheck
```

Run broader checks if shared issue detail or API schema code changes more than
expected.

## Implementation Work Packages

These packages are sequencing units, not scope cuts:

1. Schema and sqlc for approvals, discussions, jobs, and sync diagnostics.
2. GitLab client expansion and tests.
3. Sync service and manual refresh endpoints.
4. Webhook event expansion for job and note events.
5. Issue MR details, trace, and artifact endpoints.
6. Frontend core schemas/query keys/API methods.
7. Settings diagnostics UI.
8. Issue sidebar GitLab detail UI.
9. End-to-end smoke procedure against a low-risk GitLab project.

Each package should commit only its own files. Avoid `git add .`.

## References

- GitLab Merge Requests API: https://docs.gitlab.com/api/merge_requests/
- GitLab Merge Request Approvals API: https://docs.gitlab.com/api/merge_request_approvals/
- GitLab Discussions API: https://docs.gitlab.com/api/discussions/
- GitLab Pipelines API: https://docs.gitlab.com/api/pipelines/
- GitLab Jobs API: https://docs.gitlab.com/api/jobs/
- GitLab Job Artifacts API: https://docs.gitlab.com/api/job_artifacts/
- GitLab Webhook Events: https://docs.gitlab.com/user/project/integrations/webhook_events/
