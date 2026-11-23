-- Single-statement migration: PostgreSQL cannot build this index concurrently inside a transaction.
CREATE UNIQUE INDEX CONCURRENTLY IF NOT EXISTS gitlab_connection_workspace_base_url_uidx
    ON gitlab_connection (workspace_id, base_url);
