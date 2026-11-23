-- Build the scoped Pipeline identity index separately so PostgreSQL can run it concurrently.
-- An interrupted build is removed by the migration cleanup hook before retry.
CREATE UNIQUE INDEX CONCURRENTLY IF NOT EXISTS vcs_gitlab_pipeline_identity_uidx
    ON vcs_gitlab_pipeline (pull_request_id, pipeline_project_path, pipeline_id);
