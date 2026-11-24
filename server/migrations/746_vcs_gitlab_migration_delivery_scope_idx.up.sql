CREATE INDEX CONCURRENTLY IF NOT EXISTS vcs_gitlab_migration_delivery_scope_received_idx
    ON vcs_gitlab_migration_delivery (connection_id, scope_type, scope_id, received_at DESC);
