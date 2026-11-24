CREATE UNIQUE INDEX CONCURRENTLY IF NOT EXISTS vcs_gitlab_project_scope_key
    ON vcs_gitlab_project (workspace_id, connection_id, gitlab_project_id);
