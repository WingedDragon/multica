# GitLab Integration Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add a deployment-level GitLab integration for `https://code.mlamp.cn` so the Multica server can mirror GitLab merge requests and pipelines into issues through a GitLab-specific proxy.

**Architecture:** Keep GitLab isolated from the existing GitHub integration: independent `gitlab_*` tables, sqlc queries, server integration client, handler routes, core package, and view components. Reuse the established GitHub product behavior where it is already proven: issue identifier extraction, close-intent aggregation, issue auto-completion, websocket invalidation, and settings permission split.

**Tech Stack:** Go, Chi, sqlc, pgx, PostgreSQL migrations, `golang.org/x/net/proxy` for SOCKS dialing, React Query, TypeScript, Vitest, React Testing Library, shadcn/Base UI.

---

## Scope Check

This plan covers one subsystem: GitLab MR/pipeline mirroring into Multica issues. It intentionally does not add GitLab OAuth, multi-host support, GitLab Issue sync, comments/discussions, jobs/logs/artifacts, or worker-side `git`/`glab` proxy setup.

## File Structure

- Create `server/migrations/131_gitlab_integration.up.sql`: GitLab connection, project binding, MR, pipeline, and issue link schema.
- Create `server/migrations/131_gitlab_integration.down.sql`: reverse the new schema.
- Create `server/pkg/db/queries/gitlab.sql`: sqlc query surface for the new schema.
- Create `server/internal/integrations/gitlab/config.go`: environment config and safe validation.
- Create `server/internal/integrations/gitlab/client.go`: GitLab REST client with optional `socks5h://` transport.
- Create `server/internal/integrations/gitlab/types.go`: project, hook, MR, pipeline, and webhook DTOs.
- Create `server/internal/integrations/gitlab/client_test.go`: client config, proxy, path encoding, and error mapping tests.
- Create `server/internal/handler/gitlab.go`: settings APIs, project binding APIs, webhook ingress, MR/pipeline upsert, linking, and completion logic.
- Create `server/internal/handler/gitlab_test.go`: webhook, binding, route permission, and issue completion tests.
- Modify `server/cmd/server/router.go`: mount GitLab public webhook and workspace routes.
- Modify `server/pkg/protocol/events.go`: add provider-specific GitLab websocket events.
- Create `packages/core/types/gitlab.ts`: frontend GitLab response and MR row types.
- Create `packages/core/gitlab/queries.ts`: React Query keys/options.
- Create `packages/core/gitlab/index.ts`: package barrel.
- Modify `packages/core/types/index.ts`: export GitLab types.
- Modify `packages/core/api/client.ts`: add GitLab API methods.
- Modify `packages/core/types/events.ts`: add GitLab websocket event names.
- Modify `packages/core/realtime/use-realtime-sync.ts`: invalidate GitLab settings/MR queries.
- Create `packages/views/settings/components/gitlab-mark.tsx`: compact GitLab brand mark.
- Create `packages/views/settings/components/gitlab-tab.tsx`: settings UI for project bindings and manual hook fallback.
- Create `packages/views/settings/components/gitlab-tab.test.tsx`: settings UI coverage.
- Modify `packages/views/settings/components/settings-page.tsx`: add GitLab tab.
- Modify locale files `packages/views/locales/{en,zh-Hans,ja,ko}/settings.json`: add GitLab settings strings.
- Modify `packages/views/issues/components/pull-request-list.tsx`: render GitHub PR and GitLab MR rows in one sidebar.
- Modify `packages/views/issues/components/pull-request-list.test.tsx`: cover mixed provider rows.
- Modify `packages/views/locales/{en,zh-Hans,ja,ko}/issues.json`: add GitLab MR labels where needed.

## Task 1: Database Schema and sqlc Queries

**Files:**
- Create: `server/migrations/131_gitlab_integration.up.sql`
- Create: `server/migrations/131_gitlab_integration.down.sql`
- Create: `server/pkg/db/queries/gitlab.sql`
- Generated: `server/pkg/db/generated/*.go`

- [ ] **Step 1: Add the migration**

Write `server/migrations/131_gitlab_integration.up.sql`:

```sql
CREATE TABLE gitlab_connection (
    id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    workspace_id    UUID NOT NULL REFERENCES workspace(id) ON DELETE CASCADE,
    base_url        TEXT NOT NULL,
    host            TEXT NOT NULL,
    connected_by_id UUID REFERENCES "user"(id) ON DELETE SET NULL,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (workspace_id, host)
);

CREATE INDEX idx_gitlab_connection_workspace
    ON gitlab_connection(workspace_id);

CREATE TABLE gitlab_project_binding (
    id                  UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    workspace_id        UUID NOT NULL REFERENCES workspace(id) ON DELETE CASCADE,
    connection_id       UUID NOT NULL REFERENCES gitlab_connection(id) ON DELETE CASCADE,
    gitlab_project_id   BIGINT NOT NULL,
    path_with_namespace TEXT NOT NULL,
    web_url             TEXT NOT NULL,
    hook_id             BIGINT,
    hook_enabled        BOOLEAN NOT NULL DEFAULT FALSE,
    last_sync_error     TEXT,
    created_at          TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at          TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (workspace_id, gitlab_project_id)
);

CREATE INDEX idx_gitlab_project_binding_workspace
    ON gitlab_project_binding(workspace_id);
CREATE INDEX idx_gitlab_project_binding_project
    ON gitlab_project_binding(gitlab_project_id);

CREATE TABLE gitlab_merge_request (
    id                    UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    workspace_id          UUID NOT NULL REFERENCES workspace(id) ON DELETE CASCADE,
    project_binding_id    UUID NOT NULL REFERENCES gitlab_project_binding(id) ON DELETE CASCADE,
    gitlab_project_id     BIGINT NOT NULL,
    mr_iid                INTEGER NOT NULL,
    title                 TEXT NOT NULL,
    description           TEXT,
    state                 TEXT NOT NULL CHECK (state IN ('open', 'closed', 'merged', 'draft')),
    web_url               TEXT NOT NULL,
    source_branch         TEXT,
    target_branch         TEXT,
    author_username       TEXT,
    author_avatar_url     TEXT,
    sha                   TEXT NOT NULL DEFAULT '',
    merge_commit_sha      TEXT,
    detailed_merge_status TEXT,
    has_conflicts         BOOLEAN,
    additions             INTEGER NOT NULL DEFAULT 0,
    deletions             INTEGER NOT NULL DEFAULT 0,
    changed_files         INTEGER NOT NULL DEFAULT 0,
    mr_created_at         TIMESTAMPTZ NOT NULL,
    mr_updated_at         TIMESTAMPTZ NOT NULL,
    merged_at             TIMESTAMPTZ,
    closed_at             TIMESTAMPTZ,
    created_at            TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at            TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (workspace_id, gitlab_project_id, mr_iid)
);

CREATE INDEX idx_gitlab_merge_request_workspace
    ON gitlab_merge_request(workspace_id);
CREATE INDEX idx_gitlab_merge_request_project_sha
    ON gitlab_merge_request(workspace_id, gitlab_project_id, sha);

CREATE TABLE gitlab_mr_pipeline (
    merge_request_id    UUID NOT NULL REFERENCES gitlab_merge_request(id) ON DELETE CASCADE,
    pipeline_id         BIGINT NOT NULL,
    sha                 TEXT NOT NULL,
    ref                 TEXT,
    status              TEXT NOT NULL,
    web_url             TEXT,
    pipeline_updated_at TIMESTAMPTZ NOT NULL,
    created_at          TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at          TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (merge_request_id, pipeline_id)
);

CREATE INDEX idx_gitlab_mr_pipeline_current_sha
    ON gitlab_mr_pipeline(merge_request_id, sha, pipeline_updated_at DESC);

CREATE TABLE issue_gitlab_merge_request (
    issue_id           UUID NOT NULL REFERENCES issue(id) ON DELETE CASCADE,
    merge_request_id   UUID NOT NULL REFERENCES gitlab_merge_request(id) ON DELETE CASCADE,
    close_intent       BOOLEAN NOT NULL DEFAULT FALSE,
    linked_by_type     TEXT,
    linked_by_id       UUID,
    linked_at          TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (issue_id, merge_request_id)
);

CREATE INDEX idx_issue_gitlab_merge_request_mr
    ON issue_gitlab_merge_request(merge_request_id);
```

Write `server/migrations/131_gitlab_integration.down.sql`:

```sql
DROP TABLE IF EXISTS issue_gitlab_merge_request;
DROP TABLE IF EXISTS gitlab_mr_pipeline;
DROP TABLE IF EXISTS gitlab_merge_request;
DROP TABLE IF EXISTS gitlab_project_binding;
DROP TABLE IF EXISTS gitlab_connection;
```

- [ ] **Step 2: Add sqlc queries**

Write `server/pkg/db/queries/gitlab.sql` with these query groups:

```sql
-- name: GetGitLabConnectionByWorkspace :one
SELECT * FROM gitlab_connection
WHERE workspace_id = $1
ORDER BY created_at ASC
LIMIT 1;

-- name: UpsertGitLabConnection :one
INSERT INTO gitlab_connection (
    workspace_id, base_url, host, connected_by_id
) VALUES (
    $1, $2, $3, sqlc.narg('connected_by_id')
)
ON CONFLICT (workspace_id, host) DO UPDATE SET
    base_url = EXCLUDED.base_url,
    connected_by_id = COALESCE(EXCLUDED.connected_by_id, gitlab_connection.connected_by_id),
    updated_at = now()
RETURNING *;

-- name: ListGitLabProjectBindingsByWorkspace :many
SELECT * FROM gitlab_project_binding
WHERE workspace_id = $1
ORDER BY path_with_namespace ASC;

-- name: GetGitLabProjectBinding :one
SELECT * FROM gitlab_project_binding
WHERE id = $1 AND workspace_id = $2;

-- name: GetGitLabProjectBindingByProjectID :one
SELECT * FROM gitlab_project_binding
WHERE gitlab_project_id = $1
ORDER BY created_at ASC
LIMIT 1;

-- name: UpsertGitLabProjectBinding :one
INSERT INTO gitlab_project_binding (
    workspace_id, connection_id, gitlab_project_id, path_with_namespace,
    web_url, hook_id, hook_enabled, last_sync_error
) VALUES (
    $1, $2, $3, $4, $5,
    sqlc.narg('hook_id'),
    $6,
    sqlc.narg('last_sync_error')
)
ON CONFLICT (workspace_id, gitlab_project_id) DO UPDATE SET
    connection_id = EXCLUDED.connection_id,
    path_with_namespace = EXCLUDED.path_with_namespace,
    web_url = EXCLUDED.web_url,
    hook_id = COALESCE(EXCLUDED.hook_id, gitlab_project_binding.hook_id),
    hook_enabled = EXCLUDED.hook_enabled,
    last_sync_error = EXCLUDED.last_sync_error,
    updated_at = now()
RETURNING *;

-- name: DeleteGitLabProjectBinding :one
DELETE FROM gitlab_project_binding
WHERE id = $1 AND workspace_id = $2
RETURNING *;

-- name: UpsertGitLabMergeRequest :one
INSERT INTO gitlab_merge_request (
    workspace_id, project_binding_id, gitlab_project_id, mr_iid,
    title, description, state, web_url, source_branch, target_branch,
    author_username, author_avatar_url, sha, merge_commit_sha,
    detailed_merge_status, has_conflicts, additions, deletions, changed_files,
    mr_created_at, mr_updated_at, merged_at, closed_at
) VALUES (
    $1, $2, $3, $4,
    $5, sqlc.narg('description'), $6, $7,
    sqlc.narg('source_branch'), sqlc.narg('target_branch'),
    sqlc.narg('author_username'), sqlc.narg('author_avatar_url'),
    $8, sqlc.narg('merge_commit_sha'),
    sqlc.narg('detailed_merge_status'), sqlc.narg('has_conflicts'),
    $9, $10, $11, $12, $13,
    sqlc.narg('merged_at'), sqlc.narg('closed_at')
)
ON CONFLICT (workspace_id, gitlab_project_id, mr_iid) DO UPDATE SET
    project_binding_id = EXCLUDED.project_binding_id,
    title = EXCLUDED.title,
    description = EXCLUDED.description,
    state = EXCLUDED.state,
    web_url = EXCLUDED.web_url,
    source_branch = EXCLUDED.source_branch,
    target_branch = EXCLUDED.target_branch,
    author_username = EXCLUDED.author_username,
    author_avatar_url = EXCLUDED.author_avatar_url,
    sha = EXCLUDED.sha,
    merge_commit_sha = EXCLUDED.merge_commit_sha,
    detailed_merge_status = EXCLUDED.detailed_merge_status,
    has_conflicts = EXCLUDED.has_conflicts,
    additions = EXCLUDED.additions,
    deletions = EXCLUDED.deletions,
    changed_files = EXCLUDED.changed_files,
    mr_updated_at = EXCLUDED.mr_updated_at,
    merged_at = EXCLUDED.merged_at,
    closed_at = EXCLUDED.closed_at,
    updated_at = now()
RETURNING *;

-- name: GetGitLabMergeRequest :one
SELECT * FROM gitlab_merge_request
WHERE workspace_id = $1 AND gitlab_project_id = $2 AND mr_iid = $3;

-- name: ListGitLabMergeRequestsByIssue :many
WITH issue_mrs AS (
    SELECT mr.id, mr.sha
    FROM gitlab_merge_request mr
    JOIN issue_gitlab_merge_request imr ON imr.merge_request_id = mr.id
    WHERE imr.issue_id = sqlc.arg('issue_id')
),
latest_pipeline AS (
    SELECT DISTINCT ON (p.merge_request_id)
        p.merge_request_id, p.status, p.web_url
    FROM gitlab_mr_pipeline p
    JOIN issue_mrs im ON im.id = p.merge_request_id
    WHERE p.sha = im.sha AND im.sha <> ''
    ORDER BY p.merge_request_id, p.pipeline_updated_at DESC, p.pipeline_id DESC
)
SELECT
    mr.*,
    pb.path_with_namespace,
    lp.status AS pipeline_status,
    lp.web_url AS pipeline_url
FROM gitlab_merge_request mr
JOIN issue_gitlab_merge_request imr ON imr.merge_request_id = mr.id
JOIN gitlab_project_binding pb ON pb.id = mr.project_binding_id
LEFT JOIN latest_pipeline lp ON lp.merge_request_id = mr.id
WHERE imr.issue_id = sqlc.arg('issue_id')
ORDER BY mr.mr_created_at DESC;

-- name: ListGitLabMergeRequestsByProjectSHA :many
SELECT * FROM gitlab_merge_request
WHERE workspace_id = $1 AND gitlab_project_id = $2 AND sha = $3;

-- name: UpsertGitLabMRPipeline :exec
INSERT INTO gitlab_mr_pipeline (
    merge_request_id, pipeline_id, sha, ref, status, web_url, pipeline_updated_at
) VALUES (
    $1, $2, $3, sqlc.narg('ref'), $4, sqlc.narg('web_url'), $5
)
ON CONFLICT (merge_request_id, pipeline_id) DO UPDATE SET
    sha = EXCLUDED.sha,
    ref = EXCLUDED.ref,
    status = EXCLUDED.status,
    web_url = EXCLUDED.web_url,
    pipeline_updated_at = EXCLUDED.pipeline_updated_at,
    updated_at = now()
WHERE EXCLUDED.pipeline_updated_at >= gitlab_mr_pipeline.pipeline_updated_at;

-- name: LinkIssueToGitLabMergeRequest :exec
INSERT INTO issue_gitlab_merge_request (
    issue_id, merge_request_id, linked_by_type, linked_by_id, close_intent
) VALUES (
    $1, $2, sqlc.narg('linked_by_type'), sqlc.narg('linked_by_id'), $3
)
ON CONFLICT (issue_id, merge_request_id) DO UPDATE SET
    close_intent = CASE
        WHEN sqlc.arg('preserve_close_intent') THEN issue_gitlab_merge_request.close_intent
        ELSE EXCLUDED.close_intent
    END;

-- name: ListIssueIDsForGitLabMergeRequest :many
SELECT issue_id FROM issue_gitlab_merge_request
WHERE merge_request_id = $1;

-- name: GetIssueGitLabMRCloseAggregate :one
SELECT
    COALESCE(SUM(CASE WHEN mr.state IN ('open', 'draft') THEN 1 ELSE 0 END), 0)::bigint AS open_count,
    COALESCE(SUM(CASE WHEN mr.state = 'merged' AND imr.close_intent THEN 1 ELSE 0 END), 0)::bigint AS merged_with_close_intent_count
FROM gitlab_merge_request mr
JOIN issue_gitlab_merge_request imr ON imr.merge_request_id = mr.id
WHERE imr.issue_id = $1;
```

- [ ] **Step 3: Generate DB code and verify**

Run:

```bash
make sqlc
go test ./pkg/db/generated
```

Expected:

```text
make sqlc exits 0
go test ./pkg/db/generated exits 0 or reports no test files
```

- [ ] **Step 4: Commit schema and generated code**

```bash
git add server/migrations/131_gitlab_integration.up.sql server/migrations/131_gitlab_integration.down.sql server/pkg/db/queries/gitlab.sql server/pkg/db/generated
git commit -m "feat: add gitlab integration schema"
```

## Task 2: GitLab Config and REST Client

**Files:**
- Create: `server/internal/integrations/gitlab/config.go`
- Create: `server/internal/integrations/gitlab/client.go`
- Create: `server/internal/integrations/gitlab/types.go`
- Create: `server/internal/integrations/gitlab/client_test.go`
- Modify: `server/go.mod`
- Modify: `server/go.sum`

- [ ] **Step 1: Write failing client tests**

Create `server/internal/integrations/gitlab/client_test.go`:

```go
package gitlab

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestLoadConfigFromEnv(t *testing.T) {
	t.Setenv("GITLAB_BASE_URL", "https://code.mlamp.cn/")
	t.Setenv("GITLAB_TOKEN", "secret-token")
	t.Setenv("GITLAB_WEBHOOK_SECRET", "hook-secret")
	t.Setenv("GITLAB_PROXY_URL", "socks5h://127.0.0.1:18080")

	cfg := LoadConfigFromEnv()
	if cfg.BaseURL != "https://code.mlamp.cn" {
		t.Fatalf("BaseURL = %q", cfg.BaseURL)
	}
	if !cfg.Configured() {
		t.Fatal("expected configured")
	}
	if cfg.ProxyURL != "socks5h://127.0.0.1:18080" {
		t.Fatalf("ProxyURL = %q", cfg.ProxyURL)
	}
}

func TestNewClientRejectsUnsupportedProxyScheme(t *testing.T) {
	_, err := NewClient(Config{
		BaseURL: "https://code.mlamp.cn",
		Token:   "token",
		ProxyURL: "ftp://127.0.0.1:18080",
	})
	if err == nil || !strings.Contains(err.Error(), "unsupported gitlab proxy scheme") {
		t.Fatalf("error = %v", err)
	}
}

func TestGetProjectEncodesPathWithNamespace(t *testing.T) {
	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.EscapedPath()
		if got := r.Header.Get("PRIVATE-TOKEN"); got != "token" {
			t.Fatalf("PRIVATE-TOKEN = %q", got)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"id":42,"path_with_namespace":"group/repo","web_url":"https://code.mlamp.cn/group/repo"}`))
	}))
	defer srv.Close()

	c, err := NewClient(Config{BaseURL: srv.URL, Token: "token"})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	project, err := c.GetProject(context.Background(), "group/repo")
	if err != nil {
		t.Fatalf("GetProject: %v", err)
	}
	if gotPath != "/api/v4/projects/group%2Frepo" {
		t.Fatalf("path = %q", gotPath)
	}
	if project.ID != 42 || project.PathWithNamespace != "group/repo" {
		t.Fatalf("project = %+v", project)
	}
}
```

Run:

```bash
go test ./internal/integrations/gitlab
```

Expected failure:

```text
package gitlab is missing
```

- [ ] **Step 2: Add config and DTOs**

Create `server/internal/integrations/gitlab/config.go`:

```go
package gitlab

import (
	"net/url"
	"os"
	"strings"
)

type Config struct {
	BaseURL       string
	Host          string
	Token         string
	WebhookSecret string
	ProxyURL      string
}

func LoadConfigFromEnv() Config {
	base := strings.TrimRight(strings.TrimSpace(os.Getenv("GITLAB_BASE_URL")), "/")
	host := ""
	if u, err := url.Parse(base); err == nil {
		host = strings.ToLower(u.Host)
	}
	return Config{
		BaseURL:       base,
		Host:          host,
		Token:         strings.TrimSpace(os.Getenv("GITLAB_TOKEN")),
		WebhookSecret: strings.TrimSpace(os.Getenv("GITLAB_WEBHOOK_SECRET")),
		ProxyURL:      strings.TrimSpace(os.Getenv("GITLAB_PROXY_URL")),
	}
}

func (c Config) Configured() bool {
	return c.BaseURL != "" && c.Token != "" && c.WebhookSecret != ""
}
```

Create `server/internal/integrations/gitlab/types.go`:

```go
package gitlab

type Project struct {
	ID                int64  `json:"id"`
	PathWithNamespace string `json:"path_with_namespace"`
	WebURL            string `json:"web_url"`
}

type ProjectHook struct {
	ID int64 `json:"id"`
}

type MergeRequest struct {
	IID                 int32  `json:"iid"`
	Title               string `json:"title"`
	Description         string `json:"description"`
	State               string `json:"state"`
	Draft               bool   `json:"draft"`
	WebURL              string `json:"web_url"`
	SourceBranch        string `json:"source_branch"`
	TargetBranch        string `json:"target_branch"`
	SHA                 string `json:"sha"`
	MergeCommitSHA      string `json:"merge_commit_sha"`
	DetailedMergeStatus string `json:"detailed_merge_status"`
	HasConflicts        bool   `json:"has_conflicts"`
	ChangesCount        string `json:"changes_count"`
	CreatedAt           string `json:"created_at"`
	UpdatedAt           string `json:"updated_at"`
	MergedAt            string `json:"merged_at"`
	ClosedAt            string `json:"closed_at"`
	Author              struct {
		Username  string `json:"username"`
		AvatarURL string `json:"avatar_url"`
	} `json:"author"`
}

type Pipeline struct {
	ID        int64  `json:"id"`
	SHA       string `json:"sha"`
	Ref       string `json:"ref"`
	Status    string `json:"status"`
	WebURL    string `json:"web_url"`
	UpdatedAt string `json:"updated_at"`
}
```

- [ ] **Step 3: Add the REST client**

Create `server/internal/integrations/gitlab/client.go`:

```go
package gitlab

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	xproxy "golang.org/x/net/proxy"
)

type Client struct {
	cfg        Config
	httpClient *http.Client
}

func NewClient(cfg Config) (*Client, error) {
	if cfg.BaseURL == "" {
		return nil, errors.New("gitlab base url is required")
	}
	tr := &http.Transport{Proxy: nil}
	if cfg.ProxyURL != "" {
		u, err := url.Parse(cfg.ProxyURL)
		if err != nil {
			return nil, fmt.Errorf("parse gitlab proxy url: %w", err)
		}
		switch u.Scheme {
		case "socks5", "socks5h":
			dialer, err := xproxy.SOCKS5("tcp", u.Host, nil, xproxy.Direct)
			if err != nil {
				return nil, fmt.Errorf("create gitlab socks proxy dialer: %w", err)
			}
			tr.DialContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
				type contextDialer interface {
					DialContext(context.Context, string, string) (net.Conn, error)
				}
				if d, ok := dialer.(contextDialer); ok {
					return d.DialContext(ctx, network, addr)
				}
				return dialer.Dial(network, addr)
			}
		case "http", "https":
			tr.Proxy = http.ProxyURL(u)
		default:
			return nil, fmt.Errorf("unsupported gitlab proxy scheme %q", u.Scheme)
		}
	}
	return &Client{
		cfg: cfg,
		httpClient: &http.Client{
			Timeout:   15 * time.Second,
			Transport: tr,
		},
	}, nil
}

func (c *Client) GetProject(ctx context.Context, projectPath string) (Project, error) {
	var out Project
	err := c.doJSON(ctx, http.MethodGet, "/api/v4/projects/"+url.PathEscape(projectPath), nil, &out)
	return out, err
}

func (c *Client) CreateProjectHook(ctx context.Context, projectID int64, webhookURL, secret string) (ProjectHook, error) {
	body := map[string]any{
		"url":                   webhookURL,
		"token":                 secret,
		"merge_requests_events": true,
		"pipeline_events":       true,
	}
	var out ProjectHook
	err := c.doJSON(ctx, http.MethodPost, fmt.Sprintf("/api/v4/projects/%d/hooks", projectID), body, &out)
	return out, err
}

func (c *Client) DeleteProjectHook(ctx context.Context, projectID, hookID int64) error {
	return c.doJSON(ctx, http.MethodDelete, fmt.Sprintf("/api/v4/projects/%d/hooks/%d", projectID, hookID), nil, nil)
}

func (c *Client) doJSON(ctx context.Context, method, path string, in any, out any) error {
	var body io.Reader
	if in != nil {
		var buf bytes.Buffer
		if err := json.NewEncoder(&buf).Encode(in); err != nil {
			return err
		}
		body = &buf
	}
	req, err := http.NewRequestWithContext(ctx, method, strings.TrimRight(c.cfg.BaseURL, "/")+path, body)
	if err != nil {
		return err
	}
	req.Header.Set("PRIVATE-TOKEN", c.cfg.Token)
	req.Header.Set("Accept", "application/json")
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("gitlab unreachable: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		snippet, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		switch resp.StatusCode {
		case http.StatusUnauthorized:
			return errors.New("gitlab token invalid")
		case http.StatusForbidden:
			return errors.New("gitlab token lacks required project permission")
		case http.StatusNotFound:
			return errors.New("gitlab project not found")
		default:
			return fmt.Errorf("gitlab api error %d: %s", resp.StatusCode, strings.TrimSpace(string(snippet)))
		}
	}
	if out == nil {
		return nil
	}
	return json.NewDecoder(resp.Body).Decode(out)
}
```

- [ ] **Step 4: Verify and tidy modules**

Run:

```bash
go test ./internal/integrations/gitlab
go mod tidy
```

Expected:

```text
ok   github.com/multica-ai/multica/server/internal/integrations/gitlab
go.mod lists golang.org/x/net as a direct dependency if needed
```

- [ ] **Step 5: Commit client package**

```bash
git add server/internal/integrations/gitlab server/go.mod server/go.sum
git commit -m "feat: add gitlab api client"
```

## Task 3: Backend Handler, Webhook, and Routes

**Files:**
- Create: `server/internal/handler/gitlab.go`
- Create: `server/internal/handler/gitlab_test.go`
- Modify: `server/cmd/server/router.go`
- Modify: `server/pkg/protocol/events.go`

- [ ] **Step 1: Add event constants**

Modify `server/pkg/protocol/events.go` near the GitHub event constants:

```go
// GitLab integration events
EventGitLabProjectCreated      = "gitlab_project:created"
EventGitLabProjectDeleted      = "gitlab_project:deleted"
EventGitLabMergeRequestUpdated = "gitlab_merge_request:updated"
```

- [ ] **Step 2: Write failing webhook validation tests**

Create `server/internal/handler/gitlab_test.go` with the first tests:

```go
package handler

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/multica-ai/multica/server/internal/middleware"
)

func TestGitLabWebhookRejectsBadToken(t *testing.T) {
	t.Setenv("GITLAB_BASE_URL", "https://code.mlamp.cn")
	t.Setenv("GITLAB_TOKEN", "token")
	t.Setenv("GITLAB_WEBHOOK_SECRET", "secret")

	req := httptest.NewRequest(http.MethodPost, "/api/webhooks/gitlab", bytes.NewBufferString(`{}`))
	req.Header.Set("X-Gitlab-Token", "wrong")
	req.Header.Set("X-Gitlab-Event", "Merge Request Hook")
	rec := httptest.NewRecorder()

	testHandler.HandleGitLabWebhook(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
}

func TestGitLabWebhookIgnoresUnsupportedEvent(t *testing.T) {
	t.Setenv("GITLAB_BASE_URL", "https://code.mlamp.cn")
	t.Setenv("GITLAB_TOKEN", "token")
	t.Setenv("GITLAB_WEBHOOK_SECRET", "secret")

	req := httptest.NewRequest(http.MethodPost, "/api/webhooks/gitlab", bytes.NewBufferString(`{}`))
	req.Header.Set("X-Gitlab-Token", "secret")
	req.Header.Set("X-Gitlab-Event", "Job Hook")
	rec := httptest.NewRecorder()

	testHandler.HandleGitLabWebhook(rec, req)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204", rec.Code)
	}
}
```

Run:

```bash
go test ./internal/handler -run 'TestGitLabWebhook'
```

Expected failure:

```text
testHandler.HandleGitLabWebhook undefined
```

- [ ] **Step 3: Add handler response shapes and helpers**

Create `server/internal/handler/gitlab.go` with package imports and these types/helpers. Keep `GitLabAPI` injectable so tests never call real `code.mlamp.cn`.

```go
package handler

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/integrations/gitlab"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/multica-ai/multica/server/pkg/protocol"
)

type gitLabAPI interface {
	GetProject(context.Context, string) (gitlab.Project, error)
	CreateProjectHook(context.Context, int64, string, string) (gitlab.ProjectHook, error)
	DeleteProjectHook(context.Context, int64, int64) error
}

var newGitLabClient = func(cfg gitlab.Config) (gitLabAPI, error) {
	return gitlab.NewClient(cfg)
}

type GitLabProjectBindingResponse struct {
	ID                string  `json:"id"`
	WorkspaceID       string  `json:"workspace_id"`
	GitLabProjectID   int64   `json:"gitlab_project_id"`
	PathWithNamespace string  `json:"path_with_namespace"`
	WebURL            string  `json:"web_url"`
	HookID            *int64  `json:"hook_id,omitempty"`
	HookEnabled       bool    `json:"hook_enabled"`
	LastSyncError     *string `json:"last_sync_error,omitempty"`
	CreatedAt         string  `json:"created_at"`
}

type GitLabConfigResponse struct {
	Configured       bool                        `json:"configured"`
	CanManage        bool                        `json:"can_manage"`
	BaseURL          string                      `json:"base_url,omitempty"`
	ManualWebhookURL string                      `json:"manual_webhook_url,omitempty"`
	Projects         []GitLabProjectBindingResponse `json:"projects"`
}

type GitLabMergeRequestResponse struct {
	ID                  string  `json:"id"`
	WorkspaceID         string  `json:"workspace_id"`
	ProjectPath         string  `json:"project_path"`
	GitLabProjectID     int64   `json:"gitlab_project_id"`
	IID                 int32   `json:"iid"`
	Title               string  `json:"title"`
	State               string  `json:"state"`
	WebURL              string  `json:"web_url"`
	SourceBranch        *string `json:"source_branch"`
	TargetBranch        *string `json:"target_branch"`
	AuthorUsername      *string `json:"author_username"`
	AuthorAvatarURL     *string `json:"author_avatar_url"`
	SHA                 string  `json:"sha"`
	DetailedMergeStatus *string `json:"detailed_merge_status"`
	HasConflicts        *bool   `json:"has_conflicts"`
	PipelineStatus      *string `json:"pipeline_status"`
	PipelineURL         *string `json:"pipeline_url"`
	Additions           int32   `json:"additions"`
	Deletions           int32   `json:"deletions"`
	ChangedFiles        int32   `json:"changed_files"`
	MergedAt            *string `json:"merged_at"`
	ClosedAt            *string `json:"closed_at"`
	MRCreatedAt         string  `json:"mr_created_at"`
	MRUpdatedAt         string  `json:"mr_updated_at"`
}

type createGitLabProjectRequest struct {
	Project string `json:"project"`
}

func gitLabProjectBindingToResponse(row db.GitlabProjectBinding) GitLabProjectBindingResponse {
	return GitLabProjectBindingResponse{
		ID:                uuidToString(row.ID),
		WorkspaceID:       uuidToString(row.WorkspaceID),
		GitLabProjectID:   row.GitlabProjectID,
		PathWithNamespace: row.PathWithNamespace,
		WebURL:            row.WebUrl,
		HookID:            int64PtrFromPg(row.HookID),
		HookEnabled:       row.HookEnabled,
		LastSyncError:     textToPtr(row.LastSyncError),
		CreatedAt:         timestampToString(row.CreatedAt),
	}
}

func int64PtrFromPg(v pgtype.Int8) *int64 {
	if !v.Valid {
		return nil
	}
	return &v.Int64
}

func boolPtrFromPg(v pgtype.Bool) *bool {
	if !v.Valid {
		return nil
	}
	return &v.Bool
}

func normalizeGitLabProjectInput(raw string, baseHost string) (string, bool) {
	s := strings.TrimSpace(raw)
	if s == "" {
		return "", false
	}
	if u, err := url.Parse(s); err == nil && u.Host != "" {
		if !strings.EqualFold(u.Host, baseHost) {
			return "", false
		}
		path := strings.Trim(strings.TrimSuffix(u.Path, ".git"), "/")
		return path, path != ""
	}
	s = strings.Trim(strings.TrimSuffix(s, ".git"), "/")
	return s, s != "" && !strings.Contains(s, "://")
}
```

- [ ] **Step 4: Add settings and project binding endpoints**

Append these handlers to `server/internal/handler/gitlab.go`:

```go
func (h *Handler) GetGitLabConfig(w http.ResponseWriter, r *http.Request) {
	wsID, ok := parseUUIDOrBadRequest(w, chi.URLParam(r, "id"), "workspace id")
	if !ok {
		return
	}
	cfg := gitlab.LoadConfigFromEnv()
	canManage := false
	if member, ok := ctxMember(r.Context()); ok {
		canManage = roleAllowed(member.Role, "owner", "admin")
	}
	rows, err := h.Queries.ListGitLabProjectBindingsByWorkspace(r.Context(), wsID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to list gitlab projects")
		return
	}
	projects := make([]GitLabProjectBindingResponse, 0, len(rows))
	for _, row := range rows {
		projects = append(projects, gitLabProjectBindingToResponse(row))
	}
	manualWebhookURL := "/api/webhooks/gitlab"
	if h.cfg.PublicURL != "" {
		manualWebhookURL = strings.TrimRight(h.cfg.PublicURL, "/") + manualWebhookURL
	}
	writeJSON(w, http.StatusOK, GitLabConfigResponse{
		Configured:       cfg.Configured(),
		CanManage:        canManage,
		BaseURL:          cfg.BaseURL,
		ManualWebhookURL: manualWebhookURL,
		Projects:         projects,
	})
}

func (h *Handler) CreateGitLabProjectBinding(w http.ResponseWriter, r *http.Request) {
	wsID, ok := parseUUIDOrBadRequest(w, chi.URLParam(r, "id"), "workspace id")
	if !ok {
		return
	}
	cfg := gitlab.LoadConfigFromEnv()
	if !cfg.Configured() {
		writeError(w, http.StatusServiceUnavailable, "gitlab integration is not configured")
		return
	}
	var req createGitLabProjectRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid json")
		return
	}
	projectPath, valid := normalizeGitLabProjectInput(req.Project, cfg.Host)
	if !valid {
		writeError(w, http.StatusBadRequest, "invalid gitlab project")
		return
	}
	api, err := newGitLabClient(cfg)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to initialize gitlab client")
		return
	}
	project, err := api.GetProject(r.Context(), projectPath)
	if err != nil {
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}
	connectedByID := pgtype.UUID{}
	if member, ok := ctxMember(r.Context()); ok {
		connectedByID = member.UserID
	}
	conn, err := h.Queries.UpsertGitLabConnection(r.Context(), db.UpsertGitLabConnectionParams{
		WorkspaceID:   wsID,
		BaseUrl:       cfg.BaseURL,
		Host:          cfg.Host,
		ConnectedByID: connectedByID,
	})
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to save gitlab connection")
		return
	}
	var hookID pgtype.Int8
	hookEnabled := false
	var syncErr pgtype.Text
	webhookURL := "/api/webhooks/gitlab"
	if h.cfg.PublicURL != "" {
		webhookURL = strings.TrimRight(h.cfg.PublicURL, "/") + webhookURL
	}
	if h.cfg.PublicURL != "" {
		hook, err := api.CreateProjectHook(r.Context(), project.ID, webhookURL, cfg.WebhookSecret)
		if err != nil {
			syncErr = strToText(err.Error())
		} else {
			hookID = pgtype.Int8{Int64: hook.ID, Valid: true}
			hookEnabled = true
		}
	} else {
		syncErr = strToText("MULTICA_PUBLIC_URL is not configured; create the GitLab webhook manually")
	}
	row, err := h.Queries.UpsertGitLabProjectBinding(r.Context(), db.UpsertGitLabProjectBindingParams{
		WorkspaceID:        wsID,
		ConnectionID:       conn.ID,
		GitlabProjectID:    project.ID,
		PathWithNamespace:  project.PathWithNamespace,
		WebUrl:             project.WebURL,
		HookID:             hookID,
		HookEnabled:        hookEnabled,
		LastSyncError:      syncErr,
	})
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to save gitlab project")
		return
	}
	h.publish(protocol.EventGitLabProjectCreated, uuidToString(wsID), "system", "", map[string]any{
		"project": gitLabProjectBindingToResponse(row),
	})
	writeJSON(w, http.StatusOK, map[string]any{
		"project": gitLabProjectBindingToResponse(row),
		"manual_webhook_url": webhookURL,
	})
}

func (h *Handler) DeleteGitLabProjectBinding(w http.ResponseWriter, r *http.Request) {
	wsID, ok := parseUUIDOrBadRequest(w, chi.URLParam(r, "id"), "workspace id")
	if !ok {
		return
	}
	bindingID, ok := parseUUIDOrBadRequest(w, chi.URLParam(r, "bindingId"), "binding id")
	if !ok {
		return
	}
	row, err := h.Queries.DeleteGitLabProjectBinding(r.Context(), db.DeleteGitLabProjectBindingParams{
		ID: bindingID, WorkspaceID: wsID,
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			writeError(w, http.StatusNotFound, "gitlab project binding not found")
			return
		}
		writeError(w, http.StatusInternalServerError, "failed to delete gitlab project")
		return
	}
	cfg := gitlab.LoadConfigFromEnv()
	if cfg.Configured() && row.HookID.Valid {
		if api, err := newGitLabClient(cfg); err == nil {
			_ = api.DeleteProjectHook(r.Context(), row.GitlabProjectID, row.HookID.Int64)
		}
	}
	h.publish(protocol.EventGitLabProjectDeleted, uuidToString(wsID), "system", "", map[string]any{
		"id": uuidToString(row.ID),
	})
	w.WriteHeader(http.StatusNoContent)
}
```

- [ ] **Step 5: Add webhook DTOs and handlers**

Append webhook DTOs and handlers to `server/internal/handler/gitlab.go`:

```go
type gitLabMRWebhookPayload struct {
	ObjectKind       string `json:"object_kind"`
	User            struct {
		Username  string `json:"username"`
		AvatarURL string `json:"avatar_url"`
	} `json:"user"`
	Project struct {
		ID                int64  `json:"id"`
		PathWithNamespace string `json:"path_with_namespace"`
		WebURL            string `json:"web_url"`
	} `json:"project"`
	ObjectAttributes struct {
		IID                 int32  `json:"iid"`
		Title               string `json:"title"`
		Description         string `json:"description"`
		State               string `json:"state"`
		Draft               bool   `json:"draft"`
		URL                 string `json:"url"`
		SourceBranch        string `json:"source_branch"`
		TargetBranch        string `json:"target_branch"`
		LastCommit          struct{ ID string `json:"id"` } `json:"last_commit"`
		MergeCommitSHA      string `json:"merge_commit_sha"`
		DetailedMergeStatus string `json:"detailed_merge_status"`
		HasConflicts        bool   `json:"has_conflicts"`
		CreatedAt           string `json:"created_at"`
		UpdatedAt           string `json:"updated_at"`
		MergedAt            string `json:"merged_at"`
		ClosedAt            string `json:"closed_at"`
		Action              string `json:"action"`
	} `json:"object_attributes"`
	Changes struct {
		Total struct {
			Previous int32 `json:"previous"`
			Current  int32 `json:"current"`
		} `json:"total"`
	} `json:"changes"`
}

type gitLabPipelineWebhookPayload struct {
	ObjectKind       string `json:"object_kind"`
	ObjectAttributes struct {
		ID        int64  `json:"id"`
		SHA       string `json:"sha"`
		Ref       string `json:"ref"`
		Status    string `json:"status"`
		URL       string `json:"url"`
		UpdatedAt string `json:"updated_at"`
	} `json:"object_attributes"`
	Project struct {
		ID int64 `json:"id"`
	} `json:"project"`
}

func (h *Handler) HandleGitLabWebhook(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(io.LimitReader(r.Body, 10<<20))
	if err != nil {
		writeError(w, http.StatusBadRequest, "read body failed")
		return
	}
	cfg := gitlab.LoadConfigFromEnv()
	if cfg.WebhookSecret == "" {
		writeError(w, http.StatusServiceUnavailable, "gitlab webhooks not configured")
		return
	}
	if r.Header.Get("X-Gitlab-Token") != cfg.WebhookSecret {
		writeError(w, http.StatusUnauthorized, "invalid gitlab token")
		return
	}
	switch r.Header.Get("X-Gitlab-Event") {
	case "Merge Request Hook":
		if ok := h.handleGitLabMergeRequestEvent(r.Context(), body); !ok {
			writeError(w, http.StatusBadRequest, "invalid merge request payload")
			return
		}
		w.WriteHeader(http.StatusAccepted)
	case "Pipeline Hook":
		if ok := h.handleGitLabPipelineEvent(r.Context(), body); !ok {
			writeError(w, http.StatusBadRequest, "invalid pipeline payload")
			return
		}
		w.WriteHeader(http.StatusAccepted)
	default:
		w.WriteHeader(http.StatusNoContent)
	}
}
```

Implement `handleGitLabMergeRequestEvent` with the same persisted-aggregate behavior as GitHub:

```go
func (h *Handler) handleGitLabMergeRequestEvent(ctx context.Context, body []byte) bool {
	var p gitLabMRWebhookPayload
	if err := json.Unmarshal(body, &p); err != nil {
		slog.Warn("gitlab: bad merge request payload", "err", err)
		return false
	}
	binding, err := h.Queries.GetGitLabProjectBindingByProjectID(ctx, p.Project.ID)
	if err != nil {
		if !errors.Is(err, pgx.ErrNoRows) {
			slog.Warn("gitlab: lookup project binding failed", "err", err, "project_id", p.Project.ID)
		}
		return true
	}
	attrs := p.ObjectAttributes
	state := deriveGitLabMRState(attrs.State, attrs.Draft)
	mr, err := h.Queries.UpsertGitLabMergeRequest(ctx, db.UpsertGitLabMergeRequestParams{
		WorkspaceID:          binding.WorkspaceID,
		ProjectBindingID:     binding.ID,
		GitlabProjectID:      p.Project.ID,
		MrIid:                attrs.IID,
		Title:                attrs.Title,
		Description:          strToText(attrs.Description),
		State:                state,
		WebUrl:               coalesce(attrs.URL, p.Project.WebURL+"/-/merge_requests/"+strconv.Itoa(int(attrs.IID))),
		SourceBranch:         strToText(attrs.SourceBranch),
		TargetBranch:         strToText(attrs.TargetBranch),
		AuthorUsername:       strToText(p.User.Username),
		AuthorAvatarUrl:      strToText(p.User.AvatarURL),
		Sha:                  attrs.LastCommit.ID,
		MergeCommitSha:       strToText(attrs.MergeCommitSHA),
		DetailedMergeStatus:  strToText(attrs.DetailedMergeStatus),
		HasConflicts:         pgtype.Bool{Bool: attrs.HasConflicts, Valid: true},
		Additions:            0,
		Deletions:            0,
		ChangedFiles:         attrs.Changes.Total.Current,
		MrCreatedAt:          parseGitLabTimeRequired(attrs.CreatedAt),
		MrUpdatedAt:          parseGitLabTimeRequired(attrs.UpdatedAt),
		MergedAt:             parseGitLabTime(attrs.MergedAt),
		ClosedAt:             parseGitLabTime(attrs.ClosedAt),
	})
	if err != nil {
		slog.Warn("gitlab: upsert merge request failed", "err", err)
		return true
	}

	workspaceID := uuidToString(binding.WorkspaceID)
	idents := extractIdentifiers(attrs.Title, attrs.Description, attrs.SourceBranch)
	closing := map[string]struct{}{}
	for _, c := range extractClosingIdentifiers(attrs.Title, attrs.Description) {
		closing[c] = struct{}{}
	}
	prefix := h.getIssuePrefix(ctx, binding.WorkspaceID)
	preserveCloseIntent := attrs.Action != "merge" && (state == "merged" || state == "closed")
	reevalIssues := make([]db.Issue, 0, len(idents))
	linkedIssueIDs := make([]string, 0, len(idents))
	for _, ident := range idents {
		issue, ok := h.lookupIssueByIdentifier(ctx, binding.WorkspaceID, prefix, ident)
		if !ok {
			continue
		}
		_, declared := closing[ident]
		if err := h.Queries.LinkIssueToGitLabMergeRequest(ctx, db.LinkIssueToGitLabMergeRequestParams{
			IssueID:             issue.ID,
			MergeRequestID:      mr.ID,
			CloseIntent:         declared && !preserveCloseIntent,
			PreserveCloseIntent: preserveCloseIntent,
			LinkedByType:        strToText("system"),
			LinkedByID:          pgtype.UUID{},
		}); err != nil {
			slog.Warn("gitlab: link issue failed", "err", err)
			continue
		}
		linkedIssueIDs = append(linkedIssueIDs, uuidToString(issue.ID))
		reevalIssues = append(reevalIssues, issue)
	}
	if state == "merged" || state == "closed" {
		for _, issue := range reevalIssues {
			if issue.Status == "done" || issue.Status == "cancelled" {
				continue
			}
			counts, err := h.Queries.GetIssueGitLabMRCloseAggregate(ctx, issue.ID)
			if err != nil {
				slog.Warn("gitlab: count linked mr states failed", "err", err)
				continue
			}
			if counts.OpenCount == 0 && counts.MergedWithCloseIntentCount > 0 {
				h.advanceIssueToDone(ctx, issue, workspaceID)
			}
		}
	}
	h.publish(protocol.EventGitLabMergeRequestUpdated, workspaceID, "system", "", map[string]any{
		"merge_request_id": uuidToString(mr.ID),
		"linked_issue_ids": linkedIssueIDs,
	})
	return true
}
```

Implement pipeline handling:

```go
func (h *Handler) handleGitLabPipelineEvent(ctx context.Context, body []byte) bool {
	var p gitLabPipelineWebhookPayload
	if err := json.Unmarshal(body, &p); err != nil {
		slog.Warn("gitlab: bad pipeline payload", "err", err)
		return false
	}
	binding, err := h.Queries.GetGitLabProjectBindingByProjectID(ctx, p.Project.ID)
	if err != nil {
		if !errors.Is(err, pgx.ErrNoRows) {
			slog.Warn("gitlab: lookup project binding for pipeline failed", "err", err)
		}
		return true
	}
	mrs, err := h.Queries.ListGitLabMergeRequestsByProjectSHA(ctx, db.ListGitLabMergeRequestsByProjectSHAParams{
		WorkspaceID:     binding.WorkspaceID,
		GitlabProjectID: p.Project.ID,
		Sha:             p.ObjectAttributes.SHA,
	})
	if err != nil {
		slog.Warn("gitlab: lookup merge requests for pipeline failed", "err", err)
		return true
	}
	if len(mrs) == 0 {
		slog.Info("gitlab: pipeline event has no mirrored merge request", "project_id", p.Project.ID, "sha", p.ObjectAttributes.SHA)
		return true
	}
	affectedIssues := map[string]struct{}{}
	for _, mr := range mrs {
		if err := h.Queries.UpsertGitLabMRPipeline(ctx, db.UpsertGitLabMRPipelineParams{
			MergeRequestID:    mr.ID,
			PipelineID:        p.ObjectAttributes.ID,
			Sha:               p.ObjectAttributes.SHA,
			Ref:               strToText(p.ObjectAttributes.Ref),
			Status:            normalizeGitLabPipelineStatus(p.ObjectAttributes.Status),
			WebUrl:            strToText(p.ObjectAttributes.URL),
			PipelineUpdatedAt: parseGitLabTimeRequired(p.ObjectAttributes.UpdatedAt),
		}); err != nil {
			slog.Warn("gitlab: upsert pipeline failed", "err", err)
			continue
		}
		ids, err := h.Queries.ListIssueIDsForGitLabMergeRequest(ctx, mr.ID)
		if err == nil {
			for _, id := range ids {
				affectedIssues[uuidToString(id)] = struct{}{}
			}
		}
	}
	linked := make([]string, 0, len(affectedIssues))
	for id := range affectedIssues {
		linked = append(linked, id)
	}
	h.publish(protocol.EventGitLabMergeRequestUpdated, uuidToString(binding.WorkspaceID), "system", "", map[string]any{
		"linked_issue_ids": linked,
	})
	return true
}
```

Add helper functions:

```go
func deriveGitLabMRState(state string, draft bool) string {
	if state == "merged" {
		return "merged"
	}
	if state == "closed" {
		return "closed"
	}
	if draft {
		return "draft"
	}
	return "open"
}

func normalizeGitLabPipelineStatus(s string) string {
	switch s {
	case "success", "passed":
		return "passed"
	case "failed", "canceled", "skipped", "manual":
		return "failed"
	case "running", "pending", "created", "waiting_for_resource", "preparing", "scheduled":
		return "pending"
	default:
		return "pending"
	}
}

func parseGitLabTime(s string) pgtype.Timestamptz {
	if strings.TrimSpace(s) == "" {
		return pgtype.Timestamptz{}
	}
	for _, layout := range []string{time.RFC3339Nano, time.RFC3339, "2006-01-02 15:04:05 UTC"} {
		if t, err := time.Parse(layout, s); err == nil {
			return pgtype.Timestamptz{Time: t, Valid: true}
		}
	}
	return pgtype.Timestamptz{}
}

func parseGitLabTimeRequired(s string) pgtype.Timestamptz {
	t := parseGitLabTime(s)
	if !t.Valid {
		return pgtype.Timestamptz{Time: time.Now().UTC(), Valid: true}
	}
	return t
}
```

- [ ] **Step 6: Add issue MR list handler**

Add:

```go
func (h *Handler) ListGitLabMergeRequestsForIssue(w http.ResponseWriter, r *http.Request) {
	issue, ok := h.loadIssueForUser(w, r, chi.URLParam(r, "id"))
	if !ok {
		return
	}
	rows, err := h.Queries.ListGitLabMergeRequestsByIssue(r.Context(), issue.ID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to list gitlab merge requests")
		return
	}
	out := make([]GitLabMergeRequestResponse, 0, len(rows))
	for _, row := range rows {
		out = append(out, GitLabMergeRequestResponse{
			ID:                  uuidToString(row.ID),
			WorkspaceID:         uuidToString(row.WorkspaceID),
			ProjectPath:         row.PathWithNamespace,
			GitLabProjectID:     row.GitlabProjectID,
			IID:                 row.MrIid,
			Title:               row.Title,
			State:               row.State,
			WebURL:              row.WebUrl,
			SourceBranch:        textToPtr(row.SourceBranch),
			TargetBranch:        textToPtr(row.TargetBranch),
			AuthorUsername:      textToPtr(row.AuthorUsername),
			AuthorAvatarURL:     textToPtr(row.AuthorAvatarUrl),
			SHA:                 row.Sha,
			DetailedMergeStatus: textToPtr(row.DetailedMergeStatus),
			HasConflicts:        boolPtrFromPg(row.HasConflicts),
			PipelineStatus:      textToPtr(row.PipelineStatus),
			PipelineURL:         textToPtr(row.PipelineUrl),
			Additions:           row.Additions,
			Deletions:           row.Deletions,
			ChangedFiles:        row.ChangedFiles,
			MergedAt:            timestampToPtr(row.MergedAt),
			ClosedAt:            timestampToPtr(row.ClosedAt),
			MRCreatedAt:         timestampToString(row.MrCreatedAt),
			MRUpdatedAt:         timestampToString(row.MrUpdatedAt),
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"merge_requests": out})
}
```

- [ ] **Step 7: Mount routes**

Modify `server/cmd/server/router.go`:

```go
r.Post("/api/webhooks/gitlab", h.HandleGitLabWebhook)
```

Inside `/api/workspaces/{id}` member group:

```go
r.Get("/gitlab/config", h.GetGitLabConfig)
```

Inside `/api/workspaces/{id}` admin group:

```go
r.Post("/gitlab/projects", h.CreateGitLabProjectBinding)
r.Delete("/gitlab/projects/{bindingId}", h.DeleteGitLabProjectBinding)
```

Inside `/api/issues/{id}` authenticated issue routes:

```go
r.Get("/gitlab/merge-requests", h.ListGitLabMergeRequestsForIssue)
```

- [ ] **Step 8: Add route and behavior tests**

Extend `server/internal/handler/gitlab_test.go` with DB-backed tests:

```go
func TestGitLabRoutesRoleGating(t *testing.T) {
	if testHandler == nil {
		t.Skip("handler test fixture not initialized")
	}
	ctx := context.Background()
	wsID := createGitLabTestWorkspace(t, ctx, "gitlab-routes-role-gating", "GLR")
	adminID := createGitLabTestUser(t, ctx, "gitlab-routes-admin")
	memberID := createGitLabTestUser(t, ctx, "gitlab-routes-member")
	outsiderID := createGitLabTestUser(t, ctx, "gitlab-routes-outsider")
	addGitLabTestMember(t, ctx, wsID, adminID, "admin")
	addGitLabTestMember(t, ctx, wsID, memberID, "member")

	router := chi.NewRouter()
	router.Route("/api/workspaces/{id}", func(r chi.Router) {
		r.Group(func(r chi.Router) {
			r.Use(middleware.RequireWorkspaceMemberFromURL(testHandler.Queries, "id"))
			r.Get("/gitlab/config", testHandler.GetGitLabConfig)
		})
		r.Group(func(r chi.Router) {
			r.Use(middleware.RequireWorkspaceRoleFromURL(testHandler.Queries, "id", "owner", "admin"))
			r.Post("/gitlab/projects", testHandler.CreateGitLabProjectBinding)
			r.Delete("/gitlab/projects/{bindingId}", testHandler.DeleteGitLabProjectBinding)
		})
	})

	exercise := func(method, path, userID string) int {
		req := httptest.NewRequest(method, path, bytes.NewBufferString(`{"project":"group/repo"}`))
		req.Header.Set("X-User-ID", userID)
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)
		return rec.Code
	}

	if code := exercise(http.MethodGet, "/api/workspaces/"+wsID+"/gitlab/config", memberID); code != http.StatusOK {
		t.Fatalf("member GET config = %d", code)
	}
	if code := exercise(http.MethodPost, "/api/workspaces/"+wsID+"/gitlab/projects", memberID); code != http.StatusForbidden {
		t.Fatalf("member POST project = %d", code)
	}
	if code := exercise(http.MethodGet, "/api/workspaces/"+wsID+"/gitlab/config", outsiderID); code != http.StatusNotFound {
		t.Fatalf("outsider GET config = %d", code)
	}
}
```

Add tests for:

- MR title/body/branch references create links.
- Closing intent is accepted only from title/body, not branch.
- Merged close-intent MR advances the issue to `done`.
- Multiple MRs keep the issue open while any linked MR is `open` or `draft`.
- Pipeline before MR row returns `202`/accepted and does not create rows.
- Pipeline after MR row updates the latest pipeline status.
- Unbound project webhook returns accepted/no-content behavior without dirty data.

- [ ] **Step 9: Verify backend handler**

Run:

```bash
go test ./internal/integrations/gitlab ./internal/handler -run 'TestGitLab'
go test ./internal/handler -run 'TestGitHub'
```

Expected:

```text
GitLab tests pass
Existing GitHub tests still pass
```

- [ ] **Step 10: Commit backend handler**

```bash
git add server/internal/handler/gitlab.go server/internal/handler/gitlab_test.go server/cmd/server/router.go server/pkg/protocol/events.go
git commit -m "feat: add gitlab webhook and project binding api"
```

## Task 4: Frontend Core API, Types, and Realtime Invalidation

**Files:**
- Create: `packages/core/types/gitlab.ts`
- Create: `packages/core/gitlab/queries.ts`
- Create: `packages/core/gitlab/index.ts`
- Modify: `packages/core/types/index.ts`
- Modify: `packages/core/api/client.ts`
- Modify: `packages/core/types/events.ts`
- Modify: `packages/core/realtime/use-realtime-sync.ts`

- [ ] **Step 1: Add GitLab types**

Create `packages/core/types/gitlab.ts`:

```ts
export type GitLabMergeRequestState = "open" | "closed" | "merged" | "draft";
export type GitLabPipelineStatus = "passed" | "failed" | "pending";

export interface GitLabProjectBinding {
  id: string;
  workspace_id: string;
  gitlab_project_id: number;
  path_with_namespace: string;
  web_url: string;
  hook_id?: number;
  hook_enabled: boolean;
  last_sync_error?: string | null;
  created_at: string;
}

export interface GitLabConfigResponse {
  configured: boolean;
  can_manage?: boolean;
  base_url?: string;
  manual_webhook_url?: string;
  projects: GitLabProjectBinding[];
}

export interface CreateGitLabProjectRequest {
  project: string;
}

export interface CreateGitLabProjectResponse {
  project: GitLabProjectBinding;
  manual_webhook_url?: string;
}

export interface GitLabMergeRequest {
  id: string;
  workspace_id: string;
  project_path: string;
  gitlab_project_id: number;
  iid: number;
  title: string;
  state: GitLabMergeRequestState;
  web_url: string;
  source_branch: string | null;
  target_branch: string | null;
  author_username: string | null;
  author_avatar_url: string | null;
  sha: string;
  detailed_merge_status: string | null;
  has_conflicts: boolean | null;
  pipeline_status: GitLabPipelineStatus | null;
  pipeline_url: string | null;
  additions?: number;
  deletions?: number;
  changed_files?: number;
  merged_at: string | null;
  closed_at: string | null;
  mr_created_at: string;
  mr_updated_at: string;
}

export interface ListGitLabMergeRequestsResponse {
  merge_requests: GitLabMergeRequest[];
}
```

Modify `packages/core/types/index.ts`:

```ts
export type {
  GitLabConfigResponse,
  GitLabMergeRequest,
  GitLabMergeRequestState,
  GitLabPipelineStatus,
  GitLabProjectBinding,
  CreateGitLabProjectRequest,
  CreateGitLabProjectResponse,
  ListGitLabMergeRequestsResponse,
} from "./gitlab";
```

- [ ] **Step 2: Add API client methods**

Modify the type import block in `packages/core/api/client.ts` to include:

```ts
  GitLabConfigResponse,
  CreateGitLabProjectRequest,
  CreateGitLabProjectResponse,
  ListGitLabMergeRequestsResponse,
```

Add methods near the GitHub integration methods:

```ts
  // GitLab integration
  async getGitLabConfig(workspaceId: string): Promise<GitLabConfigResponse> {
    return this.fetch(`/api/workspaces/${workspaceId}/gitlab/config`);
  }

  async createGitLabProject(
    workspaceId: string,
    input: CreateGitLabProjectRequest,
  ): Promise<CreateGitLabProjectResponse> {
    return this.fetch(`/api/workspaces/${workspaceId}/gitlab/projects`, {
      method: "POST",
      body: JSON.stringify(input),
    });
  }

  async deleteGitLabProject(workspaceId: string, bindingId: string): Promise<void> {
    await this.fetch(`/api/workspaces/${workspaceId}/gitlab/projects/${bindingId}`, {
      method: "DELETE",
    });
  }

  async listIssueGitLabMergeRequests(issueId: string): Promise<ListGitLabMergeRequestsResponse> {
    return this.fetch(`/api/issues/${issueId}/gitlab/merge-requests`);
  }
```

- [ ] **Step 3: Add React Query keys**

Create `packages/core/gitlab/queries.ts`:

```ts
import { queryOptions } from "@tanstack/react-query";
import { api } from "../api";

export const gitlabKeys = {
  all: (wsId: string) => ["gitlab", wsId] as const,
  config: (wsId: string) => [...gitlabKeys.all(wsId), "config"] as const,
  mergeRequests: (issueId: string) => ["gitlab", "merge-requests", issueId] as const,
};

export const gitlabConfigOptions = (wsId: string) =>
  queryOptions({
    queryKey: gitlabKeys.config(wsId),
    queryFn: () => api.getGitLabConfig(wsId),
    enabled: !!wsId,
  });

export const issueGitLabMergeRequestsOptions = (issueId: string) =>
  queryOptions({
    queryKey: gitlabKeys.mergeRequests(issueId),
    queryFn: () => api.listIssueGitLabMergeRequests(issueId),
    enabled: !!issueId,
  });
```

Create `packages/core/gitlab/index.ts`:

```ts
export * from "./queries";
```

- [ ] **Step 4: Add realtime event types and invalidation**

Modify `packages/core/types/events.ts`:

```ts
  | "gitlab_project:created"
  | "gitlab_project:deleted"
  | "gitlab_merge_request:updated"
```

and:

```ts
  "gitlab_project:created": unknown;
  "gitlab_project:deleted": unknown;
  "gitlab_merge_request:updated": unknown;
```

Modify `packages/core/realtime/use-realtime-sync.ts`:

```ts
import { gitlabKeys } from "../gitlab/queries";
```

Add prefix handlers:

```ts
      gitlab_project: () => {
        const wsId = getCurrentWsId();
        if (wsId) qc.invalidateQueries({ queryKey: gitlabKeys.config(wsId) });
      },
      gitlab_merge_request: () => {
        qc.invalidateQueries({ queryKey: ["gitlab", "merge-requests"] });
      },
```

- [ ] **Step 5: Verify core package**

Run:

```bash
pnpm test --filter @multica/core -- --run
pnpm typecheck
```

Expected:

```text
Core tests pass
TypeScript check does not report missing GitLab exports
```

- [ ] **Step 6: Commit frontend core**

```bash
git add packages/core/types/gitlab.ts packages/core/gitlab packages/core/types/index.ts packages/core/api/client.ts packages/core/types/events.ts packages/core/realtime/use-realtime-sync.ts
git commit -m "feat: add gitlab frontend core api"
```

## Task 5: Settings UI for GitLab Project Bindings

**Files:**
- Create: `packages/views/settings/components/gitlab-mark.tsx`
- Create: `packages/views/settings/components/gitlab-tab.tsx`
- Create: `packages/views/settings/components/gitlab-tab.test.tsx`
- Modify: `packages/views/settings/components/settings-page.tsx`
- Modify: `packages/views/locales/en/settings.json`
- Modify: `packages/views/locales/zh-Hans/settings.json`
- Modify: `packages/views/locales/ja/settings.json`
- Modify: `packages/views/locales/ko/settings.json`

- [ ] **Step 1: Write failing settings tests**

Create `packages/views/settings/components/gitlab-tab.test.tsx`:

```tsx
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { vi, describe, it, expect, beforeEach } from "vitest";
import { GitLabTab } from "./gitlab-tab";

const configRef = {
  current: {
    configured: true,
    can_manage: true,
    base_url: "https://code.mlamp.cn",
    manual_webhook_url: "https://multica.example/api/webhooks/gitlab",
    projects: [],
  },
};

vi.mock("@multica/core/hooks", () => ({ useWorkspaceId: () => "ws-1" }));
vi.mock("@multica/core/gitlab", () => ({
  gitlabConfigOptions: () => ({
    queryKey: ["gitlab", "ws-1", "config"],
    queryFn: async () => configRef.current,
  }),
  gitlabKeys: {
    config: (wsId: string) => ["gitlab", wsId, "config"],
  },
}));
vi.mock("@multica/core/api", () => ({
  api: {
    createGitLabProject: vi.fn(async () => ({
      project: {
        id: "binding-1",
        workspace_id: "ws-1",
        gitlab_project_id: 42,
        path_with_namespace: "group/repo",
        web_url: "https://code.mlamp.cn/group/repo",
        hook_enabled: true,
        created_at: "2026-07-01T00:00:00Z",
      },
    })),
    deleteGitLabProject: vi.fn(async () => undefined),
  },
}));
vi.mock("../../i18n", () => ({
  useT: () => ({
    t: (fn: any, vars?: Record<string, unknown>) => {
      const dict = {
        gitlab_title: "GitLab",
        gitlab_project_placeholder: "group/project or URL",
        gitlab_add_project: "Add project",
        gitlab_not_configured: "GitLab integration is not configured",
        gitlab_manual_secret_hint: "Use the deployment configured webhook secret.",
      };
      const proxy = { gitlab: dict };
      const value = fn(proxy);
      return typeof value === "string"
        ? value.replace(/\{\{(\w+)\}\}/g, (_, k) => String(vars?.[k] ?? ""))
        : value;
    },
  }),
}));

function renderTab() {
  const qc = new QueryClient();
  render(
    <QueryClientProvider client={qc}>
      <GitLabTab />
    </QueryClientProvider>,
  );
}

describe("GitLabTab", () => {
  beforeEach(() => {
    configRef.current.projects = [];
    configRef.current.configured = true;
    configRef.current.can_manage = true;
  });

  it("renders configured GitLab base URL and add form", async () => {
    renderTab();
    expect(await screen.findByText("https://code.mlamp.cn")).toBeInTheDocument();
    expect(screen.getByPlaceholderText("group/project or URL")).toBeInTheDocument();
  });

  it("shows manual webhook fallback without raw secret", async () => {
    configRef.current.projects = [{
      id: "binding-1",
      workspace_id: "ws-1",
      gitlab_project_id: 42,
      path_with_namespace: "group/repo",
      web_url: "https://code.mlamp.cn/group/repo",
      hook_enabled: false,
      last_sync_error: "gitlab token lacks required project permission",
      created_at: "2026-07-01T00:00:00Z",
    }];
    renderTab();
    expect(await screen.findByText("group/repo")).toBeInTheDocument();
    expect(screen.getByText("https://multica.example/api/webhooks/gitlab")).toBeInTheDocument();
    expect(screen.getByText("Use the deployment configured webhook secret.")).toBeInTheDocument();
    expect(screen.queryByText(/GITLAB_WEBHOOK_SECRET=|hook-secret|secret-token/)).not.toBeInTheDocument();
  });

  it("submits project path", async () => {
    const { api } = await import("@multica/core/api");
    renderTab();
    await userEvent.type(await screen.findByPlaceholderText("group/project or URL"), "group/repo");
    await userEvent.click(screen.getByRole("button", { name: "Add project" }));
    expect(api.createGitLabProject).toHaveBeenCalledWith("ws-1", { project: "group/repo" });
  });
});
```

Run:

```bash
pnpm test --filter @multica/views -- gitlab-tab.test.tsx --run
```

Expected failure:

```text
Cannot find module './gitlab-tab'
```

- [ ] **Step 2: Add GitLab mark**

Create `packages/views/settings/components/gitlab-mark.tsx`:

```tsx
export function GitLabMark({ className }: { className?: string }) {
  return (
    <svg viewBox="0 0 24 24" aria-hidden="true" className={className} fill="currentColor">
      <path d="M22.65 13.02 20.9 7.63a.9.9 0 0 0-1.72-.02l-1.18 3.6H6l-1.18-3.6a.9.9 0 0 0-1.72.02l-1.75 5.39a1.37 1.37 0 0 0 .5 1.53L12 22.02l10.15-7.47a1.37 1.37 0 0 0 .5-1.53ZM8.74 13.01h6.52L12 17.77l-3.26-4.76Z" />
    </svg>
  );
}
```

- [ ] **Step 3: Add GitLab tab component**

Create `packages/views/settings/components/gitlab-tab.tsx`:

```tsx
"use client";

import { FormEvent, useState } from "react";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { toast } from "sonner";
import { ExternalLink, Trash2 } from "lucide-react";
import { Button } from "@multica/ui/components/ui/button";
import { Card, CardContent } from "@multica/ui/components/ui/card";
import { Input } from "@multica/ui/components/ui/input";
import { useWorkspaceId } from "@multica/core/hooks";
import { api } from "@multica/core/api";
import { gitlabConfigOptions, gitlabKeys } from "@multica/core/gitlab";
import { useT } from "../../i18n";
import { GitLabMark } from "./gitlab-mark";

export function GitLabTab() {
  const { t } = useT("settings");
  const wsId = useWorkspaceId();
  const qc = useQueryClient();
  const [project, setProject] = useState("");
  const { data, isLoading } = useQuery(gitlabConfigOptions(wsId));
  const canManage = data?.can_manage === true;

  const createMutation = useMutation({
    mutationFn: () => api.createGitLabProject(wsId, { project }),
    onSuccess: async () => {
      setProject("");
      await qc.invalidateQueries({ queryKey: gitlabKeys.config(wsId) });
      toast.success(t(($) => $.gitlab.toast_project_added));
    },
    onError: (err) => toast.error(err instanceof Error ? err.message : t(($) => $.gitlab.toast_project_add_failed)),
  });

  const deleteMutation = useMutation({
    mutationFn: (id: string) => api.deleteGitLabProject(wsId, id),
    onSuccess: async () => {
      await qc.invalidateQueries({ queryKey: gitlabKeys.config(wsId) });
      toast.success(t(($) => $.gitlab.toast_project_deleted));
    },
    onError: (err) => toast.error(err instanceof Error ? err.message : t(($) => $.gitlab.toast_project_delete_failed)),
  });

  function submit(e: FormEvent) {
    e.preventDefault();
    if (!project.trim() || createMutation.isPending) return;
    createMutation.mutate();
  }

  if (isLoading) {
    return <p className="text-sm text-muted-foreground">{t(($) => $.gitlab.loading)}</p>;
  }

  return (
    <div className="space-y-6">
      <section className="space-y-1">
        <div className="flex items-center gap-2">
          <GitLabMark className="h-5 w-5 text-muted-foreground" />
          <h2 className="text-sm font-semibold">{t(($) => $.gitlab.title)}</h2>
        </div>
        <p className="text-sm text-muted-foreground">{t(($) => $.gitlab.page_description)}</p>
      </section>

      <Card>
        <CardContent className="space-y-4">
          <div className="flex flex-wrap items-center justify-between gap-3">
            <div>
              <p className="text-sm font-medium">{data?.base_url ?? "https://code.mlamp.cn"}</p>
              <p className="text-xs text-muted-foreground">
                {data?.configured ? t(($) => $.gitlab.configured) : t(($) => $.gitlab.not_configured)}
              </p>
            </div>
          </div>

          {canManage ? (
            <form onSubmit={submit} className="flex flex-col gap-2 sm:flex-row">
              <Input
                value={project}
                onChange={(e) => setProject(e.target.value)}
                placeholder={t(($) => $.gitlab.project_placeholder)}
                disabled={!data?.configured || createMutation.isPending}
              />
              <Button type="submit" disabled={!data?.configured || createMutation.isPending || !project.trim()}>
                {createMutation.isPending ? t(($) => $.gitlab.adding_project) : t(($) => $.gitlab.add_project)}
              </Button>
            </form>
          ) : (
            <p className="text-xs text-muted-foreground">{t(($) => $.gitlab.read_only_hint)}</p>
          )}
        </CardContent>
      </Card>

      <section className="space-y-3">
        <h3 className="text-sm font-semibold">{t(($) => $.gitlab.projects_title)}</h3>
        {(data?.projects ?? []).length === 0 ? (
          <p className="text-sm text-muted-foreground">{t(($) => $.gitlab.projects_empty)}</p>
        ) : (
          <div className="space-y-2">
            {data!.projects.map((p) => (
              <Card key={p.id}>
                <CardContent className="space-y-3">
                  <div className="flex items-start justify-between gap-3">
                    <div className="min-w-0">
                      <a href={p.web_url} target="_blank" rel="noreferrer noopener" className="inline-flex max-w-full items-center gap-1 text-sm font-medium hover:underline">
                        <span className="truncate">{p.path_with_namespace}</span>
                        <ExternalLink className="h-3 w-3 shrink-0" />
                      </a>
                      <p className="text-xs text-muted-foreground">
                        {p.hook_enabled ? t(($) => $.gitlab.hook_enabled) : t(($) => $.gitlab.hook_manual_required)}
                      </p>
                    </div>
                    {canManage ? (
                      <Button
                        variant="ghost"
                        size="icon"
                        onClick={() => deleteMutation.mutate(p.id)}
                        disabled={deleteMutation.isPending}
                        aria-label={t(($) => $.gitlab.remove_project)}
                      >
                        <Trash2 className="h-4 w-4" />
                      </Button>
                    ) : null}
                  </div>
                  {!p.hook_enabled ? (
                    <div className="rounded-md border bg-muted/40 p-3 text-xs text-muted-foreground">
                      {p.last_sync_error ? <p>{p.last_sync_error}</p> : null}
                      {data?.manual_webhook_url ? (
                        <code className="mt-2 block break-all rounded bg-background px-2 py-1">{data.manual_webhook_url}</code>
                      ) : null}
                      <p className="mt-2">{t(($) => $.gitlab.manual_secret_hint)}</p>
                    </div>
                  ) : null}
                </CardContent>
              </Card>
            ))}
          </div>
        )}
      </section>
    </div>
  );
}
```

- [ ] **Step 4: Add settings tab wiring**

Modify `packages/views/settings/components/settings-page.tsx`:

```tsx
import { GitLabMark } from "./gitlab-mark";
import { GitLabTab } from "./gitlab-tab";
```

Add `"gitlab"` to `WORKSPACE_TAB_KEYS`, `WORKSPACE_TAB_VALUES`, and `WORKSPACE_TAB_ICONS`:

```tsx
const WORKSPACE_TAB_KEYS = [
  "general",
  "repositories",
  "github",
  "gitlab",
  "integrations",
  "labs",
  "members",
] as const;

const WORKSPACE_TAB_VALUES = {
  general: "workspace",
  repositories: "repositories",
  github: "github",
  gitlab: "gitlab",
  integrations: "integrations",
  labs: "labs",
  members: "members",
} as const;

const WORKSPACE_TAB_ICONS = {
  general: Settings,
  repositories: FolderGit2,
  github: GitHubMark,
  gitlab: GitLabMark,
  integrations: Plug,
  labs: FlaskConical,
  members: Users,
} as const;
```

Add content:

```tsx
<TabsContent value="gitlab"><GitLabTab /></TabsContent>
```

- [ ] **Step 5: Add locale keys**

Add to each `settings.json` under `page.tabs`:

```json
"gitlab": "GitLab"
```

Add `gitlab` object to `packages/views/locales/en/settings.json`:

```json
"gitlab": {
  "title": "GitLab",
  "page_description": "Bind GitLab projects from code.mlamp.cn so Multica can mirror merge requests and pipeline state.",
  "configured": "Configured on this deployment",
  "not_configured": "GitLab integration is not configured",
  "loading": "Loading GitLab settings...",
  "project_placeholder": "group/project or URL",
  "add_project": "Add project",
  "adding_project": "Adding...",
  "read_only_hint": "Read-only view. Only workspace admins and owners can add or remove GitLab projects.",
  "projects_title": "Projects",
  "projects_empty": "No GitLab projects bound yet.",
  "hook_enabled": "Webhook registered",
  "hook_manual_required": "Manual webhook setup required",
  "manual_secret_hint": "Use the deployment configured GITLAB_WEBHOOK_SECRET as the GitLab webhook secret. The secret is not shown here.",
  "remove_project": "Remove project",
  "toast_project_added": "GitLab project added",
  "toast_project_add_failed": "Failed to add GitLab project",
  "toast_project_deleted": "GitLab project removed",
  "toast_project_delete_failed": "Failed to remove GitLab project"
}
```

Add equivalent translated keys to `zh-Hans`, `ja`, and `ko`. Keep the same object shape so `packages/views/locales/parity.test.ts` passes.

- [ ] **Step 6: Verify settings UI**

Run:

```bash
pnpm test --filter @multica/views -- gitlab-tab.test.tsx --run
pnpm test --filter @multica/views -- locales/parity.test.ts --run
pnpm typecheck
```

Expected:

```text
GitLab settings tests pass
Locale parity test passes
TypeScript check passes
```

- [ ] **Step 7: Commit settings UI**

```bash
git add packages/views/settings/components/gitlab-mark.tsx packages/views/settings/components/gitlab-tab.tsx packages/views/settings/components/gitlab-tab.test.tsx packages/views/settings/components/settings-page.tsx packages/views/locales/en/settings.json packages/views/locales/zh-Hans/settings.json packages/views/locales/ja/settings.json packages/views/locales/ko/settings.json
git commit -m "feat: add gitlab settings tab"
```

## Task 6: Mixed GitHub PR and GitLab MR Issue Sidebar

**Files:**
- Modify: `packages/views/issues/components/pull-request-list.tsx`
- Modify: `packages/views/issues/components/pull-request-list.test.tsx`
- Modify: `packages/views/locales/en/issues.json`
- Modify: `packages/views/locales/zh-Hans/issues.json`
- Modify: `packages/views/locales/ja/issues.json`
- Modify: `packages/views/locales/ko/issues.json`

- [ ] **Step 1: Write failing mixed-row test**

Modify `packages/views/issues/components/pull-request-list.test.tsx` to mock both query options and assert both providers render:

```tsx
const mockGitLabMRs = [{
  id: "mr-1",
  workspace_id: "ws-1",
  project_path: "group/repo",
  gitlab_project_id: 42,
  iid: 7,
  title: "Fix MUL-1 from GitLab",
  state: "merged",
  web_url: "https://code.mlamp.cn/group/repo/-/merge_requests/7",
  source_branch: "fix/mul-1",
  target_branch: "main",
  author_username: "alice",
  author_avatar_url: null,
  sha: "abc",
  detailed_merge_status: "mergeable",
  has_conflicts: false,
  pipeline_status: "passed",
  pipeline_url: "https://code.mlamp.cn/group/repo/-/pipelines/99",
  additions: 4,
  deletions: 1,
  changed_files: 2,
  merged_at: "2026-07-01T00:00:00Z",
  closed_at: null,
  mr_created_at: "2026-07-01T00:00:00Z",
  mr_updated_at: "2026-07-01T00:00:00Z",
}];

vi.mock("@multica/core/gitlab", () => ({
  issueGitLabMergeRequestsOptions: (issueId: string) => ({
    queryKey: ["gitlab", "merge-requests", issueId],
    queryFn: async () => ({ merge_requests: mockGitLabMRs }),
  }),
}));

it("renders GitHub PRs and GitLab MRs together", async () => {
  render(<PullRequestList issueId="issue-1" />);
  expect(await screen.findByText("Fix MUL-1 from GitLab")).toBeInTheDocument();
  expect(screen.getByText(/group\/repo!7/)).toBeInTheDocument();
  expect(screen.getByText(/GitLab/)).toBeInTheDocument();
});
```

Run:

```bash
pnpm test --filter @multica/views -- pull-request-list.test.tsx --run
```

Expected failure:

```text
GitLab row is not rendered
```

- [ ] **Step 2: Fetch both providers**

Modify imports in `pull-request-list.tsx`:

```tsx
import { issueGitLabMergeRequestsOptions } from "@multica/core/gitlab";
import type { GitLabMergeRequest } from "@multica/core/types";
```

Inside `PullRequestList`:

```tsx
const githubQuery = useQuery(issuePullRequestsOptions(issueId));
const gitlabQuery = useQuery(issueGitLabMergeRequestsOptions(issueId));
const rows = [
  ...(githubQuery.data?.pull_requests ?? []).map((pr) => ({ provider: "github" as const, item: pr })),
  ...(gitlabQuery.data?.merge_requests ?? []).map((mr) => ({ provider: "gitlab" as const, item: mr })),
].sort((a, b) => {
  const at = a.provider === "github" ? a.item.pr_created_at : a.item.mr_created_at;
  const bt = b.provider === "github" ? b.item.pr_created_at : b.item.mr_created_at;
  return bt.localeCompare(at);
});
const isLoading = githubQuery.isLoading || gitlabQuery.isLoading;
```

Render `rows` with provider-specific row components:

```tsx
{expandedHead.map((row) =>
  row.provider === "github" ? (
    <PullRequestRow key={`github-${row.item.id}`} pr={row.item} />
  ) : (
    <GitLabMergeRequestRow key={`gitlab-${row.item.id}`} mr={row.item} />
  ),
)}
```

- [ ] **Step 3: Add GitLab row rendering**

Add this component to `pull-request-list.tsx`:

```tsx
function GitLabMergeRequestRow({ mr }: { mr: GitLabMergeRequest }) {
  const { t } = useT("issues");
  const cfg = STATE_ICON[mr.state] ?? { icon: GitPullRequest, className: "" };
  const StateIcon = cfg.icon;
  const kind = derivePullRequestStatusKind({
    state: mr.state,
    mergeable_state: mr.has_conflicts ? "dirty" : mr.detailed_merge_status === "mergeable" ? "clean" : null,
    checks_failed: mr.pipeline_status === "failed" ? 1 : 0,
    checks_pending: mr.pipeline_status === "pending" ? 1 : 0,
    checks_passed: mr.pipeline_status === "passed" ? 1 : 0,
  });
  const segments = derivePullRequestProgressSegments({
    state: mr.state,
    checks_failed: mr.pipeline_status === "failed" ? 1 : 0,
    checks_pending: mr.pipeline_status === "pending" ? 1 : 0,
    checks_passed: mr.pipeline_status === "passed" ? 1 : 0,
  });
  const statusText = useStatusText(kind);
  const showStats = shouldShowPullRequestStats(mr);
  return (
    <a
      data-testid="pull-request-row"
      href={mr.web_url}
      target="_blank"
      rel="noreferrer noopener"
      className={cn("flex items-start gap-2 rounded-md px-2 py-1.5 -mx-2 hover:bg-accent/50 transition-colors group", mr.state === "draft" ? "opacity-80" : null)}
    >
      <StateIcon className={cn("h-3.5 w-3.5 mt-0.5 shrink-0", cfg.className)} />
      <div className="min-w-0 flex-1">
        <p className="text-xs font-medium leading-snug truncate group-hover:text-foreground">
          {mr.title}
        </p>
        <p className="text-[11px] text-muted-foreground truncate">
          {t(($) => $.detail.merge_request_provider_gitlab)} · {mr.project_path}!{mr.iid} · {getStateLabel(mr.state, t)}
          {mr.author_username ? ` · @${mr.author_username}` : null}
        </p>
        <div className="mt-1 flex flex-wrap items-center gap-x-2 gap-y-0.5 text-[11px] text-muted-foreground">
          {showStats ? (
            <span className="inline-flex items-center gap-1.5 tabular-nums">
              <span className="text-emerald-600 dark:text-emerald-400">+{mr.additions ?? 0}</span>
              <span className="text-rose-600 dark:text-rose-400">-{mr.deletions ?? 0}</span>
              <span aria-hidden="true">·</span>
              <span>{t(($) => $.detail.pull_request_card_files_count, { count: mr.changed_files ?? 0 })}</span>
            </span>
          ) : null}
          <PullRequestProgressStrip segments={segments} />
          <span className="truncate">{mr.state === "draft" ? t(($) => $.detail.pull_request_card_draft_prefix, { status: statusText }) : statusText}</span>
        </div>
      </div>
    </a>
  );
}
```

- [ ] **Step 4: Add issue locale key**

Add under `detail` in `packages/views/locales/en/issues.json`:

```json
"merge_request_provider_gitlab": "GitLab"
```

Add equivalent values to `zh-Hans`, `ja`, and `ko`.

- [ ] **Step 5: Verify issue sidebar**

Run:

```bash
pnpm test --filter @multica/views -- pull-request-list.test.tsx --run
pnpm test --filter @multica/views -- locales/parity.test.ts --run
pnpm typecheck
```

Expected:

```text
Mixed provider sidebar tests pass
Locale parity passes
TypeScript check passes
```

- [ ] **Step 6: Commit issue sidebar**

```bash
git add packages/views/issues/components/pull-request-list.tsx packages/views/issues/components/pull-request-list.test.tsx packages/views/locales/en/issues.json packages/views/locales/zh-Hans/issues.json packages/views/locales/ja/issues.json packages/views/locales/ko/issues.json
git commit -m "feat: show gitlab merge requests on issues"
```

## Task 7: End-to-End Verification and Cleanup

**Files:**
- Modify only files touched by prior tasks if verification exposes defects.

- [ ] **Step 1: Run backend checks**

```bash
make sqlc
go test ./internal/integrations/gitlab ./internal/handler
```

Expected:

```text
sqlc output is current
GitLab, GitHub, and handler regression tests pass
```

- [ ] **Step 2: Run frontend checks**

```bash
pnpm test --filter @multica/core -- --run
pnpm test --filter @multica/views -- --run
pnpm typecheck
```

Expected:

```text
Core tests pass
Views tests pass
TypeScript check passes
```

- [ ] **Step 3: Run broader backend verification if local DB is available**

```bash
make test
```

Expected:

```text
Go test suite passes
```

If local DB is not available, record the exact failure in the final handoff and keep the narrower package test output.

- [ ] **Step 4: Manual deployment smoke on `dj` after merge**

With the reverse SOCKS tunnel running from `my-mini`:

```bash
curl --socks5-hostname 127.0.0.1:18080 -I https://code.mlamp.cn
```

Expected:

```text
HTTP/2 302
location: https://code.mlamp.cn/users/sign_in
```

Configure the Multica server process:

```bash
GITLAB_BASE_URL=https://code.mlamp.cn
GITLAB_TOKEN=<deployment token>
GITLAB_WEBHOOK_SECRET=<deployment webhook secret>
GITLAB_PROXY_URL=socks5h://127.0.0.1:18080
MULTICA_PUBLIC_URL=<dj public URL>
```

Then bind one test project, open an MR that references a Multica issue, and confirm:

- settings shows the project binding;
- GitLab webhook deliveries return 202;
- issue detail shows the GitLab MR row;
- pipeline state changes update the MR row;
- `Fixes PREFIX-N` in title/body moves the issue to `done` only after all linked MRs are terminal.

- [ ] **Step 5: Final commit if verification fixes were needed**

```bash
git status --short
git add <only-files-fixed-during-verification>
git commit -m "fix: stabilize gitlab integration verification"
```

## Self-Review

Spec coverage:

- Deployment-level `code.mlamp.cn` only: Task 2 config, Task 3 binding validation.
- GitLab-specific proxy only: Task 2 client transport.
- No token in DB: Task 1 schema stores connection metadata only.
- Project binding and auto/manual webhook registration: Task 3 endpoints.
- Webhook validation and unbound project behavior: Task 3 tests and handler.
- MR mirror, issue linking, close intent, and auto-completion: Task 3 webhook implementation and tests.
- Pipeline mirror and out-of-order ignore behavior: Task 3 pipeline implementation and tests.
- Frontend settings and no raw secret display: Task 5 tests and UI.
- Mixed GitHub/GitLab issue sidebar: Task 6 tests and UI.
- Realtime invalidation: Task 3 protocol events and Task 4 core invalidation.

Placeholder scan:

- No `TBD` or empty implementation blocks are present.
- Deferred product features remain outside this plan by design.

Type consistency:

- Backend JSON fields use `merge_requests`, `project_path`, `web_url`, `pipeline_status`.
- Frontend types and row rendering use the same field names.
- Query keys use `["gitlab", wsId, "config"]` and `["gitlab", "merge-requests", issueId]`.
