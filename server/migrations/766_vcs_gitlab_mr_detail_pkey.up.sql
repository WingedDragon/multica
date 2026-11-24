CREATE UNIQUE INDEX CONCURRENTLY IF NOT EXISTS vcs_gitlab_mr_detail_pkey
    ON vcs_gitlab_mr_detail (vcs_pull_request_id);
