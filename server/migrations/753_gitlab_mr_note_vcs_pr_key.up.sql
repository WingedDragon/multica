CREATE UNIQUE INDEX CONCURRENTLY IF NOT EXISTS gitlab_mr_note_vcs_pr_key_uidx
    ON gitlab_mr_note (vcs_pull_request_id, gitlab_note_id);
