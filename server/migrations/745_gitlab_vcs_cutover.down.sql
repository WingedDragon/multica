DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM vcs_gitlab_migration_state WHERE phase <> 'complete' OR maintenance_mode)
       OR EXISTS (SELECT 1 FROM vcs_gitlab_project)
       OR EXISTS (SELECT 1 FROM vcs_gitlab_migration_id_map)
       OR EXISTS (SELECT 1 FROM vcs_gitlab_migration_hook)
       OR EXISTS (SELECT 1 FROM vcs_gitlab_migration_delivery)
       OR EXISTS (SELECT 1 FROM vcs_gitlab_migration_snapshot)
       OR EXISTS (SELECT 1 FROM vcs_gitlab_mr_detail)
       OR EXISTS (SELECT 1 FROM gitlab_mr_approval_state WHERE vcs_pull_request_id IS NOT NULL OR merge_request_id IS NULL)
       OR EXISTS (SELECT 1 FROM gitlab_mr_discussion WHERE vcs_pull_request_id IS NOT NULL OR merge_request_id IS NULL)
       OR EXISTS (SELECT 1 FROM gitlab_mr_note WHERE vcs_pull_request_id IS NOT NULL OR merge_request_id IS NULL)
       OR EXISTS (SELECT 1 FROM issue_vcs_pull_request WHERE NOT vcs_linked OR gitlab_close_intent OR gitlab_linked_by_type IS NOT NULL OR gitlab_linked_by_id IS NOT NULL OR gitlab_linked_at IS NOT NULL)
       OR EXISTS (SELECT 1 FROM gitlab_pipeline_job WHERE vcs_pull_request_id IS NOT NULL OR merge_request_id IS NULL) THEN
        RAISE EXCEPTION 'refusing to roll back GitLab VCS cutover schema after migration state or data exists';
    END IF;
END $$;

ALTER TABLE issue_vcs_pull_request DROP COLUMN IF EXISTS gitlab_linked_at;
ALTER TABLE issue_vcs_pull_request DROP COLUMN IF EXISTS gitlab_linked_by_id;
ALTER TABLE issue_vcs_pull_request DROP COLUMN IF EXISTS gitlab_linked_by_type;
ALTER TABLE issue_vcs_pull_request DROP COLUMN IF EXISTS gitlab_close_intent;
ALTER TABLE issue_vcs_pull_request DROP COLUMN IF EXISTS vcs_linked;
ALTER TABLE gitlab_pipeline_job DROP COLUMN IF EXISTS vcs_pull_request_id;
ALTER TABLE gitlab_mr_note DROP COLUMN IF EXISTS vcs_pull_request_id;
ALTER TABLE gitlab_mr_discussion DROP COLUMN IF EXISTS vcs_pull_request_id;
ALTER TABLE gitlab_mr_approval_state DROP COLUMN IF EXISTS vcs_pull_request_id;
DROP TABLE IF EXISTS vcs_gitlab_mr_detail;
DROP TABLE IF EXISTS vcs_gitlab_migration_delivery;
DROP TABLE IF EXISTS vcs_gitlab_migration_hook;
DROP TABLE IF EXISTS vcs_gitlab_migration_snapshot;
DROP TABLE IF EXISTS vcs_gitlab_migration_state;
DROP TABLE IF EXISTS vcs_gitlab_migration_id_map;
DROP TABLE IF EXISTS vcs_gitlab_project;
