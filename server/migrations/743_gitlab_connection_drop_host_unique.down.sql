-- Single-statement concurrent build; rollback cleanup is registered in cmd/migrate.
CREATE UNIQUE INDEX CONCURRENTLY IF NOT EXISTS gitlab_connection_workspace_host_uidx
    ON gitlab_connection (workspace_id, host);
