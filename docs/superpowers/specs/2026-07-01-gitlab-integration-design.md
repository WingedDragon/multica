# GitLab Integration Design

Date: 2026-07-01
Status: approved design direction, pending implementation plan

## Context

Multica already has a GitHub integration that mirrors pull requests, check
suites, merge state, issue links, and auto-completion behavior. The new
requirement is not a general git or `glab` workflow. Multica workers can already
reach `code.mlamp.cn`; the missing capability is for the Multica server running
on `dj` to call the GitLab API at `https://code.mlamp.cn`.

`code.mlamp.cn` can call `dj`'s public Multica URL for webhook delivery. The
server cannot directly reach `code.mlamp.cn` from `dj`, but a reverse SOCKS
proxy exposed on `dj` by `my-mini` has been validated:

```text
Multica server on dj
  -> socks5h://127.0.0.1:18080
  -> my-mini
  -> https://code.mlamp.cn
```

## Goals

- Support a single deployment-level GitLab instance: `https://code.mlamp.cn`.
- Let the Multica server use a GitLab-specific proxy without changing global
  server outbound traffic.
- Mirror GitLab merge requests and pipeline state into Multica issues.
- Auto-link merge requests to Multica issues using existing issue identifiers.
- Auto-complete issues only when a merged MR explicitly declared closing intent.
- Keep the first version isolated from existing GitHub tables and handlers.

## Non-Goals

- Multiple GitLab hosts.
- GitLab OAuth.
- Group hooks or system hooks.
- GitLab Issue bidirectional sync.
- MR comments, discussions, approvals, jobs, logs, or artifacts.
- Refactoring GitHub and GitLab into a shared provider-neutral schema.
- Worker-side `git`, `ssh`, or `glab` proxy configuration.

## Recommended Approach

Use an independent server-side GitLab integration with its own `gitlab_*`
tables, handler, API client, and UI settings section.

This is deliberately not a generic `code_host` abstraction yet. GitHub and
GitLab do not share the same installation, webhook, CI, and mergeability
semantics. Keeping GitLab separate avoids risky migrations of stable GitHub
behavior and keeps rebases against upstream GitHub integration changes simpler.

## Deployment Configuration

The deployment config is server-level, not workspace-level:

```bash
GITLAB_BASE_URL=https://code.mlamp.cn
GITLAB_TOKEN=...
GITLAB_WEBHOOK_SECRET=...
GITLAB_PROXY_URL=socks5h://127.0.0.1:18080
```

`GITLAB_PROXY_URL` is consumed only by the GitLab HTTP client. The process-wide
`HTTP_PROXY`, `HTTPS_PROXY`, and `ALL_PROXY` environment variables must not be
required for this integration because they would affect unrelated outbound
traffic such as billing, storage, OpenAI-compatible services, or Composio.

Automatic webhook creation uses `MULTICA_PUBLIC_URL` to build:

```text
{MULTICA_PUBLIC_URL}/api/webhooks/gitlab
```

If `MULTICA_PUBLIC_URL` is missing, the UI can still present manual webhook
configuration instructions, but automatic hook registration is unavailable.

## Network Operations

`my-mini` should run a persistent reverse SOCKS tunnel to `dj`:

```bash
ssh -N \
  -o ExitOnForwardFailure=yes \
  -o ServerAliveInterval=30 \
  -o ServerAliveCountMax=3 \
  -R 127.0.0.1:18080 \
  dj
```

The tunnel should be supervised outside the Multica server process, for example
with `launchd` on `my-mini`. Multica should not try to start or manage this SSH
tunnel itself. It should only report clear connection errors when the configured
GitLab proxy is unavailable.

## Data Model

Add new migrations, for example `131_gitlab_integration.up.sql` and matching
down migration. Do not edit existing applied migrations.

### `gitlab_connection`

Workspace-level connection record for the deployment GitLab instance.

Key fields:

- `id uuid primary key`
- `workspace_id uuid not null references workspace(id) on delete cascade`
- `base_url text not null`
- `host text not null`
- `connected_by_id uuid references "user"(id) on delete set null`
- `created_at timestamptz not null default now()`
- `updated_at timestamptz not null default now()`

Token material is not stored in this table. The first version uses the
deployment-level `GITLAB_TOKEN`.

### `gitlab_project_binding`

Maps a workspace to GitLab projects whose webhooks and MRs should be accepted.

Key fields:

- `id uuid primary key`
- `workspace_id uuid not null references workspace(id) on delete cascade`
- `connection_id uuid not null references gitlab_connection(id) on delete cascade`
- `gitlab_project_id bigint not null`
- `path_with_namespace text not null`
- `web_url text not null`
- `hook_id bigint`
- `hook_enabled boolean not null default false`
- `last_sync_error text`
- `created_at timestamptz not null default now()`
- `updated_at timestamptz not null default now()`

Unique constraint:

- `(workspace_id, gitlab_project_id)`

### `gitlab_merge_request`

Mirrors the GitLab MR state needed by the issue detail sidebar.

Key fields:

- `id uuid primary key`
- `workspace_id uuid not null references workspace(id) on delete cascade`
- `project_binding_id uuid not null references gitlab_project_binding(id) on delete cascade`
- `gitlab_project_id bigint not null`
- `mr_iid integer not null`
- `title text not null`
- `description text`
- `state text not null`
- `web_url text not null`
- `source_branch text`
- `target_branch text`
- `author_username text`
- `author_avatar_url text`
- `sha text not null default ''`
- `merge_commit_sha text`
- `detailed_merge_status text`
- `has_conflicts boolean`
- `additions integer not null default 0`
- `deletions integer not null default 0`
- `changed_files integer not null default 0`
- `mr_created_at timestamptz not null`
- `mr_updated_at timestamptz not null`
- `merged_at timestamptz`
- `closed_at timestamptz`
- `created_at timestamptz not null default now()`
- `updated_at timestamptz not null default now()`

Unique constraint:

- `(workspace_id, gitlab_project_id, mr_iid)`

State is normalized for UI behavior:

- GitLab `opened` -> `open`
- GitLab `merged` -> `merged`
- GitLab `closed` -> `closed`
- draft MRs -> `draft`

### `gitlab_mr_pipeline`

Stores pipeline status associated with a mirrored MR's current head SHA.

Key fields:

- `merge_request_id uuid not null references gitlab_merge_request(id) on delete cascade`
- `pipeline_id bigint not null`
- `sha text not null`
- `ref text`
- `status text not null`
- `web_url text`
- `pipeline_updated_at timestamptz not null`
- `created_at timestamptz not null default now()`
- `updated_at timestamptz not null default now()`

Primary key:

- `(merge_request_id, pipeline_id)`

Query code should aggregate the latest pipeline rows for the MR's current SHA
into `passed`, `failed`, or `pending`.

### `issue_gitlab_merge_request`

Links Multica issues to GitLab MRs.

Key fields:

- `issue_id uuid not null references issue(id) on delete cascade`
- `merge_request_id uuid not null references gitlab_merge_request(id) on delete cascade`
- `close_intent boolean not null default false`
- `linked_by_type text`
- `linked_by_id uuid`
- `linked_at timestamptz not null default now()`

Primary key:

- `(issue_id, merge_request_id)`

`close_intent` has the same product meaning as the GitHub PR link table:
reference-only links must not auto-complete issues.

## Server Components

Add a focused package:

```text
server/internal/integrations/gitlab/
  client.go
  config.go
  types.go
```

Responsibilities:

- `config.go`: read and validate deployment env.
- `client.go`: GitLab REST client with optional `GITLAB_PROXY_URL` transport.
- `types.go`: typed Project, MR, Pipeline, and webhook payload DTOs.

Add handler code:

```text
server/internal/handler/gitlab.go
```

Responsibilities:

- Workspace GitLab settings endpoints.
- Project binding endpoints.
- Webhook ingress.
- MR upsert, issue identifier extraction, close-intent handling, and issue
  auto-completion.

Add sqlc queries:

```text
server/pkg/db/queries/gitlab.sql
```

Run `make sqlc` after adding the queries.

## API Design

Workspace APIs:

- `GET /api/workspaces/{id}/gitlab/config`
  - Returns configured status, current connection, and project bindings.
  - Does not expose token material.
- `POST /api/workspaces/{id}/gitlab/projects`
  - Body accepts a project URL or `path_with_namespace`.
  - Server resolves the project through GitLab API, creates a binding, and
    tries to create or update a project webhook.
- `DELETE /api/workspaces/{id}/gitlab/projects/{bindingId}`
  - Deletes the binding and best-effort removes or disables the GitLab webhook
    when `hook_id` is known.

Issue APIs:

The low-risk first version adds:

- `GET /api/issues/{id}/gitlab/merge-requests`

The UI can then merge GitHub PRs and GitLab MRs client-side. A later version can
introduce a unified:

- `GET /api/issues/{id}/merge-requests`

The unified endpoint should wait until both provider shapes are stable.

Webhook API:

- `POST /api/webhooks/gitlab`

This route is unauthenticated by Multica session auth and authenticated by the
GitLab webhook secret.

## Webhook Behavior

Validation:

- Require `X-Gitlab-Token`.
- Compare it with `GITLAB_WEBHOOK_SECRET`.
- Reject mismatches with `401`.

Accepted events:

- `X-Gitlab-Event: Merge Request Hook`
- `X-Gitlab-Event: Pipeline Hook`

Routing:

- Use payload `project.id` to find `gitlab_project_binding`.
- If no binding exists, ignore the event and log at info/debug level.
- Do not create workspace data from unbound projects.

Merge request event:

- Upsert `gitlab_merge_request`.
- Extract identifiers from title, description, and source branch.
- Link every matching issue in the workspace.
- Extract closing identifiers only from title and description.
- Do not treat branch names as closing intent.
- On terminal MR state, recalculate issue completion from persisted links:
  - no linked MR is still `open` or `draft`;
  - at least one merged linked MR has `close_intent=true`.
- If both are true, update the issue to `done` and publish the same style of
  realtime issue update used by existing system-side status transitions.

Pipeline event:

- Find mirrored MRs in the same project that match the pipeline SHA.
- Upsert `gitlab_mr_pipeline` rows.
- Publish a merge-request update event so open issue detail views refetch.

Out-of-order pipeline events:

- If no MR row exists for a pipeline event, ignore the event and log the project
  id plus SHA at info/debug level.
- Do not add a pending pipeline table in the first version.
- A later MR webhook or explicit API refresh is responsible for filling the
  state. This behavior must be explicit in tests.

## Project Binding and Webhook Registration

When a workspace admin binds a project:

1. Normalize input into `path_with_namespace`.
2. Call GitLab `GET /projects/:id` using the configured token and proxy.
3. Upsert `gitlab_connection` and `gitlab_project_binding`.
4. If `MULTICA_PUBLIC_URL` is set, call the Project Webhooks API to register:
   - `merge_requests_events=true`
   - `pipeline_events=true`
   - `token=GITLAB_WEBHOOK_SECRET`
5. If GitLab returns permission errors, keep the binding but mark
   `hook_enabled=false` and return manual setup instructions. The response does
   not include `GITLAB_WEBHOOK_SECRET`; it tells the operator to use the
   deployment-configured secret.

This matches GitLab's documented requirement that project webhook management
requires administrator, Maintainer, or Owner permissions.

## UI Design

Settings:

- Add a GitLab section under integrations.
- Show configured/not configured state based on server config.
- Allow workspace admins to add or remove project bindings.
- Show each project binding with:
  - path with namespace;
  - hook status;
  - last sync error;
  - manual webhook URL and guidance to use the deployment
    `GITLAB_WEBHOOK_SECRET` when automatic hook registration failed. The UI must
    not display the raw secret.

Issue detail:

- Extend the existing pull request list component to render provider-aware rows.
- GitHub rows keep their current behavior.
- GitLab rows show provider badge, MR number, title, state, pipeline status,
  conflict/mergeability signal, branch, author, and diff stats when available.

Frontend package boundaries:

- Shared types and query helpers live under `packages/core/gitlab`.
- Shared UI lives under `packages/views`.
- No Next.js API imports in shared views.

## Error Handling

GitLab API client:

- Connection timeout or proxy failure returns a clear "GitLab unreachable"
  error.
- `401` returns "GitLab token invalid".
- `403` returns "GitLab token lacks required project permission".
- Non-JSON upstream errors are captured as short raw snippets for logs but not
  exposed verbatim to end users.

Webhook:

- Bad token: `401`.
- Unsupported event: `204`.
- Unbound project: `204` plus log.
- Malformed payload: `400` with safe message.

Project binding:

- Invalid URL/path: `400`.
- Unknown project: `404`.
- Duplicate binding: idempotently return the existing binding or use `409` only
  if the request conflicts with existing normalized data.

## Security

- Do not store `GITLAB_TOKEN` in the database.
- Do not log `GITLAB_TOKEN` or `GITLAB_WEBHOOK_SECRET`.
- Redaction should rely on existing GitLab token redaction patterns where logs
  may include environment-like text.
- `GITLAB_PROXY_URL` is not a credential, but still should be logged sparingly.
- Webhook payloads should not create data for unbound projects.
- Automatic webhook registration should send only the configured public webhook
  URL and secret to GitLab.

## Realtime and Cache Invalidation

Add provider-specific events first, for example:

- `gitlab_project:created`
- `gitlab_project:deleted`
- `gitlab_merge_request:updated`

Frontend invalidation can mirror the current GitHub strategy:

- GitLab settings/project events invalidate GitLab project queries.
- MR update events invalidate GitLab MR queries for issue detail views.

A later provider-neutral `merge_request:*` event can be introduced after both
GitHub and GitLab shapes stabilize.

## Testing Plan

Backend unit/integration tests:

- GitLab client uses the proxy transport when `GITLAB_PROXY_URL` is set.
- GitLab client does not require process-wide `HTTPS_PROXY`.
- Project binding resolves URLs and paths.
- Permission errors return manual webhook fallback state.
- Webhook token validation accepts valid token and rejects invalid token.
- MR opened/update/merged/closed payloads upsert rows idempotently.
- Pipeline success/failed/running payloads map to passed/failed/pending.
- Unbound project webhook is ignored without dirty data.
- Issue identifier extraction links title, description, and branch references.
- Closing intent is accepted only from title and description.
- Multiple linked MRs complete an issue only when all are terminal and one
  merged close-intent MR exists.

Frontend tests:

- Settings GitLab tab renders configured, unconfigured, bound, and manual-hook
  fallback states.
- Issue MR list renders mixed GitHub and GitLab rows.
- GitLab MR status mapping covers merged, closed, conflict, pipeline failed,
  pipeline pending, pipeline passed, and unknown.

Verification commands:

```bash
make sqlc
go test ./internal/integrations/gitlab ./internal/handler
pnpm test --filter @multica/core -- --run
pnpm test --filter @multica/views -- --run
pnpm typecheck
```

Run broader `make test` when backend changes are complete and the local database
is available.

## Rollout Plan

1. Deploy the `my-mini -> dj` reverse SOCKS tunnel and verify:

   ```bash
   curl --socks5-hostname 127.0.0.1:18080 -I https://code.mlamp.cn
   ```

2. Configure Multica server env with `GITLAB_*`.
3. Deploy schema and server changes.
4. Bind one low-risk GitLab project in a test workspace.
5. Confirm automatic webhook creation or manual fallback.
6. Open and update a test MR that references a Multica issue.
7. Confirm MR card display, pipeline status display, and close-intent behavior.

## References

- GitLab Webhooks: https://docs.gitlab.com/user/project/integrations/webhooks/
- GitLab Webhook Events: https://docs.gitlab.com/user/project/integrations/webhook_events/
- GitLab Project Webhooks API: https://docs.gitlab.com/api/project_webhooks/
- GitLab Merge Requests API: https://docs.gitlab.com/api/merge_requests/
- GitLab Pipelines API: https://docs.gitlab.com/api/pipelines/
