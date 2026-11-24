-- Rollback requires the migration tool to restore every canonical-only row to a
-- legacy MR ID first. SET NOT NULL fails closed if any row cannot be restored.
ALTER TABLE gitlab_mr_discussion
    ALTER COLUMN merge_request_id SET NOT NULL;
ALTER TABLE gitlab_mr_note
    ALTER COLUMN merge_request_id SET NOT NULL;
ALTER TABLE gitlab_pipeline_job
    ALTER COLUMN merge_request_id SET NOT NULL;
ALTER TABLE gitlab_mr_approval_state
    ALTER COLUMN merge_request_id SET NOT NULL;
ALTER TABLE gitlab_mr_approval_state
    ADD CONSTRAINT gitlab_mr_approval_state_pkey
    PRIMARY KEY USING INDEX gitlab_mr_approval_legacy_id_uidx;
