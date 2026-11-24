CREATE UNIQUE INDEX CONCURRENTLY IF NOT EXISTS vcs_gitlab_migration_snapshot_pkey
    ON vcs_gitlab_migration_snapshot (generation, entity_kind, workspace_id, record_key);
