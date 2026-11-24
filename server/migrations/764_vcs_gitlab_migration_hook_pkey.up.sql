CREATE UNIQUE INDEX CONCURRENTLY IF NOT EXISTS vcs_gitlab_migration_hook_pkey
    ON vcs_gitlab_migration_hook (connection_id, scope_type, scope_id, legacy_hook_id);
