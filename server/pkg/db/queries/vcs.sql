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
WHERE EXCLUDED.provider <> 'gitlab' OR EXCLUDED.pr_updated_at > vcs_pull_request.pr_updated_at
RETURNING *;

-- name: ListVCSPullRequestsByIssue :many
-- Aggregates current-head commit statuses and GitLab Pipelines into check counts.
-- Old-head rows stay stored but cannot pollute the current PR card.
WITH current_checks AS (
    SELECT pr.id AS pr_id, cs.state
    FROM vcs_pull_request pr
    JOIN issue_vcs_pull_request ipr ON ipr.pull_request_id = pr.id
    JOIN issue i ON i.id = ipr.issue_id AND i.workspace_id = pr.workspace_id
    JOIN vcs_commit_status cs
      ON cs.connection_id = pr.connection_id
     AND cs.sha = pr.head_sha
     AND pr.head_sha <> ''
    WHERE i.id = sqlc.arg('issue_id')
    UNION ALL
    SELECT pr.id AS pr_id, pipeline.status AS state
    FROM vcs_pull_request pr
    JOIN issue_vcs_pull_request ipr ON ipr.pull_request_id = pr.id
    JOIN issue i ON i.id = ipr.issue_id AND i.workspace_id = pr.workspace_id
    JOIN vcs_gitlab_pipeline pipeline
      ON pipeline.workspace_id = pr.workspace_id
     AND pipeline.connection_id = pr.connection_id
     AND pipeline.pull_request_id = pr.id
     AND pipeline.sha = pr.head_sha
    WHERE i.id = sqlc.arg('issue_id')
      AND pr.provider = 'gitlab'
      AND pr.head_sha <> ''
),
checks AS (
    SELECT
        pr_id,
        COUNT(*)::bigint AS total,
        COUNT(*) FILTER (WHERE state = 'failed')::bigint AS failed,
        COUNT(*) FILTER (WHERE state = 'passed')::bigint AS passed,
        COUNT(*) FILTER (WHERE state = 'pending')::bigint AS pending
    FROM current_checks
    GROUP BY pr_id
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
JOIN issue i ON i.id = ipr.issue_id AND i.workspace_id = pr.workspace_id
LEFT JOIN checks c ON c.pr_id = pr.id
WHERE i.id = sqlc.arg('issue_id')
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
JOIN issue i ON i.id = ipr.issue_id AND i.workspace_id = pr.workspace_id
WHERE pr.connection_id = $1 AND pr.head_sha = $2 AND pr.head_sha <> '';

-- =====================
-- Issue ↔ VCS PR link
-- =====================

-- name: LinkIssueToVCSPullRequest :execrows
-- Mirrors LinkIssueToPullRequest: automatic link, 1 only when new. The issue
-- and canonical PR must belong to the same workspace.
INSERT INTO issue_vcs_pull_request (
    issue_id, pull_request_id, linked_by_type, linked_by_id, vcs_linked
)
SELECT i.id, pr.id, 'system', NULL, TRUE
FROM issue i
JOIN vcs_pull_request pr ON pr.id = sqlc.arg('pull_request_id') AND pr.workspace_id = i.workspace_id
WHERE i.id = sqlc.arg('issue_id')
ON CONFLICT (issue_id, pull_request_id) DO UPDATE SET
    linked_by_type = 'system',
    linked_by_id = NULL,
    vcs_linked = TRUE
WHERE NOT issue_vcs_pull_request.vcs_linked;

-- name: LinkGitLabIssueToVCSPullRequest :execrows
-- Legacy-policy GitLab links carry close-intent provenance independently from
-- official VCS links; an existing official link remains independently linked.
INSERT INTO issue_vcs_pull_request (
    issue_id, pull_request_id, linked_by_type, linked_by_id, vcs_linked,
    gitlab_close_intent, gitlab_linked_by_type, gitlab_linked_by_id, gitlab_linked_at
)
SELECT i.id, pr.id, NULL, NULL, FALSE,
    sqlc.arg('gitlab_close_intent'), 'system', NULL, now()
FROM issue i
JOIN vcs_pull_request pr ON pr.id = sqlc.arg('pull_request_id') AND pr.workspace_id = i.workspace_id AND pr.provider = 'gitlab'
WHERE i.id = sqlc.arg('issue_id')
ON CONFLICT (issue_id, pull_request_id) DO UPDATE SET
    gitlab_close_intent = CASE WHEN sqlc.arg('preserve_close_intent')
        THEN issue_vcs_pull_request.gitlab_close_intent ELSE EXCLUDED.gitlab_close_intent END,
    gitlab_linked_by_type = COALESCE(issue_vcs_pull_request.gitlab_linked_by_type, EXCLUDED.gitlab_linked_by_type),
    gitlab_linked_by_id = COALESCE(issue_vcs_pull_request.gitlab_linked_by_id, EXCLUDED.gitlab_linked_by_id),
    gitlab_linked_at = COALESCE(issue_vcs_pull_request.gitlab_linked_at, EXCLUDED.gitlab_linked_at)
WHERE (CASE WHEN sqlc.arg('preserve_close_intent')
        THEN issue_vcs_pull_request.gitlab_close_intent ELSE EXCLUDED.gitlab_close_intent END)
        IS DISTINCT FROM issue_vcs_pull_request.gitlab_close_intent
   OR issue_vcs_pull_request.gitlab_linked_at IS NULL;

-- name: LinkIssueToVCSPullRequestManually :execrows
INSERT INTO issue_vcs_pull_request (
    issue_id, pull_request_id, linked_by_type, linked_by_id, vcs_linked
)
SELECT i.id, pr.id, 'member', sqlc.narg('linked_by_id'), TRUE
FROM issue i
JOIN vcs_pull_request pr ON pr.id = sqlc.arg('pull_request_id') AND pr.workspace_id = i.workspace_id
WHERE i.id = sqlc.arg('issue_id')
ON CONFLICT (issue_id, pull_request_id) DO UPDATE SET
    linked_by_type = 'member',
    linked_by_id = EXCLUDED.linked_by_id,
    vcs_linked = TRUE
WHERE issue_vcs_pull_request.linked_by_type IS DISTINCT FROM 'member'
   OR NOT issue_vcs_pull_request.vcs_linked;

-- name: ListIssueIDsForVCSPullRequest :many
SELECT ipr.issue_id
FROM issue_vcs_pull_request ipr
JOIN issue i ON i.id = ipr.issue_id
JOIN vcs_pull_request pr ON pr.id = ipr.pull_request_id AND pr.workspace_id = i.workspace_id
WHERE ipr.pull_request_id = $1;

-- name: ListAutoLinkedIssueIDsForVCSPullRequest :many
SELECT ipr.issue_id
FROM issue_vcs_pull_request ipr
JOIN issue i ON i.id = ipr.issue_id
JOIN vcs_pull_request pr ON pr.id = ipr.pull_request_id AND pr.workspace_id = i.workspace_id
WHERE ipr.pull_request_id = $1
  AND ipr.vcs_linked
  AND COALESCE(ipr.linked_by_type, 'system') <> 'member';

-- name: UnlinkIssueFromVCSPullRequest :one
WITH preserved_gitlab_links AS (
    UPDATE issue_vcs_pull_request ipr
    SET vcs_linked = FALSE, linked_by_type = NULL, linked_by_id = NULL
    WHERE ipr.issue_id = $1 AND ipr.pull_request_id = $2
      AND ipr.vcs_linked AND ipr.gitlab_linked_at IS NOT NULL
      AND EXISTS (
          SELECT 1 FROM issue i
          JOIN vcs_pull_request pr ON pr.id = ipr.pull_request_id AND pr.workspace_id = i.workspace_id
          WHERE i.id = ipr.issue_id
      )
    RETURNING 1
), deleted_vcs_links AS (
    DELETE FROM issue_vcs_pull_request ipr
    WHERE ipr.issue_id = $1 AND ipr.pull_request_id = $2
      AND ipr.vcs_linked AND ipr.gitlab_linked_at IS NULL
      AND EXISTS (
          SELECT 1 FROM issue i
          JOIN vcs_pull_request pr ON pr.id = ipr.pull_request_id AND pr.workspace_id = i.workspace_id
          WHERE i.id = ipr.issue_id
      )
    RETURNING 1
)
SELECT (SELECT count(*) FROM preserved_gitlab_links)
     + (SELECT count(*) FROM deleted_vcs_links);

-- name: ListAutoLinkedGitLabIssueIDsForVCSPullRequest :many
SELECT ipr.issue_id
FROM issue_vcs_pull_request ipr
JOIN issue i ON i.id = ipr.issue_id
JOIN vcs_pull_request pr ON pr.id = ipr.pull_request_id AND pr.workspace_id = i.workspace_id
WHERE ipr.pull_request_id = $1
  AND ipr.gitlab_linked_at IS NOT NULL
  AND COALESCE(ipr.gitlab_linked_by_type, 'system') <> 'member';

-- name: UnlinkGitLabIssueFromVCSPullRequest :one
WITH cleared_legacy_provenance AS (
    UPDATE issue_vcs_pull_request ipr
    SET gitlab_close_intent = FALSE,
        gitlab_linked_by_type = NULL,
        gitlab_linked_by_id = NULL,
        gitlab_linked_at = NULL
    WHERE ipr.issue_id = $1 AND ipr.pull_request_id = $2
      AND ipr.vcs_linked AND ipr.gitlab_linked_at IS NOT NULL
      AND EXISTS (
          SELECT 1 FROM issue i
          JOIN vcs_pull_request pr ON pr.id = ipr.pull_request_id AND pr.workspace_id = i.workspace_id
          WHERE i.id = ipr.issue_id
      )
    RETURNING 1
), deleted_legacy_only_links AS (
    DELETE FROM issue_vcs_pull_request ipr
    WHERE ipr.issue_id = $1 AND ipr.pull_request_id = $2
      AND NOT ipr.vcs_linked AND ipr.gitlab_linked_at IS NOT NULL
      AND EXISTS (
          SELECT 1 FROM issue i
          JOIN vcs_pull_request pr ON pr.id = ipr.pull_request_id AND pr.workspace_id = i.workspace_id
          WHERE i.id = ipr.issue_id
      )
    RETURNING 1
)
SELECT (SELECT count(*) FROM cleared_legacy_provenance)
     + (SELECT count(*) FROM deleted_legacy_only_links);

-- name: GetGitLabVCSIssueCloseAggregate :one
WITH linked_prs AS (
    SELECT pr.state, FALSE AS legacy_gitlab, FALSE AS close_intent
    FROM issue_pull_request ipr
    JOIN issue i ON i.id = ipr.issue_id
    JOIN github_pull_request pr ON pr.id = ipr.pull_request_id AND pr.workspace_id = i.workspace_id
    WHERE i.workspace_id = sqlc.arg('workspace_id')
      AND i.id = sqlc.arg('issue_id')
    UNION ALL
    SELECT pr.state,
           (pr.provider = 'gitlab' AND ivpr.gitlab_linked_at IS NOT NULL AND COALESCE(gp.legacy_link_policy, FALSE)),
           COALESCE(ivpr.gitlab_close_intent, FALSE)
    FROM issue_vcs_pull_request ivpr
    JOIN issue i ON i.id = ivpr.issue_id
    JOIN vcs_pull_request pr ON pr.id = ivpr.pull_request_id AND pr.workspace_id = i.workspace_id
    LEFT JOIN vcs_gitlab_project gp
      ON gp.workspace_id = pr.workspace_id
     AND gp.connection_id = pr.connection_id
     AND gp.path_with_namespace = pr.repo_owner || '/' || pr.repo_name
    WHERE i.workspace_id = sqlc.arg('workspace_id')
      AND i.id = sqlc.arg('issue_id')
      AND (ivpr.vcs_linked OR (pr.provider = 'gitlab' AND gp.legacy_link_policy AND ivpr.gitlab_linked_at IS NOT NULL))
)
SELECT
    COUNT(*) FILTER (WHERE state <> 'merged')::bigint AS unmerged_count,
    COUNT(*) FILTER (WHERE legacy_gitlab AND state = 'merged' AND close_intent)::bigint AS merged_with_close_intent_count
FROM linked_prs;

-- name: CompleteIssueFromGitLabCloseIntent :one
UPDATE issue AS i SET
    status = 'done',
    duplicate_of_issue_id = NULL,
    position = (
        SELECT COALESCE(MIN(target.position), 0) - 1
        FROM issue AS target
        WHERE target.workspace_id = i.workspace_id AND target.status = 'done'
    ),
    revision = i.revision + 1,
    last_activity_at = GREATEST(COALESCE(i.last_activity_at, i.updated_at), now()),
    updated_at = now()
WHERE i.id = sqlc.arg('issue_id')
  AND i.workspace_id = sqlc.arg('workspace_id')
  AND i.status <> 'done'
  AND EXISTS (
      SELECT 1
      FROM issue_vcs_pull_request ivpr
      JOIN vcs_pull_request pr ON pr.id = ivpr.pull_request_id
      WHERE ivpr.issue_id = i.id
        AND pr.workspace_id = i.workspace_id
        AND pr.provider = 'gitlab'
        AND ivpr.gitlab_linked_at IS NOT NULL
        AND ivpr.gitlab_close_intent
        AND pr.state = 'merged'
        AND EXISTS (
            SELECT 1
            FROM vcs_gitlab_project gp
            WHERE gp.workspace_id = i.workspace_id
              AND gp.connection_id = pr.connection_id
              AND gp.path_with_namespace = pr.repo_owner || '/' || pr.repo_name
              AND gp.legacy_link_policy
        )
  )
  AND NOT EXISTS (
      SELECT 1
      FROM issue_pull_request ipr
      JOIN github_pull_request pr ON pr.id = ipr.pull_request_id AND pr.workspace_id = i.workspace_id
      WHERE ipr.issue_id = i.id
        AND pr.state <> 'merged'
      UNION ALL
      SELECT 1
      FROM issue_vcs_pull_request ivpr
      JOIN vcs_pull_request pr ON pr.id = ivpr.pull_request_id AND pr.workspace_id = i.workspace_id
      LEFT JOIN vcs_gitlab_project gp
        ON gp.workspace_id = pr.workspace_id
       AND gp.connection_id = pr.connection_id
       AND gp.path_with_namespace = pr.repo_owner || '/' || pr.repo_name
      WHERE ivpr.issue_id = i.id
        AND (ivpr.vcs_linked OR (pr.provider = 'gitlab' AND gp.legacy_link_policy AND ivpr.gitlab_linked_at IS NOT NULL))
        AND pr.state <> 'merged'
  )
RETURNING i.*;

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

-- name: GetVCSGitLabPipelineByMRIdentity :one
SELECT * FROM vcs_gitlab_pipeline
WHERE workspace_id = $1
  AND pull_request_id = $2
  AND pipeline_project_path = $3
  AND pipeline_id = $4;

-- name: IsGitLabPipelineJobEventDuplicate :one
SELECT EXISTS (
    SELECT 1
    FROM gitlab_pipeline_job job
    JOIN vcs_pull_request pr ON pr.id = job.vcs_pull_request_id
    WHERE pr.id = sqlc.arg('pull_request_id')
      AND pr.workspace_id = sqlc.arg('workspace_id')
      AND pr.provider = 'gitlab'
      AND job.workspace_id = pr.workspace_id
      AND job.job_id = sqlc.arg('job_id')
      AND job.pipeline_id = sqlc.arg('pipeline_id')
      AND job.name = sqlc.arg('name')
      AND job.stage IS NOT DISTINCT FROM sqlc.narg('stage')
      AND job.status = sqlc.arg('status')
      AND job.ref IS NOT DISTINCT FROM sqlc.narg('ref')
      AND job.sha IS NOT DISTINCT FROM sqlc.narg('sha')
      AND job.started_at IS NOT DISTINCT FROM sqlc.narg('started_at')
      AND job.finished_at IS NOT DISTINCT FROM sqlc.narg('finished_at')
      AND job.failure_reason IS NOT DISTINCT FROM sqlc.narg('failure_reason')
      AND job.allow_failure = sqlc.arg('allow_failure')
)::boolean AS duplicate;

-- name: IsGitLabMRNoteEventDuplicate :one
SELECT EXISTS (
    SELECT 1
    FROM gitlab_mr_note note
    JOIN vcs_pull_request pr ON pr.id = note.vcs_pull_request_id
    WHERE pr.id = sqlc.arg('pull_request_id')
      AND pr.workspace_id = sqlc.arg('workspace_id')
      AND pr.provider = 'gitlab'
      AND note.gitlab_note_id = sqlc.arg('note_id')
      AND note.body = sqlc.arg('note_body')
      AND note.note_updated_at IS NOT DISTINCT FROM sqlc.narg('note_updated_at')
)::boolean AS duplicate;

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
