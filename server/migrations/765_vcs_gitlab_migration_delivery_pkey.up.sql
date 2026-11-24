CREATE UNIQUE INDEX CONCURRENTLY IF NOT EXISTS vcs_gitlab_migration_delivery_pkey
    ON vcs_gitlab_migration_delivery (delivery_id, workspace_id, connection_id, scope_type, scope_id, hook_id);
