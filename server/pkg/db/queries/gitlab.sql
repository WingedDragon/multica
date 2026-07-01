-- name: GetGitLabConnectionByWorkspace :one
SELECT * FROM gitlab_connection
WHERE workspace_id = $1
ORDER BY created_at ASC
LIMIT 1;

-- name: ListGitLabConnectionsByWorkspace :many
SELECT * FROM gitlab_connection
WHERE workspace_id = $1
ORDER BY created_at ASC, id ASC;

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
ORDER BY path_with_namespace ASC, id ASC;

-- name: GetGitLabProjectBinding :one
SELECT * FROM gitlab_project_binding
WHERE id = $1 AND workspace_id = $2;

-- name: ListGitLabProjectBindingsByHostAndProjectID :many
SELECT pb.* FROM gitlab_project_binding pb
JOIN gitlab_connection gc
  ON gc.id = pb.connection_id
 AND gc.workspace_id = pb.workspace_id
WHERE gc.host = $1 AND pb.gitlab_project_id = $2
ORDER BY pb.created_at ASC, pb.id ASC;

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
ON CONFLICT (workspace_id, connection_id, gitlab_project_id) DO UPDATE SET
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
ON CONFLICT (workspace_id, project_binding_id, mr_iid) DO UPDATE SET
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

-- name: GetGitLabMergeRequestByBinding :one
SELECT * FROM gitlab_merge_request
WHERE workspace_id = $1 AND project_binding_id = $2 AND mr_iid = $3;

-- name: ListGitLabMergeRequestsByIssue :many
WITH issue_mrs AS (
    SELECT mr.id, mr.sha
    FROM gitlab_merge_request mr
    JOIN issue_gitlab_merge_request imr
      ON imr.merge_request_id = mr.id
     AND imr.workspace_id = mr.workspace_id
    WHERE imr.workspace_id = sqlc.arg('workspace_id')
      AND imr.issue_id = sqlc.arg('issue_id')
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
JOIN issue_gitlab_merge_request imr
  ON imr.merge_request_id = mr.id
 AND imr.workspace_id = mr.workspace_id
JOIN gitlab_project_binding pb
  ON pb.id = mr.project_binding_id
 AND pb.workspace_id = mr.workspace_id
LEFT JOIN latest_pipeline lp ON lp.merge_request_id = mr.id
WHERE imr.workspace_id = sqlc.arg('workspace_id')
  AND imr.issue_id = sqlc.arg('issue_id')
ORDER BY mr.mr_created_at DESC, mr.id ASC;

-- name: ListGitLabMergeRequestsByProjectBindingSHA :many
SELECT * FROM gitlab_merge_request
WHERE workspace_id = $1 AND project_binding_id = $2 AND sha = $3
ORDER BY mr_updated_at DESC, id ASC;

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
    workspace_id, issue_id, merge_request_id, linked_by_type, linked_by_id, close_intent
) VALUES (
    $1, $2, $3, sqlc.narg('linked_by_type'), sqlc.narg('linked_by_id'), $4
)
ON CONFLICT (issue_id, merge_request_id) DO UPDATE SET
    close_intent = CASE
        WHEN sqlc.arg('preserve_close_intent') THEN issue_gitlab_merge_request.close_intent
        ELSE EXCLUDED.close_intent
    END;

-- name: ListIssueIDsForGitLabMergeRequest :many
SELECT issue_id FROM issue_gitlab_merge_request
WHERE workspace_id = $1 AND merge_request_id = $2
ORDER BY issue_id ASC;

-- name: GetIssueGitLabMRCloseAggregate :one
SELECT
    COALESCE(SUM(CASE WHEN mr.state IN ('open', 'draft') THEN 1 ELSE 0 END), 0)::bigint AS open_count,
    COALESCE(SUM(CASE WHEN mr.state = 'merged' AND imr.close_intent THEN 1 ELSE 0 END), 0)::bigint AS merged_with_close_intent_count
FROM gitlab_merge_request mr
JOIN issue_gitlab_merge_request imr
  ON imr.merge_request_id = mr.id
 AND imr.workspace_id = mr.workspace_id
WHERE imr.workspace_id = $1 AND imr.issue_id = $2;
