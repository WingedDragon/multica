CREATE INDEX CONCURRENTLY IF NOT EXISTS gitlab_mr_note_vcs_pr_idx
    ON gitlab_mr_note (vcs_pull_request_id);
