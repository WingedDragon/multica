DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM vcs_gitlab_migration_state WHERE phase <> 'complete' OR maintenance_mode)
       OR EXISTS (SELECT 1 FROM vcs_gitlab_project)
       OR EXISTS (SELECT 1 FROM vcs_gitlab_migration_id_map)
       OR EXISTS (SELECT 1 FROM vcs_gitlab_migration_hook)
       OR EXISTS (SELECT 1 FROM vcs_gitlab_migration_delivery)
       OR EXISTS (SELECT 1 FROM vcs_gitlab_migration_snapshot)
       OR EXISTS (SELECT 1 FROM vcs_gitlab_mr_detail)
       OR EXISTS (SELECT 1 FROM vcs_gitlab_migration_system_hook_attestation)
       OR EXISTS (SELECT 1 FROM gitlab_mr_approval_state WHERE vcs_pull_request_id IS NOT NULL OR merge_request_id IS NULL)
       OR EXISTS (SELECT 1 FROM gitlab_mr_discussion WHERE vcs_pull_request_id IS NOT NULL OR merge_request_id IS NULL)
       OR EXISTS (SELECT 1 FROM gitlab_mr_note WHERE vcs_pull_request_id IS NOT NULL OR merge_request_id IS NULL)
       OR EXISTS (SELECT 1 FROM issue_vcs_pull_request WHERE NOT vcs_linked OR gitlab_close_intent OR gitlab_linked_by_type IS NOT NULL OR gitlab_linked_by_id IS NOT NULL OR gitlab_linked_at IS NOT NULL)
       OR EXISTS (SELECT 1 FROM gitlab_pipeline_job WHERE vcs_pull_request_id IS NOT NULL OR merge_request_id IS NULL) THEN
        RAISE EXCEPTION 'refusing to roll back GitLab VCS key constraints after migration state or data exists';
    END IF;
END
$$;

ALTER TABLE vcs_gitlab_migration_system_hook_attestation DROP CONSTRAINT IF EXISTS vcs_gitlab_migration_system_hook_attestation_pkey;
ALTER TABLE vcs_gitlab_mr_detail DROP CONSTRAINT IF EXISTS vcs_gitlab_mr_detail_pkey;
ALTER TABLE vcs_gitlab_migration_delivery DROP CONSTRAINT IF EXISTS vcs_gitlab_migration_delivery_pkey;
ALTER TABLE vcs_gitlab_migration_hook DROP CONSTRAINT IF EXISTS vcs_gitlab_migration_hook_pkey;
ALTER TABLE vcs_gitlab_migration_snapshot DROP CONSTRAINT IF EXISTS vcs_gitlab_migration_snapshot_pkey;
ALTER TABLE vcs_gitlab_migration_state DROP CONSTRAINT IF EXISTS vcs_gitlab_migration_state_pkey;
ALTER TABLE vcs_gitlab_migration_id_map DROP CONSTRAINT IF EXISTS vcs_gitlab_migration_id_map_unified_key;
ALTER TABLE vcs_gitlab_migration_id_map DROP CONSTRAINT IF EXISTS vcs_gitlab_migration_id_map_pkey;
ALTER TABLE vcs_gitlab_project DROP CONSTRAINT IF EXISTS vcs_gitlab_project_scope_key;
ALTER TABLE vcs_gitlab_project DROP CONSTRAINT IF EXISTS vcs_gitlab_project_pkey;
