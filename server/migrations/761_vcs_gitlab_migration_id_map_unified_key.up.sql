CREATE UNIQUE INDEX CONCURRENTLY IF NOT EXISTS vcs_gitlab_migration_id_map_unified_key
    ON vcs_gitlab_migration_id_map (workspace_id, entity_kind, unified_id);
