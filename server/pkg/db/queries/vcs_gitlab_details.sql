-- GitLab-specific enrichment is keyed exclusively by the canonical VCS pull request.
-- Legacy IDs are resolved only at the old API boundary and remain workspace-scoped.

-- name: ResolveGitLabVCSMigrationPullRequestID :one
SELECT unified_id
FROM vcs_gitlab_migration_id_map
WHERE workspace_id = sqlc.arg('workspace_id')
  AND entity_kind = 'pull_request'
  AND legacy_id = sqlc.arg('legacy_id');

-- name: GetVCSGitLabPullRequestForIssue :one
SELECT pr.*
FROM vcs_pull_request pr
JOIN issue_vcs_pull_request ipr ON ipr.pull_request_id = pr.id
JOIN issue i ON i.id = ipr.issue_id
WHERE i.id = sqlc.arg('issue_id')
  AND i.workspace_id = sqlc.arg('workspace_id')
  AND pr.id = sqlc.arg('pull_request_id')
  AND pr.workspace_id = i.workspace_id
  AND pr.provider = 'gitlab';

-- name: GetVCSGitLabProjectForPullRequest :one
SELECT project.*
FROM vcs_gitlab_project project
JOIN vcs_connection connection
  ON connection.id = project.connection_id
 AND connection.workspace_id = project.workspace_id
JOIN vcs_pull_request pr
  ON pr.connection_id = project.connection_id
 AND pr.workspace_id = project.workspace_id
WHERE pr.workspace_id = sqlc.arg('workspace_id')
  AND pr.connection_id = sqlc.arg('connection_id')
  AND pr.provider = 'gitlab'
  AND pr.repo_owner = sqlc.arg('repo_owner')
  AND pr.repo_name = sqlc.arg('repo_name')
  AND lower(project.path_with_namespace) = lower(pr.repo_owner || '/' || pr.repo_name)
  AND connection.provider = 'gitlab'
LIMIT 1;

-- name: GetVCSGitLabMRDetail :one
SELECT detail.*
FROM vcs_gitlab_mr_detail detail
JOIN vcs_pull_request pr ON pr.id = detail.vcs_pull_request_id
WHERE pr.workspace_id = sqlc.arg('workspace_id')
  AND pr.id = sqlc.arg('pull_request_id')
  AND pr.provider = 'gitlab';

-- name: UpsertVCSGitLabMRDetail :one
INSERT INTO vcs_gitlab_mr_detail (
    vcs_pull_request_id, description, merge_commit_sha, detailed_merge_status,
    target_branch, reviewers, assignees, labels, last_refreshed_at,
    last_refresh_error, conflict_sha, pipeline_sync_at, pipeline_sync_error,
    wakeup_sync_generation
)
SELECT pr.id AS vcs_pull_request_id,
       sqlc.narg('description') AS description,
       sqlc.narg('merge_commit_sha') AS merge_commit_sha,
       sqlc.narg('detailed_merge_status') AS detailed_merge_status,
       sqlc.narg('target_branch') AS target_branch,
       COALESCE(sqlc.narg('reviewers')::jsonb, '[]'::jsonb) AS reviewers,
       COALESCE(sqlc.narg('assignees')::jsonb, '[]'::jsonb) AS assignees,
       COALESCE(sqlc.narg('labels')::jsonb, '[]'::jsonb) AS labels,
       sqlc.narg('last_refreshed_at') AS last_refreshed_at,
       sqlc.narg('last_refresh_error') AS last_refresh_error,
       COALESCE(sqlc.narg('conflict_sha'), '') AS conflict_sha,
       sqlc.narg('pipeline_sync_at') AS pipeline_sync_at,
       sqlc.narg('pipeline_sync_error') AS pipeline_sync_error,
       sqlc.arg('wakeup_sync_generation') AS wakeup_sync_generation
FROM vcs_pull_request pr
WHERE pr.id = sqlc.arg('pull_request_id')
  AND pr.workspace_id = sqlc.arg('workspace_id')
  AND pr.provider = 'gitlab'
  AND pr.wakeup_sync_generation = sqlc.arg('wakeup_sync_generation')
ON CONFLICT (vcs_pull_request_id) DO UPDATE SET
    description = EXCLUDED.description,
    merge_commit_sha = EXCLUDED.merge_commit_sha,
    detailed_merge_status = EXCLUDED.detailed_merge_status,
    target_branch = EXCLUDED.target_branch,
    reviewers = EXCLUDED.reviewers,
    assignees = EXCLUDED.assignees,
    labels = EXCLUDED.labels,
    last_refreshed_at = EXCLUDED.last_refreshed_at,
    last_refresh_error = EXCLUDED.last_refresh_error,
    conflict_sha = EXCLUDED.conflict_sha,
    pipeline_sync_at = EXCLUDED.pipeline_sync_at,
    pipeline_sync_error = EXCLUDED.pipeline_sync_error,
    wakeup_sync_generation = EXCLUDED.wakeup_sync_generation,
    updated_at = now()
WHERE EXCLUDED.wakeup_sync_generation >= vcs_gitlab_mr_detail.wakeup_sync_generation
  AND EXISTS (
      SELECT 1 FROM vcs_pull_request current_pr
      WHERE current_pr.id = EXCLUDED.vcs_pull_request_id
        AND current_pr.workspace_id = sqlc.arg('workspace_id')
        AND current_pr.wakeup_sync_generation = sqlc.arg('wakeup_sync_generation')
  )
RETURNING *;

-- name: MarkVCSGitLabMRRefreshError :execrows
UPDATE vcs_gitlab_mr_detail detail
SET last_refreshed_at = now(),
    last_refresh_error = sqlc.arg('last_refresh_error'),
    updated_at = now()
FROM vcs_pull_request pr
WHERE detail.vcs_pull_request_id = pr.id
  AND pr.id = sqlc.arg('pull_request_id')
  AND pr.workspace_id = sqlc.arg('workspace_id')
  AND pr.provider = 'gitlab'
  AND pr.wakeup_sync_generation = sqlc.arg('wakeup_sync_generation');

-- name: GetGitLabMRApprovalStateByPullRequest :one
SELECT * FROM gitlab_mr_approval_state
WHERE workspace_id = sqlc.arg('workspace_id')
  AND vcs_pull_request_id = sqlc.arg('pull_request_id');

-- name: UpsertGitLabMRApprovalStateByPullRequest :one
INSERT INTO gitlab_mr_approval_state (
    merge_request_id, vcs_pull_request_id, workspace_id, approved,
    approvals_required, approvals_left, approved_by, rules, raw_state, fetched_at
)
SELECT NULL::uuid AS merge_request_id,
       pr.id AS vcs_pull_request_id,
       pr.workspace_id AS workspace_id,
       sqlc.arg('approved') AS approved,
       sqlc.narg('approvals_required') AS approvals_required,
       sqlc.narg('approvals_left') AS approvals_left,
       COALESCE(sqlc.narg('approved_by')::jsonb, '[]'::jsonb) AS approved_by,
       COALESCE(sqlc.narg('rules')::jsonb, '[]'::jsonb) AS rules,
       sqlc.narg('raw_state') AS raw_state,
       now() AS fetched_at
FROM vcs_pull_request pr
WHERE pr.id = sqlc.arg('pull_request_id')
  AND pr.workspace_id = sqlc.arg('workspace_id')
  AND pr.provider = 'gitlab'
  AND pr.wakeup_sync_generation = sqlc.arg('wakeup_sync_generation')
ON CONFLICT (vcs_pull_request_id) DO UPDATE SET
    merge_request_id = NULL,
    approved = EXCLUDED.approved,
    approvals_required = EXCLUDED.approvals_required,
    approvals_left = EXCLUDED.approvals_left,
    approved_by = EXCLUDED.approved_by,
    rules = EXCLUDED.rules,
    raw_state = EXCLUDED.raw_state,
    fetched_at = now(),
    updated_at = now()
WHERE EXISTS (
    SELECT 1 FROM vcs_pull_request current_pr
    WHERE current_pr.id = EXCLUDED.vcs_pull_request_id
      AND current_pr.workspace_id = EXCLUDED.workspace_id
      AND current_pr.wakeup_sync_generation = sqlc.arg('wakeup_sync_generation')
)
RETURNING *;

-- name: ListGitLabMRDiscussionsByPullRequest :many
SELECT * FROM gitlab_mr_discussion
WHERE workspace_id = sqlc.arg('workspace_id')
  AND vcs_pull_request_id = sqlc.arg('pull_request_id')
ORDER BY COALESCE(discussion_updated_at, discussion_created_at, updated_at) DESC, id ASC;

-- name: UpsertGitLabMRDiscussionByPullRequest :one
INSERT INTO gitlab_mr_discussion (
    merge_request_id, workspace_id, vcs_pull_request_id, gitlab_discussion_id,
    individual_note, resolved, discussion_created_at, discussion_updated_at, fetched_at
)
SELECT NULL::uuid AS merge_request_id,
       pr.workspace_id AS workspace_id,
       pr.id AS vcs_pull_request_id,
       sqlc.arg('gitlab_discussion_id') AS gitlab_discussion_id,
       sqlc.arg('individual_note') AS individual_note,
       sqlc.narg('resolved') AS resolved,
       sqlc.narg('discussion_created_at') AS discussion_created_at,
       sqlc.narg('discussion_updated_at') AS discussion_updated_at,
       now() AS fetched_at
FROM vcs_pull_request pr
WHERE pr.id = sqlc.arg('pull_request_id')
  AND pr.workspace_id = sqlc.arg('workspace_id')
  AND pr.provider = 'gitlab'
  AND pr.wakeup_sync_generation = sqlc.arg('wakeup_sync_generation')
ON CONFLICT (vcs_pull_request_id, gitlab_discussion_id) DO UPDATE SET
    merge_request_id = NULL,
    individual_note = EXCLUDED.individual_note,
    resolved = EXCLUDED.resolved,
    discussion_created_at = EXCLUDED.discussion_created_at,
    discussion_updated_at = EXCLUDED.discussion_updated_at,
    fetched_at = now(),
    updated_at = now()
WHERE EXISTS (
    SELECT 1 FROM vcs_pull_request current_pr
    WHERE current_pr.id = EXCLUDED.vcs_pull_request_id
      AND current_pr.workspace_id = EXCLUDED.workspace_id
      AND current_pr.wakeup_sync_generation = sqlc.arg('wakeup_sync_generation')
)
RETURNING *;

-- name: ListGitLabMRNotesByPullRequest :many
SELECT * FROM gitlab_mr_note
WHERE workspace_id = sqlc.arg('workspace_id')
  AND vcs_pull_request_id = sqlc.arg('pull_request_id')
ORDER BY COALESCE(note_created_at, created_at) ASC, id ASC;

-- name: UpsertGitLabMRNoteByPullRequest :one
INSERT INTO gitlab_mr_note (
    merge_request_id, workspace_id, discussion_id, vcs_pull_request_id,
    gitlab_note_id, author_username, author_avatar_url, body, system,
    resolved, resolvable, note_created_at, note_updated_at
)
SELECT NULL::uuid AS merge_request_id,
       pr.workspace_id AS workspace_id,
       sqlc.arg('discussion_id') AS discussion_id,
       pr.id AS vcs_pull_request_id,
       sqlc.arg('gitlab_note_id') AS gitlab_note_id,
       sqlc.narg('author_username') AS author_username,
       sqlc.narg('author_avatar_url') AS author_avatar_url,
       sqlc.arg('body') AS body,
       sqlc.arg('system') AS system,
       sqlc.narg('resolved') AS resolved,
       sqlc.narg('resolvable') AS resolvable,
       sqlc.narg('note_created_at') AS note_created_at,
       sqlc.narg('note_updated_at') AS note_updated_at
FROM vcs_pull_request pr
WHERE pr.id = sqlc.arg('pull_request_id')
  AND pr.workspace_id = sqlc.arg('workspace_id')
  AND pr.provider = 'gitlab'
  AND pr.wakeup_sync_generation = sqlc.arg('wakeup_sync_generation')
ON CONFLICT (vcs_pull_request_id, gitlab_note_id) DO UPDATE SET
    merge_request_id = NULL,
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
WHERE COALESCE(EXCLUDED.note_updated_at, EXCLUDED.note_created_at, '-infinity'::timestamptz)
      >= COALESCE(gitlab_mr_note.note_updated_at, gitlab_mr_note.note_created_at, '-infinity'::timestamptz)
  AND EXISTS (
      SELECT 1 FROM vcs_pull_request current_pr
      WHERE current_pr.id = EXCLUDED.vcs_pull_request_id
        AND current_pr.workspace_id = EXCLUDED.workspace_id
        AND current_pr.wakeup_sync_generation = sqlc.arg('wakeup_sync_generation')
  )
RETURNING *;

-- name: ListGitLabPipelineJobsByPullRequest :many
SELECT job.*
FROM gitlab_pipeline_job job
JOIN vcs_pull_request pr ON pr.id = job.vcs_pull_request_id
JOIN vcs_gitlab_pipeline pipeline
  ON pipeline.workspace_id = pr.workspace_id
 AND pipeline.pull_request_id = pr.id
 AND pipeline.pipeline_id = job.pipeline_id
 AND pipeline.sha = pr.head_sha
WHERE pr.workspace_id = sqlc.arg('workspace_id')
  AND pr.id = sqlc.arg('pull_request_id')
  AND pr.provider = 'gitlab'
  AND pr.head_sha <> ''
  AND job.workspace_id = pr.workspace_id
  AND job.sha = pr.head_sha
ORDER BY job.pipeline_id DESC, job.stage ASC NULLS LAST, job.job_id ASC;

-- name: GetGitLabVCSJobForIssue :one
SELECT job.*
FROM gitlab_pipeline_job job
JOIN vcs_pull_request pr ON pr.id = job.vcs_pull_request_id
JOIN vcs_gitlab_pipeline pipeline
  ON pipeline.workspace_id = pr.workspace_id
 AND pipeline.pull_request_id = pr.id
 AND pipeline.pipeline_id = job.pipeline_id
 AND pipeline.sha = pr.head_sha
JOIN issue_vcs_pull_request ipr ON ipr.pull_request_id = pr.id
JOIN issue i ON i.id = ipr.issue_id
WHERE i.id = sqlc.arg('issue_id')
  AND i.workspace_id = sqlc.arg('workspace_id')
  AND job.id = sqlc.arg('job_id')
  AND job.workspace_id = i.workspace_id
  AND pr.workspace_id = i.workspace_id
  AND pr.provider = 'gitlab'
  AND pr.head_sha <> ''
  AND job.sha = pr.head_sha;

-- name: GetGitLabVCSJobForIssueByPullRequest :one
SELECT job.*
FROM gitlab_pipeline_job job
JOIN vcs_pull_request pr ON pr.id = job.vcs_pull_request_id
JOIN vcs_gitlab_pipeline pipeline
  ON pipeline.workspace_id = pr.workspace_id
 AND pipeline.pull_request_id = pr.id
 AND pipeline.pipeline_id = job.pipeline_id
 AND pipeline.sha = pr.head_sha
JOIN issue_vcs_pull_request ipr ON ipr.pull_request_id = pr.id
JOIN issue i ON i.id = ipr.issue_id
WHERE i.id = sqlc.arg('issue_id')
  AND i.workspace_id = sqlc.arg('workspace_id')
  AND pr.id = sqlc.arg('pull_request_id')
  AND job.id = sqlc.arg('job_id')
  AND job.workspace_id = i.workspace_id
  AND pr.workspace_id = i.workspace_id
  AND pr.provider = 'gitlab'
  AND pr.head_sha <> ''
  AND job.sha = pr.head_sha;

-- name: UpsertGitLabPipelineJobByPullRequest :one
INSERT INTO gitlab_pipeline_job (
    merge_request_id, workspace_id, vcs_pull_request_id, pipeline_id, job_id,
    name, stage, status, ref, sha, web_url, started_at, finished_at,
    duration_seconds, queued_duration_seconds, failure_reason, allow_failure,
    artifacts_file_name, artifacts_file_size, artifacts_expire_at, fetched_at
)
SELECT NULL::uuid AS merge_request_id,
       pr.workspace_id AS workspace_id,
       pr.id AS vcs_pull_request_id,
       sqlc.arg('pipeline_id') AS pipeline_id,
       sqlc.arg('job_id') AS job_id,
       sqlc.arg('name') AS name,
       sqlc.narg('stage') AS stage,
       sqlc.arg('status') AS status,
       sqlc.narg('ref') AS ref,
       sqlc.narg('sha') AS sha,
       sqlc.narg('web_url') AS web_url,
       sqlc.narg('started_at') AS started_at,
       sqlc.narg('finished_at') AS finished_at,
       sqlc.narg('duration_seconds') AS duration_seconds,
       sqlc.narg('queued_duration_seconds') AS queued_duration_seconds,
       sqlc.narg('failure_reason') AS failure_reason,
       sqlc.arg('allow_failure') AS allow_failure,
       sqlc.narg('artifacts_file_name') AS artifacts_file_name,
       sqlc.narg('artifacts_file_size') AS artifacts_file_size,
       sqlc.narg('artifacts_expire_at') AS artifacts_expire_at,
       now() AS fetched_at
FROM vcs_pull_request pr
WHERE pr.id = sqlc.arg('pull_request_id')
  AND pr.workspace_id = sqlc.arg('workspace_id')
  AND pr.provider = 'gitlab'
  AND pr.wakeup_sync_generation = sqlc.arg('wakeup_sync_generation')
ON CONFLICT (vcs_pull_request_id, job_id) DO UPDATE SET
    merge_request_id = NULL,
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
WHERE COALESCE(EXCLUDED.finished_at, EXCLUDED.started_at, '-infinity'::timestamptz) >=
      COALESCE(gitlab_pipeline_job.finished_at, gitlab_pipeline_job.started_at, '-infinity'::timestamptz)
  AND EXISTS (
      SELECT 1 FROM vcs_pull_request current_pr
      WHERE current_pr.id = EXCLUDED.vcs_pull_request_id
        AND current_pr.workspace_id = EXCLUDED.workspace_id
        AND current_pr.wakeup_sync_generation = sqlc.arg('wakeup_sync_generation')
  )
RETURNING *;

-- name: UpdateGitLabPipelineJobTraceSummaryByPullRequest :execrows
UPDATE gitlab_pipeline_job job
SET trace_summary = sqlc.narg('trace_summary'),
    trace_truncated = sqlc.arg('trace_truncated'),
    trace_fetched_at = now(),
    updated_at = now()
FROM vcs_pull_request pr
WHERE job.vcs_pull_request_id = pr.id
  AND job.id = sqlc.arg('job_id')
  AND job.workspace_id = sqlc.arg('workspace_id')
  AND pr.id = sqlc.arg('pull_request_id')
  AND pr.workspace_id = sqlc.arg('workspace_id')
  AND pr.provider = 'gitlab'
  AND pr.head_sha = job.sha
  AND pr.wakeup_sync_generation = sqlc.arg('wakeup_sync_generation');
-- name: MarkVCSGitLabMRPipelineSync :execrows
UPDATE vcs_gitlab_mr_detail detail
SET pipeline_sync_at = now(),
    pipeline_sync_error = sqlc.arg('pipeline_sync_error'),
    updated_at = now()
FROM vcs_pull_request pr
WHERE detail.vcs_pull_request_id = pr.id
  AND pr.id = sqlc.arg('pull_request_id')
  AND pr.workspace_id = sqlc.arg('workspace_id')
  AND pr.provider = 'gitlab'
  AND pr.wakeup_sync_generation = sqlc.arg('wakeup_sync_generation')
  AND pr.head_sha = sqlc.arg('head_sha');

-- name: MarkVCSGitLabMRPipelineSyncSuccess :execrows
UPDATE vcs_gitlab_mr_detail detail
SET pipeline_sync_at = now(),
    pipeline_sync_error = NULL,
    updated_at = now()
FROM vcs_pull_request pr
WHERE detail.vcs_pull_request_id = pr.id
  AND pr.id = sqlc.arg('pull_request_id')
  AND pr.workspace_id = sqlc.arg('workspace_id')
  AND pr.provider = 'gitlab'
  AND pr.wakeup_sync_generation = sqlc.arg('wakeup_sync_generation')
  AND pr.head_sha = sqlc.arg('head_sha');

-- name: GetVCSGitLabPipelineForJob :one
SELECT pipeline.*
FROM vcs_gitlab_pipeline pipeline
JOIN vcs_pull_request pr ON pr.id = pipeline.pull_request_id
WHERE pr.workspace_id = sqlc.arg('workspace_id')
  AND pr.id = sqlc.arg('pull_request_id')
  AND pr.provider = 'gitlab'
  AND pipeline.workspace_id = pr.workspace_id
  AND pipeline.pipeline_id = sqlc.arg('pipeline_id')
  AND pipeline.sha = pr.head_sha
ORDER BY pipeline.pipeline_updated_at DESC
LIMIT 1;
-- name: ListGitLabVCSMergeRequestsForIssue :many
SELECT
    pr.id,
    pr.workspace_id,
    project.path_with_namespace,
    project.gitlab_project_id,
    pr.pr_number AS mr_iid,
    pr.title,
    pr.state,
    pr.html_url AS web_url,
    pr.branch AS source_branch,
    detail.target_branch,
    pr.author_login AS author_username,
    pr.author_avatar_url,
    pr.head_sha AS sha,
    detail.detailed_merge_status,
    pr.has_conflicts,
    COALESCE(current_pipeline.status, '') AS pipeline_status,
    current_pipeline.web_url AS pipeline_url,
    pr.additions,
    pr.deletions,
    pr.changed_files,
    COALESCE(detail.reviewers, '[]'::jsonb) AS reviewers,
    COALESCE(detail.assignees, '[]'::jsonb) AS assignees,
    COALESCE(detail.labels, '[]'::jsonb) AS labels,
    detail.last_refreshed_at,
    detail.last_refresh_error,
    pr.merged_at,
    pr.closed_at,
    pr.pr_created_at AS mr_created_at,
    pr.pr_updated_at AS mr_updated_at,
    COALESCE(ipr.linked_by_type, 'system')::text AS linked_by_type
FROM issue_vcs_pull_request ipr
JOIN issue i ON i.id = ipr.issue_id
JOIN vcs_pull_request pr ON pr.id = ipr.pull_request_id
JOIN vcs_gitlab_project project
  ON project.workspace_id = pr.workspace_id
 AND project.connection_id = pr.connection_id
 AND lower(project.path_with_namespace) = lower(pr.repo_owner || '/' || pr.repo_name)
LEFT JOIN vcs_gitlab_mr_detail detail ON detail.vcs_pull_request_id = pr.id
LEFT JOIN LATERAL (
    SELECT pipeline.status, pipeline.web_url
    FROM vcs_gitlab_pipeline pipeline
    WHERE pipeline.workspace_id = pr.workspace_id
      AND pipeline.pull_request_id = pr.id
      AND pipeline.sha = pr.head_sha
      AND pr.head_sha <> ''
    ORDER BY pipeline.pipeline_updated_at DESC, pipeline.pipeline_id DESC
    LIMIT 1
) current_pipeline ON true
WHERE i.id = sqlc.arg('issue_id')
  AND i.workspace_id = sqlc.arg('workspace_id')
  AND pr.workspace_id = i.workspace_id
  AND pr.provider = 'gitlab'
  AND ipr.gitlab_linked_at IS NOT NULL
ORDER BY pr.pr_created_at DESC;
-- name: EnsureVCSGitLabMRDetail :execrows
INSERT INTO vcs_gitlab_mr_detail (vcs_pull_request_id)
SELECT pr.id
FROM vcs_pull_request pr
WHERE pr.id = sqlc.arg('pull_request_id')
  AND pr.workspace_id = sqlc.arg('workspace_id')
  AND pr.provider = 'gitlab'
ON CONFLICT (vcs_pull_request_id) DO NOTHING;
