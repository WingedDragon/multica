ALTER TABLE issue
    ADD CONSTRAINT issue_workspace_id_id_key UNIQUE (workspace_id, id);

CREATE TABLE gitlab_connection (
    id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    workspace_id    UUID NOT NULL REFERENCES workspace(id) ON DELETE CASCADE,
    base_url        TEXT NOT NULL,
    host            TEXT NOT NULL,
    connected_by_id UUID REFERENCES "user"(id) ON DELETE SET NULL,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (workspace_id, id),
    UNIQUE (workspace_id, host)
);

CREATE INDEX idx_gitlab_connection_workspace
    ON gitlab_connection(workspace_id);
CREATE INDEX idx_gitlab_connection_host
    ON gitlab_connection(host);

CREATE TABLE gitlab_project_binding (
    id                  UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    workspace_id        UUID NOT NULL REFERENCES workspace(id) ON DELETE CASCADE,
    connection_id       UUID NOT NULL,
    gitlab_project_id   BIGINT NOT NULL,
    path_with_namespace TEXT NOT NULL,
    web_url             TEXT NOT NULL,
    hook_id             BIGINT,
    hook_enabled        BOOLEAN NOT NULL DEFAULT FALSE,
    last_sync_error     TEXT,
    created_at          TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at          TIMESTAMPTZ NOT NULL DEFAULT now(),
    FOREIGN KEY (workspace_id, connection_id)
        REFERENCES gitlab_connection(workspace_id, id) ON DELETE CASCADE,
    UNIQUE (workspace_id, id),
    UNIQUE (workspace_id, id, gitlab_project_id),
    UNIQUE (workspace_id, connection_id, gitlab_project_id)
);

CREATE INDEX idx_gitlab_project_binding_workspace
    ON gitlab_project_binding(workspace_id);
CREATE INDEX idx_gitlab_project_binding_project
    ON gitlab_project_binding(gitlab_project_id);

CREATE TABLE gitlab_merge_request (
    id                    UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    workspace_id          UUID NOT NULL REFERENCES workspace(id) ON DELETE CASCADE,
    project_binding_id    UUID NOT NULL,
    gitlab_project_id     BIGINT NOT NULL,
    mr_iid                INTEGER NOT NULL,
    title                 TEXT NOT NULL,
    description           TEXT,
    state                 TEXT NOT NULL CHECK (state IN ('open', 'closed', 'merged', 'draft')),
    web_url               TEXT NOT NULL,
    source_branch         TEXT,
    target_branch         TEXT,
    author_username       TEXT,
    author_avatar_url     TEXT,
    sha                   TEXT NOT NULL DEFAULT '',
    merge_commit_sha      TEXT,
    detailed_merge_status TEXT,
    has_conflicts         BOOLEAN,
    additions             INTEGER NOT NULL DEFAULT 0,
    deletions             INTEGER NOT NULL DEFAULT 0,
    changed_files         INTEGER NOT NULL DEFAULT 0,
    mr_created_at         TIMESTAMPTZ NOT NULL,
    mr_updated_at         TIMESTAMPTZ NOT NULL,
    merged_at             TIMESTAMPTZ,
    closed_at             TIMESTAMPTZ,
    created_at            TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at            TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (workspace_id, id),
    FOREIGN KEY (workspace_id, project_binding_id, gitlab_project_id)
        REFERENCES gitlab_project_binding(workspace_id, id, gitlab_project_id) ON DELETE CASCADE,
    UNIQUE (workspace_id, project_binding_id, mr_iid)
);

CREATE INDEX idx_gitlab_merge_request_workspace
    ON gitlab_merge_request(workspace_id);
CREATE INDEX idx_gitlab_merge_request_project_sha
    ON gitlab_merge_request(workspace_id, gitlab_project_id, sha);

CREATE TABLE gitlab_mr_pipeline (
    merge_request_id    UUID NOT NULL REFERENCES gitlab_merge_request(id) ON DELETE CASCADE,
    pipeline_id         BIGINT NOT NULL,
    sha                 TEXT NOT NULL,
    ref                 TEXT,
    status              TEXT NOT NULL,
    web_url             TEXT,
    pipeline_updated_at TIMESTAMPTZ NOT NULL,
    created_at          TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at          TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (merge_request_id, pipeline_id)
);

CREATE INDEX idx_gitlab_mr_pipeline_current_sha
    ON gitlab_mr_pipeline(merge_request_id, sha, pipeline_updated_at DESC);

CREATE TABLE issue_gitlab_merge_request (
    workspace_id        UUID NOT NULL REFERENCES workspace(id) ON DELETE CASCADE,
    issue_id           UUID NOT NULL,
    merge_request_id   UUID NOT NULL,
    close_intent       BOOLEAN NOT NULL DEFAULT FALSE,
    linked_by_type     TEXT,
    linked_by_id       UUID,
    linked_at          TIMESTAMPTZ NOT NULL DEFAULT now(),
    FOREIGN KEY (workspace_id, issue_id)
        REFERENCES issue(workspace_id, id) ON DELETE CASCADE,
    FOREIGN KEY (workspace_id, merge_request_id)
        REFERENCES gitlab_merge_request(workspace_id, id) ON DELETE CASCADE,
    PRIMARY KEY (issue_id, merge_request_id)
);

CREATE INDEX idx_issue_gitlab_merge_request_mr
    ON issue_gitlab_merge_request(merge_request_id);
