ALTER TABLE vcs_gitlab_project
    ADD CONSTRAINT vcs_gitlab_project_pkey PRIMARY KEY USING INDEX vcs_gitlab_project_pkey;
ALTER TABLE vcs_gitlab_project
    ADD CONSTRAINT vcs_gitlab_project_scope_key UNIQUE USING INDEX vcs_gitlab_project_scope_key;
ALTER TABLE vcs_gitlab_migration_id_map
    ADD CONSTRAINT vcs_gitlab_migration_id_map_pkey PRIMARY KEY USING INDEX vcs_gitlab_migration_id_map_pkey;
ALTER TABLE vcs_gitlab_migration_id_map
    ADD CONSTRAINT vcs_gitlab_migration_id_map_unified_key UNIQUE USING INDEX vcs_gitlab_migration_id_map_unified_key;
ALTER TABLE vcs_gitlab_migration_state
    ADD CONSTRAINT vcs_gitlab_migration_state_pkey PRIMARY KEY USING INDEX vcs_gitlab_migration_state_pkey;
ALTER TABLE vcs_gitlab_migration_snapshot
    ADD CONSTRAINT vcs_gitlab_migration_snapshot_pkey PRIMARY KEY USING INDEX vcs_gitlab_migration_snapshot_pkey;
ALTER TABLE vcs_gitlab_migration_hook
    ADD CONSTRAINT vcs_gitlab_migration_hook_pkey PRIMARY KEY USING INDEX vcs_gitlab_migration_hook_pkey;
ALTER TABLE vcs_gitlab_migration_delivery
    ADD CONSTRAINT vcs_gitlab_migration_delivery_pkey PRIMARY KEY USING INDEX vcs_gitlab_migration_delivery_pkey;
ALTER TABLE vcs_gitlab_mr_detail
    ADD CONSTRAINT vcs_gitlab_mr_detail_pkey PRIMARY KEY USING INDEX vcs_gitlab_mr_detail_pkey;
ALTER TABLE vcs_gitlab_migration_system_hook_attestation
    ADD CONSTRAINT vcs_gitlab_migration_system_hook_attestation_pkey
    PRIMARY KEY USING INDEX vcs_gitlab_migration_system_hook_attestation_pkey;

-- Seed the runtime gate only after its unique conflict target exists. An old
-- deployment with legacy rows must not start normally after migrate up; a fresh
-- install starts completed without inspecting credentials.
WITH legacy_data AS (
    SELECT
        EXISTS (SELECT 1 FROM gitlab_connection) OR
        EXISTS (SELECT 1 FROM gitlab_project_binding) OR
        EXISTS (SELECT 1 FROM gitlab_merge_request) OR
        EXISTS (
            SELECT 1 FROM chat_wakeup
            WHERE condition ->> 'provider' = 'gitlab'
              AND condition -> 'gitlab' ->> 'source' = 'gitlab_connection'
        ) AS found
)
INSERT INTO vcs_gitlab_migration_state (id, phase, maintenance_mode, generation, began_at)
SELECT 1,
       CASE WHEN found THEN 'maintenance' ELSE 'complete' END,
       found,
       0,
       CASE WHEN found THEN now() ELSE NULL END
FROM legacy_data
ON CONFLICT (id) DO NOTHING;
