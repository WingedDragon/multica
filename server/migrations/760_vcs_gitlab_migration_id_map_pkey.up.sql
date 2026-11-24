CREATE UNIQUE INDEX CONCURRENTLY IF NOT EXISTS vcs_gitlab_migration_id_map_pkey
    ON vcs_gitlab_migration_id_map (workspace_id, entity_kind, legacy_id);
