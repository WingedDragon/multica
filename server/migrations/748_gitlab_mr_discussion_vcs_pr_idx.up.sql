CREATE INDEX CONCURRENTLY IF NOT EXISTS gitlab_mr_discussion_vcs_pr_idx
    ON gitlab_mr_discussion (vcs_pull_request_id);
