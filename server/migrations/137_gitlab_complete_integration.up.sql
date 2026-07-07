ALTER TABLE gitlab_project_binding
    ADD COLUMN last_refresh_at TIMESTAMPTZ,
    ADD COLUMN last_refresh_error TEXT,
    ADD COLUMN last_event_at TIMESTAMPTZ,
    ADD COLUMN last_event_type TEXT,
    ADD COLUMN refresh_in_progress_at TIMESTAMPTZ;

ALTER TABLE gitlab_merge_request
    ADD COLUMN reviewers JSONB NOT NULL DEFAULT '[]'::jsonb,
    ADD COLUMN assignees JSONB NOT NULL DEFAULT '[]'::jsonb,
    ADD COLUMN labels JSONB NOT NULL DEFAULT '[]'::jsonb,
    ADD COLUMN last_refreshed_at TIMESTAMPTZ,
    ADD COLUMN last_refresh_error TEXT;

CREATE TABLE gitlab_mr_approval_state (
    merge_request_id UUID PRIMARY KEY REFERENCES gitlab_merge_request(id) ON DELETE CASCADE,
    workspace_id UUID NOT NULL REFERENCES workspace(id) ON DELETE CASCADE,
    approved BOOLEAN NOT NULL DEFAULT FALSE,
    approvals_required INTEGER,
    approvals_left INTEGER,
    approved_by JSONB NOT NULL DEFAULT '[]'::jsonb,
    rules JSONB NOT NULL DEFAULT '[]'::jsonb,
    raw_state JSONB,
    fetched_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    FOREIGN KEY (workspace_id, merge_request_id)
        REFERENCES gitlab_merge_request(workspace_id, id) ON DELETE CASCADE
);

CREATE INDEX idx_gitlab_mr_approval_state_workspace
    ON gitlab_mr_approval_state(workspace_id);

CREATE TABLE gitlab_mr_discussion (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    workspace_id UUID NOT NULL REFERENCES workspace(id) ON DELETE CASCADE,
    merge_request_id UUID NOT NULL REFERENCES gitlab_merge_request(id) ON DELETE CASCADE,
    gitlab_discussion_id TEXT NOT NULL,
    individual_note BOOLEAN NOT NULL DEFAULT FALSE,
    resolved BOOLEAN,
    discussion_created_at TIMESTAMPTZ,
    discussion_updated_at TIMESTAMPTZ,
    fetched_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (merge_request_id, gitlab_discussion_id),
    FOREIGN KEY (workspace_id, merge_request_id)
        REFERENCES gitlab_merge_request(workspace_id, id) ON DELETE CASCADE
);

CREATE INDEX idx_gitlab_mr_discussion_mr
    ON gitlab_mr_discussion(merge_request_id);
CREATE INDEX idx_gitlab_mr_discussion_workspace
    ON gitlab_mr_discussion(workspace_id);

CREATE TABLE gitlab_mr_note (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    workspace_id UUID NOT NULL REFERENCES workspace(id) ON DELETE CASCADE,
    discussion_id UUID NOT NULL REFERENCES gitlab_mr_discussion(id) ON DELETE CASCADE,
    merge_request_id UUID NOT NULL REFERENCES gitlab_merge_request(id) ON DELETE CASCADE,
    gitlab_note_id BIGINT NOT NULL,
    author_username TEXT,
    author_avatar_url TEXT,
    body TEXT NOT NULL,
    system BOOLEAN NOT NULL DEFAULT FALSE,
    resolved BOOLEAN,
    resolvable BOOLEAN,
    note_created_at TIMESTAMPTZ,
    note_updated_at TIMESTAMPTZ,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (merge_request_id, gitlab_note_id),
    FOREIGN KEY (workspace_id, merge_request_id)
        REFERENCES gitlab_merge_request(workspace_id, id) ON DELETE CASCADE
);

CREATE INDEX idx_gitlab_mr_note_discussion
    ON gitlab_mr_note(discussion_id);
CREATE INDEX idx_gitlab_mr_note_mr
    ON gitlab_mr_note(merge_request_id);

CREATE TABLE gitlab_pipeline_job (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    workspace_id UUID NOT NULL REFERENCES workspace(id) ON DELETE CASCADE,
    merge_request_id UUID NOT NULL REFERENCES gitlab_merge_request(id) ON DELETE CASCADE,
    pipeline_id BIGINT NOT NULL,
    job_id BIGINT NOT NULL,
    name TEXT NOT NULL,
    stage TEXT,
    status TEXT NOT NULL,
    ref TEXT,
    sha TEXT,
    web_url TEXT,
    started_at TIMESTAMPTZ,
    finished_at TIMESTAMPTZ,
    duration_seconds DOUBLE PRECISION,
    queued_duration_seconds DOUBLE PRECISION,
    failure_reason TEXT,
    allow_failure BOOLEAN NOT NULL DEFAULT FALSE,
    artifacts_file_name TEXT,
    artifacts_file_size BIGINT,
    artifacts_expire_at TIMESTAMPTZ,
    trace_summary TEXT,
    trace_truncated BOOLEAN NOT NULL DEFAULT FALSE,
    trace_fetched_at TIMESTAMPTZ,
    fetched_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (merge_request_id, job_id),
    FOREIGN KEY (workspace_id, merge_request_id)
        REFERENCES gitlab_merge_request(workspace_id, id) ON DELETE CASCADE
);

CREATE INDEX idx_gitlab_pipeline_job_mr
    ON gitlab_pipeline_job(merge_request_id);
CREATE INDEX idx_gitlab_pipeline_job_pipeline
    ON gitlab_pipeline_job(merge_request_id, pipeline_id);
CREATE INDEX idx_gitlab_pipeline_job_status
    ON gitlab_pipeline_job(workspace_id, status);
