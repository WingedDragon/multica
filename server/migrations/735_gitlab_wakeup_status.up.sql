ALTER TABLE gitlab_merge_request
    ADD COLUMN conflict_sha TEXT NOT NULL DEFAULT '',
    ADD COLUMN source_project_id BIGINT,
    ADD COLUMN source_project_path TEXT,
    ADD COLUMN pipeline_sync_at TIMESTAMPTZ,
    ADD COLUMN pipeline_sync_error TEXT,
    ADD COLUMN wakeup_sync_generation BIGINT NOT NULL DEFAULT 0;

ALTER TABLE gitlab_mr_pipeline
    ADD COLUMN pipeline_project_id BIGINT NOT NULL DEFAULT 0;

ALTER TABLE vcs_pull_request
    ADD COLUMN has_conflicts BOOLEAN,
    ADD COLUMN conflict_head_sha TEXT NOT NULL DEFAULT '',
    ADD COLUMN source_project_id BIGINT,
    ADD COLUMN source_project_path TEXT,
    ADD COLUMN status_sync_at TIMESTAMPTZ,
    ADD COLUMN status_sync_error TEXT,
    ADD COLUMN pipeline_sync_at TIMESTAMPTZ,
    ADD COLUMN pipeline_sync_error TEXT,
    ADD COLUMN wakeup_sync_generation BIGINT NOT NULL DEFAULT 0;

CREATE TABLE vcs_gitlab_pipeline (
    workspace_id         UUID NOT NULL,
    connection_id        UUID NOT NULL,
    pull_request_id      UUID NOT NULL,
    pipeline_id          BIGINT NOT NULL,
    pipeline_project_id  BIGINT NOT NULL,
    pipeline_project_path TEXT NOT NULL,
    sha                  TEXT NOT NULL,
    ref                  TEXT NOT NULL DEFAULT '',
    status               TEXT NOT NULL CHECK (status IN ('pending', 'passed', 'failed')),
    web_url              TEXT,
    pipeline_updated_at  TIMESTAMPTZ NOT NULL,
    created_at           TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at           TIMESTAMPTZ NOT NULL DEFAULT now()
);
