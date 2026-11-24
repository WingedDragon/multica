-- GitLab projects are workspace-scoped extensions of canonical VCS connections.
-- Every runtime read validates the connection owner and provider; legacy GitLab
-- tables are used only by the explicit cutover tooling.

-- name: ListVCSGitLabProjectsByWorkspace :many
SELECT p.*
FROM vcs_gitlab_project p
JOIN vcs_connection c
  ON c.id = p.connection_id
 AND c.workspace_id = p.workspace_id
 AND c.provider = 'gitlab'
WHERE p.workspace_id = sqlc.arg('workspace_id')
ORDER BY p.path_with_namespace ASC, p.id ASC;
-- name: GetVCSGitLabConnectionByIDAndWorkspace :one
SELECT * FROM vcs_connection
WHERE id = sqlc.arg('id')
  AND workspace_id = sqlc.arg('workspace_id')
  AND provider = 'gitlab';


-- name: GetVCSGitLabProjectByIDAndWorkspace :one
SELECT p.*
FROM vcs_gitlab_project p
JOIN vcs_connection c
  ON c.id = p.connection_id
 AND c.workspace_id = p.workspace_id
 AND c.provider = 'gitlab'
WHERE p.id = sqlc.arg('id')
  AND p.workspace_id = sqlc.arg('workspace_id');

-- name: GetVCSGitLabProjectByRemoteID :one
SELECT p.*
FROM vcs_gitlab_project p
JOIN vcs_connection c
  ON c.id = p.connection_id
 AND c.workspace_id = p.workspace_id
 AND c.provider = 'gitlab'
WHERE p.workspace_id = sqlc.arg('workspace_id')
  AND p.connection_id = sqlc.arg('connection_id')
  AND p.gitlab_project_id = sqlc.arg('gitlab_project_id');


-- name: UpsertVCSGitLabProject :one
INSERT INTO vcs_gitlab_project (
    workspace_id, connection_id, gitlab_project_id, path_with_namespace, web_url,
    hook_id, hook_enabled, last_sync_error
) VALUES (
    sqlc.arg('workspace_id'), sqlc.arg('connection_id'), sqlc.arg('gitlab_project_id'),
    sqlc.arg('path_with_namespace'), sqlc.arg('web_url'), sqlc.narg('hook_id'),
    sqlc.arg('hook_enabled'), sqlc.narg('last_sync_error')
)
ON CONFLICT (workspace_id, connection_id, gitlab_project_id) DO UPDATE SET
    path_with_namespace = EXCLUDED.path_with_namespace,
    web_url = EXCLUDED.web_url,
    hook_id = EXCLUDED.hook_id,
    hook_enabled = EXCLUDED.hook_enabled,
    last_sync_error = EXCLUDED.last_sync_error,
    updated_at = now()
RETURNING *;

-- name: DeleteVCSGitLabProjectByID :one
DELETE FROM vcs_gitlab_project p
WHERE p.id = sqlc.arg('id')
  AND p.workspace_id = sqlc.arg('workspace_id')
  AND p.connection_id = sqlc.arg('connection_id')
  AND EXISTS (
      SELECT 1 FROM vcs_connection c
      WHERE c.id = p.connection_id
        AND c.workspace_id = p.workspace_id
        AND c.provider = 'gitlab'
  )
RETURNING p.*;

-- name: MarkVCSGitLabProjectRefreshStarted :exec
UPDATE vcs_gitlab_project
SET refresh_in_progress_at = now(),
    last_refresh_error = NULL,
    updated_at = now()
WHERE id = sqlc.arg('id')
  AND workspace_id = sqlc.arg('workspace_id')
  AND connection_id = sqlc.arg('connection_id');

-- name: MarkVCSGitLabProjectRefreshFinished :exec
UPDATE vcs_gitlab_project
SET last_refresh_at = now(),
    refresh_in_progress_at = NULL,
    last_refresh_error = sqlc.narg('last_refresh_error'),
    updated_at = now()
WHERE id = sqlc.arg('id')
  AND workspace_id = sqlc.arg('workspace_id')
  AND connection_id = sqlc.arg('connection_id');

-- name: ListVCSGitLabPullRequestsByProject :many
SELECT pr.*
FROM vcs_pull_request pr
WHERE pr.workspace_id = sqlc.arg('workspace_id')
  AND pr.connection_id = sqlc.arg('connection_id')
  AND pr.provider = 'gitlab'
  AND pr.repo_owner = sqlc.arg('repo_owner')
  AND pr.repo_name = sqlc.arg('repo_name')
ORDER BY pr.pr_updated_at DESC, pr.id ASC;

-- A published legacy connection UUID can differ from its canonical connection
-- ID after migration. Resolve only through the owning workspace and kind.
-- name: GetGitLabVCSConnectionIDByLegacyID :one
SELECT unified_id
FROM vcs_gitlab_migration_id_map
WHERE workspace_id = sqlc.arg('workspace_id')
  AND entity_kind = 'connection'
  AND legacy_id = sqlc.arg('legacy_id');


-- Remove project-scoped GitLab provenance and data in the same transaction as
-- deleting the project row. Official VCS links (vcs_linked) survive; a PR with
-- any such link keeps its canonical state and GitLab enrichment.
-- name: DeleteVCSGitLabProjectOrphanedPullRequests :exec
WITH target_project AS MATERIALIZED (
    SELECT project.workspace_id, project.connection_id, project.path_with_namespace
    FROM vcs_gitlab_project project
    JOIN vcs_connection conn
      ON conn.id = project.connection_id
     AND conn.workspace_id = project.workspace_id
     AND conn.provider = 'gitlab'
    WHERE project.id = sqlc.arg('project_id')
      AND project.workspace_id = sqlc.arg('workspace_id')
),
target_prs AS MATERIALIZED (
    SELECT pr.id, pr.workspace_id
    FROM vcs_pull_request pr
    JOIN target_project project
      ON pr.workspace_id = project.workspace_id
     AND pr.connection_id = project.connection_id
     AND pr.provider = 'gitlab'
     AND pr.repo_owner || '/' || pr.repo_name = project.path_with_namespace
),
deleted_gitlab_only_links AS (
    DELETE FROM issue_vcs_pull_request ipr
    USING target_prs pr, issue i
    WHERE ipr.pull_request_id = pr.id
      AND i.id = ipr.issue_id
      AND i.workspace_id = pr.workspace_id
      AND NOT ipr.vcs_linked
    RETURNING 1
),
cleared_gitlab_provenance AS (
    UPDATE issue_vcs_pull_request ipr
    SET gitlab_close_intent = FALSE,
        gitlab_linked_by_type = NULL,
        gitlab_linked_by_id = NULL,
        gitlab_linked_at = NULL
    FROM target_prs pr, issue i
    WHERE ipr.pull_request_id = pr.id
      AND i.id = ipr.issue_id
      AND i.workspace_id = pr.workspace_id
      AND ipr.vcs_linked
      AND ipr.gitlab_linked_at IS NOT NULL
    RETURNING 1
),
orphaned_prs AS MATERIALIZED (
    SELECT pr.id
    FROM target_prs pr
    WHERE NOT EXISTS (
        SELECT 1
        FROM issue_vcs_pull_request ipr
        JOIN issue i ON i.id = ipr.issue_id
        WHERE ipr.pull_request_id = pr.id
          AND i.workspace_id = pr.workspace_id
          AND ipr.vcs_linked
    )
      AND NOT EXISTS (
        SELECT 1
        FROM issue_vcs_pull_request ipr
        WHERE ipr.pull_request_id = pr.id
          AND NOT EXISTS (
              SELECT 1 FROM issue i
              WHERE i.id = ipr.issue_id
                AND i.workspace_id = pr.workspace_id
          )
    )
),
deleted_notes AS (
    DELETE FROM gitlab_mr_note note
    USING orphaned_prs pr
    WHERE note.workspace_id = sqlc.arg('workspace_id')
      AND note.vcs_pull_request_id = pr.id
    RETURNING note.id
),
deleted_discussions AS (
    DELETE FROM gitlab_mr_discussion discussion
    USING orphaned_prs pr
    WHERE discussion.workspace_id = sqlc.arg('workspace_id')
      AND discussion.vcs_pull_request_id = pr.id
      AND (SELECT COUNT(*) FROM deleted_notes) >= 0
    RETURNING discussion.id
),
deleted_approvals AS (
    DELETE FROM gitlab_mr_approval_state approval
    USING orphaned_prs pr
    WHERE approval.workspace_id = sqlc.arg('workspace_id')
      AND approval.vcs_pull_request_id = pr.id
      AND (SELECT COUNT(*) FROM deleted_discussions) >= 0
    RETURNING approval.vcs_pull_request_id
),
deleted_jobs AS (
    DELETE FROM gitlab_pipeline_job job
    USING orphaned_prs pr
    WHERE job.workspace_id = sqlc.arg('workspace_id')
      AND job.vcs_pull_request_id = pr.id
      AND (SELECT COUNT(*) FROM deleted_approvals) >= 0
    RETURNING job.vcs_pull_request_id
),
deleted_pipelines AS (
    DELETE FROM vcs_gitlab_pipeline pipeline
    USING orphaned_prs pr
    WHERE pipeline.workspace_id = sqlc.arg('workspace_id')
      AND pipeline.pull_request_id = pr.id
      AND (SELECT COUNT(*) FROM deleted_jobs) >= 0
    RETURNING pipeline.pull_request_id
),
deleted_details AS (
    DELETE FROM vcs_gitlab_mr_detail detail
    USING orphaned_prs pr
    WHERE detail.vcs_pull_request_id = pr.id
      AND (SELECT COUNT(*) FROM deleted_pipelines) >= 0
    RETURNING detail.vcs_pull_request_id
),
deleted_aliases AS (
    DELETE FROM vcs_gitlab_migration_id_map alias
    USING orphaned_prs pr
    WHERE alias.workspace_id = sqlc.arg('workspace_id')
      AND alias.entity_kind = 'pull_request'
      AND alias.unified_id = pr.id
      AND (SELECT COUNT(*) FROM deleted_details) >= 0
    RETURNING alias.unified_id
)
DELETE FROM vcs_pull_request pr
USING orphaned_prs orphan
WHERE pr.id = orphan.id
  AND pr.workspace_id = sqlc.arg('workspace_id')
  AND (SELECT COUNT(*) FROM deleted_gitlab_only_links) >= 0
  AND (SELECT COUNT(*) FROM cleared_gitlab_provenance) >= 0
  AND (SELECT COUNT(*) FROM deleted_aliases) >= 0;

-- Connection removal is broader than project removal: no VCS source can
-- survive a deleted connection. Its caller runs this query and
-- DeleteVCSConnection in one transaction.
-- name: DeleteVCSGitLabConnectionArtifacts :exec
WITH target_connection AS MATERIALIZED (
    SELECT id
    FROM vcs_connection
    WHERE id = sqlc.arg('connection_id')
      AND workspace_id = sqlc.arg('workspace_id')
),
target_prs AS MATERIALIZED (
    SELECT pr.id
    FROM vcs_pull_request pr
    JOIN target_connection conn
      ON conn.id = pr.connection_id
),
deleted_notes AS (
    DELETE FROM gitlab_mr_note note
    USING target_prs pr
    WHERE note.workspace_id = sqlc.arg('workspace_id')
      AND note.vcs_pull_request_id = pr.id
    RETURNING note.id
),
deleted_discussions AS (
    DELETE FROM gitlab_mr_discussion discussion
    USING target_prs pr
    WHERE discussion.workspace_id = sqlc.arg('workspace_id')
      AND discussion.vcs_pull_request_id = pr.id
      AND (SELECT COUNT(*) FROM deleted_notes) >= 0
    RETURNING discussion.id
),
deleted_approvals AS (
    DELETE FROM gitlab_mr_approval_state approval
    USING target_prs pr
    WHERE approval.workspace_id = sqlc.arg('workspace_id')
      AND approval.vcs_pull_request_id = pr.id
      AND (SELECT COUNT(*) FROM deleted_discussions) >= 0
    RETURNING approval.vcs_pull_request_id
),
deleted_jobs AS (
    DELETE FROM gitlab_pipeline_job job
    USING target_prs pr
    WHERE job.workspace_id = sqlc.arg('workspace_id')
      AND job.vcs_pull_request_id = pr.id
      AND (SELECT COUNT(*) FROM deleted_approvals) >= 0
    RETURNING job.vcs_pull_request_id
),
deleted_pipelines AS (
    DELETE FROM vcs_gitlab_pipeline pipeline
    WHERE pipeline.workspace_id = sqlc.arg('workspace_id')
      AND pipeline.connection_id = sqlc.arg('connection_id')
      AND (SELECT COUNT(*) FROM deleted_jobs) >= 0
    RETURNING pipeline.pull_request_id
),
deleted_details AS (
    DELETE FROM vcs_gitlab_mr_detail detail
    USING target_prs pr
    WHERE detail.vcs_pull_request_id = pr.id
      AND (SELECT COUNT(*) FROM deleted_pipelines) >= 0
    RETURNING detail.vcs_pull_request_id
),
deleted_aliases AS (
    DELETE FROM vcs_gitlab_migration_id_map alias
    WHERE alias.workspace_id = sqlc.arg('workspace_id')
      AND (
          (alias.entity_kind = 'connection'
           AND alias.unified_id = sqlc.arg('connection_id'))
          OR
          (alias.entity_kind = 'pull_request'
           AND alias.unified_id IN (SELECT id FROM target_prs))
      )
      AND (SELECT COUNT(*) FROM deleted_details) >= 0
    RETURNING alias.unified_id
)
DELETE FROM vcs_gitlab_project project
WHERE project.workspace_id = sqlc.arg('workspace_id')
  AND project.connection_id = sqlc.arg('connection_id')
  AND (SELECT COUNT(*) FROM deleted_aliases) >= 0;


-- Only connections with an explicit migration/legacy-API alias are visible to
-- the legacy GitLab project API.
-- name: ListLegacyGitLabVCSConnectionsByWorkspace :many
SELECT c.*
FROM vcs_connection c
WHERE c.workspace_id = sqlc.arg('workspace_id')
  AND c.provider = 'gitlab'
  AND EXISTS (
      SELECT 1
      FROM vcs_gitlab_migration_id_map m
      WHERE m.workspace_id = c.workspace_id
        AND m.entity_kind = 'connection'
        AND m.unified_id = c.id
  )
ORDER BY c.created_at ASC, c.id ASC;

-- Accept a canonical ID in a legacy request only when that exact connection
-- has explicit legacy ownership in this workspace.
-- name: GetLegacyGitLabVCSConnectionIDByCanonicalID :one
SELECT c.id
FROM vcs_connection c
WHERE c.id = sqlc.arg('connection_id')
  AND c.workspace_id = sqlc.arg('workspace_id')
  AND c.provider = 'gitlab'
  AND EXISTS (
      SELECT 1
      FROM vcs_gitlab_migration_id_map m
      WHERE m.workspace_id = c.workspace_id
        AND m.entity_kind = 'connection'
        AND m.unified_id = c.id
  );

-- The old explicit-token connection endpoint marks its own connection as
-- legacy-owned. Reconnecting an already migrated connection leaves its
-- existing legacy alias untouched.
-- name: EnsureLegacyGitLabVCSConnectionAlias :exec
INSERT INTO vcs_gitlab_migration_id_map (workspace_id, entity_kind, legacy_id, unified_id)
SELECT c.workspace_id, 'connection', c.id, c.id
FROM vcs_connection c
WHERE c.id = sqlc.arg('connection_id')
  AND c.workspace_id = sqlc.arg('workspace_id')
  AND c.provider = 'gitlab'
  AND NOT EXISTS (
      SELECT 1
      FROM vcs_gitlab_migration_id_map m
      WHERE m.workspace_id = c.workspace_id
        AND m.entity_kind = 'connection'
        AND m.unified_id = c.id
  )
ON CONFLICT DO NOTHING;
