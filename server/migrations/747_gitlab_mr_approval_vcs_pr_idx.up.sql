CREATE UNIQUE INDEX CONCURRENTLY IF NOT EXISTS gitlab_mr_approval_vcs_pr_uidx
    ON gitlab_mr_approval_state (vcs_pull_request_id);
