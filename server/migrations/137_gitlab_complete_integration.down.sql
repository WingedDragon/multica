DROP TABLE IF EXISTS gitlab_pipeline_job;
DROP TABLE IF EXISTS gitlab_mr_note;
DROP TABLE IF EXISTS gitlab_mr_discussion;
DROP TABLE IF EXISTS gitlab_mr_approval_state;

ALTER TABLE gitlab_merge_request
    DROP COLUMN IF EXISTS reviewers,
    DROP COLUMN IF EXISTS assignees,
    DROP COLUMN IF EXISTS labels,
    DROP COLUMN IF EXISTS last_refreshed_at,
    DROP COLUMN IF EXISTS last_refresh_error;

ALTER TABLE gitlab_project_binding
    DROP COLUMN IF EXISTS last_refresh_at,
    DROP COLUMN IF EXISTS last_refresh_error,
    DROP COLUMN IF EXISTS last_event_at,
    DROP COLUMN IF EXISTS last_event_type,
    DROP COLUMN IF EXISTS refresh_in_progress_at;
