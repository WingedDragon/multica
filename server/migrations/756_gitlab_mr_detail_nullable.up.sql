-- Keep legacy unique conflict targets intact while canonical replay stores only
-- vcs_pull_request_id. The concurrent approval index from migration 755 replaces
-- the primary-key conflict target before its NOT NULL/PK constraint is removed.
ALTER TABLE gitlab_mr_approval_state
    DROP CONSTRAINT IF EXISTS gitlab_mr_approval_state_pkey;
ALTER TABLE gitlab_mr_approval_state
    ALTER COLUMN merge_request_id DROP NOT NULL;
ALTER TABLE gitlab_mr_discussion
    ALTER COLUMN merge_request_id DROP NOT NULL;
ALTER TABLE gitlab_mr_note
    ALTER COLUMN merge_request_id DROP NOT NULL;
ALTER TABLE gitlab_pipeline_job
    ALTER COLUMN merge_request_id DROP NOT NULL;
