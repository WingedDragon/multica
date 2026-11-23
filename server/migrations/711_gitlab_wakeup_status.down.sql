DROP TABLE IF EXISTS vcs_gitlab_pipeline;

ALTER TABLE vcs_pull_request
    DROP COLUMN IF EXISTS has_conflicts,
    DROP COLUMN IF EXISTS conflict_head_sha,
    DROP COLUMN IF EXISTS source_project_id,
    DROP COLUMN IF EXISTS source_project_path,
    DROP COLUMN IF EXISTS status_sync_at,
    DROP COLUMN IF EXISTS status_sync_error,
    DROP COLUMN IF EXISTS pipeline_sync_at,
    DROP COLUMN IF EXISTS pipeline_sync_error,
    DROP COLUMN IF EXISTS wakeup_sync_generation;

ALTER TABLE gitlab_mr_pipeline
    DROP COLUMN IF EXISTS pipeline_project_id;

ALTER TABLE gitlab_merge_request
    DROP COLUMN IF EXISTS conflict_sha,
    DROP COLUMN IF EXISTS source_project_id,
    DROP COLUMN IF EXISTS source_project_path,
    DROP COLUMN IF EXISTS pipeline_sync_at,
    DROP COLUMN IF EXISTS pipeline_sync_error,
    DROP COLUMN IF EXISTS wakeup_sync_generation;
