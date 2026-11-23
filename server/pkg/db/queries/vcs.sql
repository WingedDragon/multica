-- =====================
-- VCS Connection (Forgejo / Gitea / GitLab)
-- =====================

-- name: ListVCSConnectionsByWorkspace :many
SELECT * FROM vcs_connection
WHERE workspace_id = $1
ORDER BY created_at ASC;

-- name: GetVCSConnectionByID :one
SELECT * FROM vcs_connection
WHERE id = $1;

-- name: UpsertVCSConnection :one
-- Reconnecting the same instance rotates the stored token/secret, provider,
-- and identity in place rather than creating a duplicate row.
INSERT INTO vcs_connection (
    workspace_id, provider, instance_url, account_login,
    access_token_encrypted, webhook_secret_encrypted, connected_by_id
) VALUES (
    $1, $2, $3, $4, $5, $6, sqlc.narg('connected_by_id')
)
ON CONFLICT (workspace_id, instance_url) DO UPDATE SET
    provider                 = EXCLUDED.provider,
    account_login            = EXCLUDED.account_login,
    access_token_encrypted   = EXCLUDED.access_token_encrypted,
    webhook_secret_encrypted = EXCLUDED.webhook_secret_encrypted,
    connected_by_id          = EXCLUDED.connected_by_id,
    updated_at               = now()
RETURNING *;

-- name: DeleteVCSConnection :exec
-- The VCS mirror tables intentionally have no foreign keys. Remove the
-- connection's MR pipeline ledger, issue links, commit statuses, and MRs in
-- the same statement as the connection, scoped to the owning workspace.
WITH target AS (
    SELECT vcs_connection.id FROM vcs_connection WHERE vcs_connection.id = $1 AND vcs_connection.workspace_id = $2
),
cleared_links AS (
    DELETE FROM issue_vcs_pull_request
    WHERE pull_request_id IN (
        SELECT vcs_pull_request.id FROM vcs_pull_request
        WHERE vcs_pull_request.connection_id IN (SELECT target.id FROM target)
    )
),
cleared_statuses AS (
    DELETE FROM vcs_commit_status WHERE connection_id IN (SELECT target.id FROM target)
),
cleared_pipelines AS (
    DELETE FROM vcs_gitlab_pipeline WHERE connection_id IN (SELECT target.id FROM target)
),
cleared_prs AS (
    DELETE FROM vcs_pull_request WHERE connection_id IN (SELECT target.id FROM target)
)
DELETE FROM vcs_connection WHERE vcs_connection.id = $1 AND vcs_connection.workspace_id = $2;

-- name: RotateVCSConnectionWebhookSecret :one
UPDATE vcs_connection
SET webhook_secret_encrypted = $3,
    updated_at = now()
WHERE id = $1 AND workspace_id = $2
RETURNING *;

-- =====================
-- VCS Pull Request
-- =====================

-- name: UpsertVCSPullRequest :one
-- pr_updated_at guards against an out-of-order webhook redelivery regressing
-- the PR state. Conflict data is head-versioned; a missing value never turns a
-- previously known result into a result for a new head.
INSERT INTO vcs_pull_request (
    workspace_id, connection_id, provider, repo_owner, repo_name, pr_number,
    title, state, html_url, branch, author_login, author_avatar_url,
    merged_at, closed_at, pr_created_at, pr_updated_at,
    additions, deletions, changed_files, head_sha, has_conflicts,
    conflict_head_sha, source_project_id, source_project_path
) VALUES (
    $1, $2, $3, $4, $5, $6,
    $7, $8, $9, sqlc.narg('branch'), sqlc.narg('author_login'), sqlc.narg('author_avatar_url'),
    sqlc.narg('merged_at'), sqlc.narg('closed_at'), $10, $11,
    $12, $13, $14, $15, sqlc.narg('has_conflicts'), $16,
    sqlc.narg('source_project_id'), sqlc.narg('source_project_path')
)
ON CONFLICT (connection_id, repo_owner, repo_name, pr_number) DO UPDATE SET
    workspace_id      = CASE WHEN EXCLUDED.pr_updated_at >= vcs_pull_request.pr_updated_at THEN EXCLUDED.workspace_id      ELSE vcs_pull_request.workspace_id      END,
    provider          = CASE WHEN EXCLUDED.pr_updated_at >= vcs_pull_request.pr_updated_at THEN EXCLUDED.provider          ELSE vcs_pull_request.provider          END,
    title             = CASE WHEN EXCLUDED.pr_updated_at >= vcs_pull_request.pr_updated_at THEN EXCLUDED.title             ELSE vcs_pull_request.title             END,
    state             = CASE WHEN EXCLUDED.pr_updated_at >= vcs_pull_request.pr_updated_at THEN EXCLUDED.state             ELSE vcs_pull_request.state             END,
    html_url          = CASE WHEN EXCLUDED.pr_updated_at >= vcs_pull_request.pr_updated_at THEN EXCLUDED.html_url          ELSE vcs_pull_request.html_url          END,
    branch            = CASE WHEN EXCLUDED.pr_updated_at >= vcs_pull_request.pr_updated_at THEN EXCLUDED.branch            ELSE vcs_pull_request.branch            END,
    author_login      = CASE WHEN EXCLUDED.pr_updated_at >= vcs_pull_request.pr_updated_at THEN EXCLUDED.author_login      ELSE vcs_pull_request.author_login      END,
    author_avatar_url = CASE WHEN EXCLUDED.pr_updated_at >= vcs_pull_request.pr_updated_at THEN EXCLUDED.author_avatar_url ELSE vcs_pull_request.author_avatar_url END,
    merged_at         = CASE WHEN EXCLUDED.pr_updated_at >= vcs_pull_request.pr_updated_at THEN EXCLUDED.merged_at         ELSE vcs_pull_request.merged_at         END,
    closed_at         = CASE WHEN EXCLUDED.pr_updated_at >= vcs_pull_request.pr_updated_at THEN EXCLUDED.closed_at         ELSE vcs_pull_request.closed_at         END,
    pr_updated_at     = CASE WHEN EXCLUDED.pr_updated_at >= vcs_pull_request.pr_updated_at THEN EXCLUDED.pr_updated_at     ELSE vcs_pull_request.pr_updated_at     END,
    additions         = CASE WHEN EXCLUDED.pr_updated_at >= vcs_pull_request.pr_updated_at THEN EXCLUDED.additions         ELSE vcs_pull_request.additions         END,
    deletions         = CASE WHEN EXCLUDED.pr_updated_at >= vcs_pull_request.pr_updated_at THEN EXCLUDED.deletions         ELSE vcs_pull_request.deletions         END,
    changed_files     = CASE WHEN EXCLUDED.pr_updated_at >= vcs_pull_request.pr_updated_at THEN EXCLUDED.changed_files     ELSE vcs_pull_request.changed_files     END,
    head_sha          = CASE WHEN EXCLUDED.pr_updated_at >= vcs_pull_request.pr_updated_at THEN EXCLUDED.head_sha          ELSE vcs_pull_request.head_sha          END,
    has_conflicts     = CASE WHEN EXCLUDED.pr_updated_at >= vcs_pull_request.pr_updated_at AND EXCLUDED.provider = 'gitlab' THEN EXCLUDED.has_conflicts ELSE vcs_pull_request.has_conflicts END,
    conflict_head_sha = CASE WHEN EXCLUDED.pr_updated_at >= vcs_pull_request.pr_updated_at AND EXCLUDED.provider = 'gitlab' THEN EXCLUDED.conflict_head_sha ELSE vcs_pull_request.conflict_head_sha END,
    source_project_id = CASE WHEN EXCLUDED.pr_updated_at >= vcs_pull_request.pr_updated_at THEN EXCLUDED.source_project_id ELSE vcs_pull_request.source_project_id END,
    source_project_path = CASE WHEN EXCLUDED.pr_updated_at >= vcs_pull_request.pr_updated_at THEN EXCLUDED.source_project_path ELSE vcs_pull_request.source_project_path END,
    updated_at        = now()
RETURNING *;

-- name: ListVCSPullRequestsByIssue :many
-- Aggregates each PR's commit statuses for its CURRENT head sha into
-- passed/failed/pending counts. vcs_commit_status holds one row per
-- (connection, sha, context) with a normalized state, so a count by state is
-- correct. Statuses for an old head sha stay stored but are excluded by the
-- head_sha join, so a stale run can't pollute the bar.
WITH checks AS (
    SELECT
        pr.id AS pr_id,
        COUNT(*)::bigint AS total,
        SUM(CASE WHEN cs.state = 'failed'  THEN 1 ELSE 0 END)::bigint AS failed,
        SUM(CASE WHEN cs.state = 'passed'  THEN 1 ELSE 0 END)::bigint AS passed,
        SUM(CASE WHEN cs.state = 'pending' THEN 1 ELSE 0 END)::bigint AS pending
    FROM vcs_pull_request pr
    JOIN issue_vcs_pull_request ipr ON ipr.pull_request_id = pr.id
    JOIN vcs_commit_status cs
        ON cs.connection_id = pr.connection_id
       AND cs.sha = pr.head_sha
       AND pr.head_sha <> ''
    WHERE ipr.issue_id = sqlc.arg('issue_id')
    GROUP BY pr.id
)
SELECT
    pr.*,
    COALESCE(ipr.linked_by_type, 'system')::text AS linked_by_type,
    COALESCE(c.total, 0)::bigint   AS checks_total,
    COALESCE(c.passed, 0)::bigint  AS checks_passed,
    COALESCE(c.failed, 0)::bigint  AS checks_failed,
    COALESCE(c.pending, 0)::bigint AS checks_pending
FROM vcs_pull_request pr
JOIN issue_vcs_pull_request ipr ON ipr.pull_request_id = pr.id
LEFT JOIN checks c ON c.pr_id = pr.id
WHERE ipr.issue_id = sqlc.arg('issue_id')
ORDER BY pr.pr_created_at DESC;

-- =====================
-- VCS commit status (CI)
-- =====================

-- name: UpsertVCSCommitStatus :exec
-- One row per (connection, sha, context); a redelivery or state transition
-- overwrites in place. updated_at guards against an older event overwriting a
-- newer one for the same context. state is normalized (passed/failed/pending).
INSERT INTO vcs_commit_status (
    connection_id, sha, context, state, target_url, description, updated_at
) VALUES (
    $1, $2, $3, $4, sqlc.narg('target_url'), sqlc.narg('description'), $5
)
ON CONFLICT (connection_id, sha, context) DO UPDATE SET
    state       = EXCLUDED.state,
    target_url  = EXCLUDED.target_url,
    description = EXCLUDED.description,
    updated_at  = EXCLUDED.updated_at
WHERE EXCLUDED.updated_at >= vcs_commit_status.updated_at;

-- name: ListIssueIDsForVCSPRHead :many
-- Issues linked to any PR whose head sha matches the given status, so a
-- commit-status event can fan out a PR-card refresh to the right issues.
SELECT DISTINCT ipr.issue_id
FROM vcs_pull_request pr
JOIN issue_vcs_pull_request ipr ON ipr.pull_request_id = pr.id
WHERE pr.connection_id = $1 AND pr.head_sha = $2 AND pr.head_sha <> '';

-- =====================
-- Issue ↔ VCS PR link
-- =====================

-- name: LinkIssueToVCSPullRequest :execrows
-- Mirrors LinkIssueToPullRequest: automatic link, 1 only when new.
INSERT INTO issue_vcs_pull_request (
    issue_id, pull_request_id, linked_by_type, linked_by_id
) VALUES (
    $1, $2, 'system', NULL
)
ON CONFLICT (issue_id, pull_request_id) DO NOTHING;

-- name: LinkIssueToVCSPullRequestManually :execrows
INSERT INTO issue_vcs_pull_request (
    issue_id, pull_request_id, linked_by_type, linked_by_id
) VALUES (
    $1, $2, 'member', sqlc.narg('linked_by_id')
)
ON CONFLICT (issue_id, pull_request_id) DO UPDATE SET
    linked_by_type = 'member',
    linked_by_id = EXCLUDED.linked_by_id
WHERE issue_vcs_pull_request.linked_by_type IS DISTINCT FROM 'member';

-- name: ListIssueIDsForVCSPullRequest :many
SELECT issue_id FROM issue_vcs_pull_request
WHERE pull_request_id = $1;

-- name: ListAutoLinkedIssueIDsForVCSPullRequest :many
SELECT issue_id FROM issue_vcs_pull_request
WHERE pull_request_id = $1
  AND COALESCE(linked_by_type, 'system') <> 'member';

-- name: UnlinkIssueFromVCSPullRequest :execrows
DELETE FROM issue_vcs_pull_request
WHERE issue_id = $1 AND pull_request_id = $2;

-- name: GetVCSPullRequestByKey :one
-- The stored row before a webhook upsert, so the handler can tell a merge
-- transition apart from a later event on an already-merged PR.
SELECT * FROM vcs_pull_request
WHERE connection_id = $1 AND repo_owner = $2 AND repo_name = $3 AND pr_number = $4;

-- name: GetVCSPullRequestInWorkspace :one
SELECT * FROM vcs_pull_request
WHERE id = $1 AND workspace_id = $2;
-- name: GetVCSGitLabPullRequestByWakeupIdentity :one
SELECT * FROM vcs_pull_request
WHERE workspace_id = $1
  AND connection_id = $2
  AND provider = 'gitlab'
  AND repo_owner = $3
  AND repo_name = $4
  AND pr_number = $5;

-- name: BeginGitLabVCSPullRequestWakeupSync :one
UPDATE vcs_pull_request
SET wakeup_sync_generation = wakeup_sync_generation + 1,
    status_sync_at = now(),
    status_sync_error = 'MR refresh pending',
    pipeline_sync_at = now(),
    pipeline_sync_error = 'Pipeline refresh pending',
    updated_at = now()
WHERE id = $1 AND workspace_id = $2 AND provider = 'gitlab'
RETURNING wakeup_sync_generation;

-- name: ListVCSGitLabPullRequestsByProjectHead :many
SELECT * FROM vcs_pull_request
WHERE workspace_id = $1
  AND connection_id = $2
  AND provider = 'gitlab'
  AND repo_owner = $3
  AND repo_name = $4
  AND head_sha = $5
  AND head_sha <> ''
ORDER BY id ASC;
-- name: ListVCSGitLabPullRequestsBySourceProjectHead :many
SELECT * FROM vcs_pull_request
WHERE workspace_id = $1
  AND connection_id = $2
  AND provider = 'gitlab'
  AND source_project_id = $3
  AND head_sha = $4
  AND head_sha <> ''
ORDER BY id ASC;


-- name: UpdateGitLabVCSPullRequestWakeupState :execrows
UPDATE vcs_pull_request
SET state = $3,
    head_sha = $4,
    has_conflicts = sqlc.narg('has_conflicts'),
    conflict_head_sha = $5,
    source_project_id = sqlc.narg('source_project_id'),
    source_project_path = sqlc.narg('source_project_path'),
    merged_at = sqlc.narg('merged_at'),
    closed_at = sqlc.narg('closed_at'),
    pr_updated_at = $6,
    status_sync_at = now(),
    status_sync_error = NULL,
    pipeline_sync_at = now(),
    pipeline_sync_error = 'pipeline refresh pending',
    updated_at = now()
WHERE id = $1
  AND workspace_id = $2
  AND provider = 'gitlab'
  AND pr_updated_at <= $6
  AND head_sha = sqlc.arg(expected_head_sha)
  AND pr_updated_at = sqlc.arg(expected_pr_updated_at)
  AND wakeup_sync_generation = $7;

-- name: MarkGitLabVCSPullRequestWakeupSyncError :execrows
UPDATE vcs_pull_request
SET status_sync_at = now(),
    status_sync_error = $3,
    updated_at = now()
WHERE id = $1 AND workspace_id = $2 AND provider = 'gitlab'
  AND wakeup_sync_generation = $4;

-- name: MarkGitLabVCSPullRequestPipelineSync :execrows
UPDATE vcs_pull_request
SET pipeline_sync_at = now(),
    pipeline_sync_error = $3,
    updated_at = now()
WHERE id = $1 AND workspace_id = $2 AND provider = 'gitlab'
  AND wakeup_sync_generation = $4
  AND head_sha = $5;

-- name: MarkGitLabVCSPullRequestPipelineSyncSuccess :execrows
UPDATE vcs_pull_request
SET pipeline_sync_at = now(),
    pipeline_sync_error = NULL,
    updated_at = now()
WHERE id = $1 AND workspace_id = $2 AND provider = 'gitlab'
  AND wakeup_sync_generation = $3
  AND head_sha = $4;

-- name: LockGitLabVCSPullRequestWakeupSync :one
SELECT id FROM vcs_pull_request
WHERE id = $1
  AND workspace_id = $2
  AND provider = 'gitlab'
  AND wakeup_sync_generation = $3
  AND head_sha = $4
  AND pr_updated_at = $5
FOR UPDATE;


-- name: UpsertVCSGitLabPipeline :exec
INSERT INTO vcs_gitlab_pipeline (
    workspace_id, connection_id, pull_request_id, pipeline_id,
    pipeline_project_id, pipeline_project_path, sha, ref, status, web_url,
    pipeline_updated_at
)
SELECT pr.workspace_id, pr.connection_id, pr.id, $3,
       $4, $5, $6, $7, $8, sqlc.narg('web_url'), $9
FROM vcs_pull_request pr
WHERE pr.id = $1 AND pr.workspace_id = $2 AND pr.provider = 'gitlab'
ON CONFLICT (pull_request_id, pipeline_project_path, pipeline_id) DO UPDATE SET
    pipeline_project_id = EXCLUDED.pipeline_project_id,
    pipeline_project_path = EXCLUDED.pipeline_project_path,
    sha = EXCLUDED.sha,
    ref = EXCLUDED.ref,
    status = EXCLUDED.status,
    web_url = EXCLUDED.web_url,
    pipeline_updated_at = EXCLUDED.pipeline_updated_at,
    updated_at = now()
WHERE EXCLUDED.pipeline_updated_at >= vcs_gitlab_pipeline.pipeline_updated_at;

-- name: ListVCSGitLabPipelinesByMR :many
SELECT * FROM vcs_gitlab_pipeline
WHERE workspace_id = $1 AND pull_request_id = $2
ORDER BY pipeline_updated_at DESC, pipeline_id DESC;

-- name: DeleteVCSGitLabPipelinesByMR :execrows
DELETE FROM vcs_gitlab_pipeline
WHERE workspace_id = $1 AND pull_request_id = $2;

-- name: FindVCSPullRequestByURL :one
SELECT * FROM vcs_pull_request
WHERE workspace_id = $1
  AND lower(rtrim(html_url, '/')) = lower(sqlc.arg('html_url')::text)
ORDER BY pr_updated_at DESC
LIMIT 1;
