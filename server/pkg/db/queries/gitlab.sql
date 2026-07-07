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

-- name: MarkGitLabProjectBindingEvent :exec
UPDATE gitlab_project_binding
SET last_event_at = now(),
    last_event_type = $3,
    updated_at = now()
WHERE id = $1 AND workspace_id = $2;

-- name: MarkGitLabProjectRefreshStarted :exec
UPDATE gitlab_project_binding
SET refresh_in_progress_at = now(),
    last_refresh_error = NULL,
    updated_at = now()
WHERE id = $1 AND workspace_id = $2;

-- name: MarkGitLabProjectRefreshFinished :exec
UPDATE gitlab_project_binding
SET last_refresh_at = now(),
    refresh_in_progress_at = NULL,
    last_refresh_error = sqlc.narg('last_refresh_error'),
    updated_at = now()
WHERE id = $1 AND workspace_id = $2;

-- name: GetGitLabMergeRequestByID :one
SELECT * FROM gitlab_merge_request
WHERE id = $1 AND workspace_id = $2;

-- name: ListGitLabMergeRequestsByProjectBinding :many
SELECT * FROM gitlab_merge_request
WHERE workspace_id = $1 AND project_binding_id = $2
ORDER BY mr_updated_at DESC, id ASC
LIMIT $3;

-- name: UpdateGitLabMergeRequestEnrichment :one
UPDATE gitlab_merge_request
SET title = $3,
    description = sqlc.narg('description'),
    state = $4,
    web_url = $5,
    source_branch = sqlc.narg('source_branch'),
    target_branch = sqlc.narg('target_branch'),
    author_username = sqlc.narg('author_username'),
    author_avatar_url = sqlc.narg('author_avatar_url'),
    sha = $6,
    merge_commit_sha = sqlc.narg('merge_commit_sha'),
    detailed_merge_status = sqlc.narg('detailed_merge_status'),
    has_conflicts = sqlc.narg('has_conflicts'),
    additions = $7,
    deletions = $8,
    changed_files = $9,
    reviewers = $10,
    assignees = $11,
    labels = $12,
    mr_updated_at = $13,
    merged_at = sqlc.narg('merged_at'),
    closed_at = sqlc.narg('closed_at'),
    last_refreshed_at = now(),
    last_refresh_error = NULL,
    updated_at = now()
WHERE id = $1 AND workspace_id = $2
RETURNING *;

-- name: MarkGitLabMergeRequestRefreshError :exec
UPDATE gitlab_merge_request
SET last_refreshed_at = now(),
    last_refresh_error = $3,
    updated_at = now()
WHERE id = $1 AND workspace_id = $2;

-- name: UpsertGitLabMRApprovalState :one
INSERT INTO gitlab_mr_approval_state (
    merge_request_id, workspace_id, approved, approvals_required,
    approvals_left, approved_by, rules, raw_state, fetched_at
) VALUES (
    $1, $2, $3, sqlc.narg('approvals_required'),
    sqlc.narg('approvals_left'), $4, $5, sqlc.narg('raw_state'), now()
)
ON CONFLICT (merge_request_id) DO UPDATE SET
    approved = EXCLUDED.approved,
    approvals_required = EXCLUDED.approvals_required,
    approvals_left = EXCLUDED.approvals_left,
    approved_by = EXCLUDED.approved_by,
    rules = EXCLUDED.rules,
    raw_state = EXCLUDED.raw_state,
    fetched_at = now(),
    updated_at = now()
RETURNING *;

-- name: UpsertGitLabMRDiscussion :one
INSERT INTO gitlab_mr_discussion (
    workspace_id, merge_request_id, gitlab_discussion_id, individual_note,
    resolved, discussion_created_at, discussion_updated_at, fetched_at
) VALUES (
    $1, $2, $3, $4, sqlc.narg('resolved'),
    sqlc.narg('discussion_created_at'), sqlc.narg('discussion_updated_at'), now()
)
ON CONFLICT (merge_request_id, gitlab_discussion_id) DO UPDATE SET
    individual_note = EXCLUDED.individual_note,
    resolved = EXCLUDED.resolved,
    discussion_created_at = EXCLUDED.discussion_created_at,
    discussion_updated_at = EXCLUDED.discussion_updated_at,
    fetched_at = now(),
    updated_at = now()
RETURNING *;

-- name: UpsertGitLabMRNote :one
INSERT INTO gitlab_mr_note (
    workspace_id, discussion_id, merge_request_id, gitlab_note_id,
    author_username, author_avatar_url, body, system, resolved, resolvable,
    note_created_at, note_updated_at
) VALUES (
    $1, $2, $3, $4,
    sqlc.narg('author_username'), sqlc.narg('author_avatar_url'),
    $5, $6, sqlc.narg('resolved'), sqlc.narg('resolvable'),
    sqlc.narg('note_created_at'), sqlc.narg('note_updated_at')
)
ON CONFLICT (merge_request_id, gitlab_note_id) DO UPDATE SET
    discussion_id = EXCLUDED.discussion_id,
    author_username = EXCLUDED.author_username,
    author_avatar_url = EXCLUDED.author_avatar_url,
    body = EXCLUDED.body,
    system = EXCLUDED.system,
    resolved = EXCLUDED.resolved,
    resolvable = EXCLUDED.resolvable,
    note_created_at = EXCLUDED.note_created_at,
    note_updated_at = EXCLUDED.note_updated_at,
    updated_at = now()
RETURNING *;

-- name: ListGitLabMRPipelinesByMR :many
SELECT * FROM gitlab_mr_pipeline
WHERE merge_request_id = $1
ORDER BY pipeline_updated_at DESC, pipeline_id DESC;

-- name: UpsertGitLabPipelineJob :one
INSERT INTO gitlab_pipeline_job (
    workspace_id, merge_request_id, pipeline_id, job_id, name, stage,
    status, ref, sha, web_url, started_at, finished_at, duration_seconds,
    queued_duration_seconds, failure_reason, allow_failure,
    artifacts_file_name, artifacts_file_size, artifacts_expire_at, fetched_at
) VALUES (
    $1, $2, $3, $4, $5, sqlc.narg('stage'),
    $6, sqlc.narg('ref'), sqlc.narg('sha'), sqlc.narg('web_url'),
    sqlc.narg('started_at'), sqlc.narg('finished_at'),
    sqlc.narg('duration_seconds'), sqlc.narg('queued_duration_seconds'),
    sqlc.narg('failure_reason'), $7,
    sqlc.narg('artifacts_file_name'), sqlc.narg('artifacts_file_size'),
    sqlc.narg('artifacts_expire_at'), now()
)
ON CONFLICT (merge_request_id, job_id) DO UPDATE SET
    pipeline_id = EXCLUDED.pipeline_id,
    name = EXCLUDED.name,
    stage = EXCLUDED.stage,
    status = EXCLUDED.status,
    ref = EXCLUDED.ref,
    sha = EXCLUDED.sha,
    web_url = EXCLUDED.web_url,
    started_at = EXCLUDED.started_at,
    finished_at = EXCLUDED.finished_at,
    duration_seconds = EXCLUDED.duration_seconds,
    queued_duration_seconds = EXCLUDED.queued_duration_seconds,
    failure_reason = EXCLUDED.failure_reason,
    allow_failure = EXCLUDED.allow_failure,
    artifacts_file_name = EXCLUDED.artifacts_file_name,
    artifacts_file_size = EXCLUDED.artifacts_file_size,
    artifacts_expire_at = EXCLUDED.artifacts_expire_at,
    fetched_at = now(),
    updated_at = now()
RETURNING *;

-- name: UpdateGitLabPipelineJobTraceSummary :exec
UPDATE gitlab_pipeline_job
SET trace_summary = $3,
    trace_truncated = $4,
    trace_fetched_at = now(),
    updated_at = now()
WHERE id = $1 AND workspace_id = $2;

-- name: GetGitLabMergeRequestForIssue :one
SELECT mr.*
FROM gitlab_merge_request mr
JOIN issue_gitlab_merge_request imr
  ON imr.merge_request_id = mr.id
 AND imr.workspace_id = mr.workspace_id
WHERE imr.issue_id = $1
  AND mr.id = $2
  AND mr.workspace_id = $3;

-- name: GetGitLabPipelineJobForIssue :one
SELECT j.*
FROM gitlab_pipeline_job j
JOIN issue_gitlab_merge_request imr
  ON imr.merge_request_id = j.merge_request_id
 AND imr.workspace_id = j.workspace_id
WHERE imr.issue_id = $1
  AND j.id = $2
  AND j.workspace_id = $3;

-- name: ListGitLabMRDiscussions :many
SELECT * FROM gitlab_mr_discussion
WHERE workspace_id = $1 AND merge_request_id = $2
ORDER BY COALESCE(discussion_updated_at, discussion_created_at, updated_at) DESC, id ASC;

-- name: ListGitLabMRNotes :many
SELECT * FROM gitlab_mr_note
WHERE workspace_id = $1 AND merge_request_id = $2
ORDER BY COALESCE(note_created_at, created_at) ASC, id ASC;

-- name: ListGitLabPipelineJobsByMR :many
SELECT * FROM gitlab_pipeline_job
WHERE workspace_id = $1 AND merge_request_id = $2
ORDER BY pipeline_id DESC, stage ASC NULLS LAST, job_id ASC;

-- name: GetGitLabMRApprovalState :one
SELECT * FROM gitlab_mr_approval_state
WHERE workspace_id = $1 AND merge_request_id = $2;
