CREATE UNIQUE INDEX CONCURRENTLY IF NOT EXISTS gitlab_mr_approval_legacy_id_uidx
    ON gitlab_mr_approval_state (merge_request_id);
