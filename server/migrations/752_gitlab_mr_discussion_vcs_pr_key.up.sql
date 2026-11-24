CREATE UNIQUE INDEX CONCURRENTLY IF NOT EXISTS gitlab_mr_discussion_vcs_pr_key_uidx
    ON gitlab_mr_discussion (vcs_pull_request_id, gitlab_discussion_id);
