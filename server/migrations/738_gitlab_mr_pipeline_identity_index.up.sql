CREATE UNIQUE INDEX CONCURRENTLY IF NOT EXISTS gitlab_mr_pipeline_identity_uidx
    ON gitlab_mr_pipeline (merge_request_id, pipeline_project_id, pipeline_id);
