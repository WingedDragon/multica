CREATE INDEX CONCURRENTLY IF NOT EXISTS gitlab_pipeline_job_vcs_pr_idx
    ON gitlab_pipeline_job (vcs_pull_request_id);
