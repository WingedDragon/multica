-- Additive groundwork for the operator-controlled GitLab → unified VCS cutover.
-- This migration is safe for fresh installs: no old rows, credentials or Hooks are required,
-- and it deliberately does not rewrite or drop the legacy tables. Data moves only when
-- cmd/gitlab-vcs-migrate runs inside the explicit maintenance window.
-- Key constraints are attached in migration 768 after standalone concurrent
-- index builds in migrations 758–767.
CREATE TABLE IF NOT EXISTS vcs_gitlab_project (
    id                  UUID NOT NULL DEFAULT gen_random_uuid(),
    workspace_id        UUID NOT NULL,
    connection_id       UUID NOT NULL,
    gitlab_project_id   BIGINT NOT NULL,
    path_with_namespace TEXT NOT NULL,
    web_url             TEXT NOT NULL,
    hook_id             BIGINT,
    hook_enabled        BOOLEAN NOT NULL DEFAULT FALSE,
    legacy_link_policy BOOLEAN NOT NULL DEFAULT FALSE,
    last_sync_error     TEXT,
    last_refresh_at     TIMESTAMPTZ,
    last_refresh_error  TEXT,
    last_event_at       TIMESTAMPTZ,
    last_event_type     TEXT,
    refresh_in_progress_at TIMESTAMPTZ,
    created_at          TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at          TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS vcs_gitlab_migration_id_map (
    workspace_id UUID NOT NULL,
    entity_kind  TEXT NOT NULL CHECK (entity_kind IN ('connection', 'pull_request')),
    legacy_id    UUID NOT NULL,
    unified_id   UUID NOT NULL,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- Exactly one row (id = 1) gates all writes and wakeup processing during cutover.
-- An absent row is not permission to bypass the gate when legacy GitLab rows exist.
CREATE TABLE IF NOT EXISTS vcs_gitlab_migration_state (
    id               SMALLINT NOT NULL DEFAULT 1 CHECK (id = 1),
    phase            TEXT NOT NULL CHECK (phase IN (
                         'maintenance', 'database_migrated', 'hooks_pending',
                         'verification_pending', 'ready', 'complete',
                         'restoring', 'restore_blocked', 'restored'
                     )),
    maintenance_mode BOOLEAN NOT NULL,
    generation       BIGINT NOT NULL DEFAULT 0,
    began_at         TIMESTAMPTZ,
    updated_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    last_error       TEXT
);
-- First-write before-images make a failed cutover reversible without restoring
-- a stale full database backup over concurrent, unrelated application state.
-- Any credential values in before_image must remain their encrypted ciphertext.
CREATE TABLE IF NOT EXISTS vcs_gitlab_migration_snapshot (
    generation   BIGINT NOT NULL,
    entity_kind  TEXT NOT NULL,
    workspace_id UUID NOT NULL,
    record_key   TEXT NOT NULL,
    existed      BOOLEAN NOT NULL,
    before_image JSONB,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    CHECK ((existed AND before_image IS NOT NULL) OR (NOT existed AND before_image IS NULL))
);

-- Hook snapshots omit API tokens and Hook tokens. The legacy webhook secret is
-- sealed with MULTICA_VCS_SECRET_KEY before switching and kept only for rollback.
CREATE TABLE IF NOT EXISTS vcs_gitlab_migration_hook (
    connection_id          UUID NOT NULL,
    workspace_id           UUID NOT NULL,
    scope_type             TEXT NOT NULL CHECK (scope_type IN ('project', 'group', 'system')),
    scope_id               TEXT NOT NULL,
    scope_path             TEXT NOT NULL DEFAULT '',
    legacy_hook_id         BIGINT NOT NULL DEFAULT 0,
    new_hook_id            BIGINT,
    managed                BOOLEAN NOT NULL,
    created_by_tool        BOOLEAN NOT NULL DEFAULT FALSE,
    previous_config        JSONB NOT NULL DEFAULT '{}'::jsonb,
    previous_secret_encrypted TEXT NOT NULL DEFAULT '',
    target_url             TEXT NOT NULL DEFAULT '',
    state                  TEXT NOT NULL CHECK (state IN (
                               'pending', 'updated', 'verified', 'manual_required',
                               'restoring', 'restored', 'restore_blocked'
                           )),
    verified_delivery_id   TEXT,
    verified_at            TIMESTAMPTZ,
    last_error             TEXT,
    created_at             TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at             TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- Maintenance ingress authenticates and authorizes before persisting a receipt,
-- then returns 503 so GitLab retries; it never acknowledges by returning 2xx.
-- One delivery can map to multiple workspace-scoped connections.
CREATE TABLE IF NOT EXISTS vcs_gitlab_migration_delivery (
    delivery_id       TEXT NOT NULL,
    workspace_id      UUID NOT NULL,
    connection_id     UUID NOT NULL,
    scope_type        TEXT NOT NULL CHECK (scope_type IN ('project', 'group', 'system')),
    scope_id          TEXT NOT NULL,
    scope_path        TEXT NOT NULL DEFAULT '',
    hook_id           BIGINT NOT NULL,
    event_type        TEXT NOT NULL,
    received_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    payload           JSONB,
    reconciled_at     TIMESTAMPTZ,
    reconciliation_note TEXT
);

-- Canonical core MR state lives only in vcs_pull_request. This table holds
-- GitLab-specific attributes formerly stored on gitlab_merge_request.
CREATE TABLE IF NOT EXISTS vcs_gitlab_mr_detail (
    vcs_pull_request_id UUID NOT NULL,
    description         TEXT,
    merge_commit_sha    TEXT,
    detailed_merge_status TEXT,
    target_branch       TEXT,
    reviewers           JSONB NOT NULL DEFAULT '[]'::jsonb,
    assignees           JSONB NOT NULL DEFAULT '[]'::jsonb,
    labels              JSONB NOT NULL DEFAULT '[]'::jsonb,
    last_refreshed_at   TIMESTAMPTZ,
    last_refresh_error  TEXT,
    conflict_sha        TEXT NOT NULL DEFAULT '',
    pipeline_sync_at    TIMESTAMPTZ,
    pipeline_sync_error TEXT,
    wakeup_sync_generation BIGINT NOT NULL DEFAULT 0,
    created_at          TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at          TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- Existing GitLab detail rows remain addressable while the migration tool is
-- reversible. New runtime reads use only this canonical MR identity.
ALTER TABLE gitlab_mr_approval_state
    ADD COLUMN IF NOT EXISTS vcs_pull_request_id UUID;
ALTER TABLE gitlab_mr_discussion
    ADD COLUMN IF NOT EXISTS vcs_pull_request_id UUID;
ALTER TABLE gitlab_mr_note
    ADD COLUMN IF NOT EXISTS vcs_pull_request_id UUID;
ALTER TABLE gitlab_pipeline_job
    ADD COLUMN IF NOT EXISTS vcs_pull_request_id UUID;
ALTER TABLE issue_vcs_pull_request
    ADD COLUMN IF NOT EXISTS gitlab_close_intent BOOLEAN NOT NULL DEFAULT FALSE,
    ADD COLUMN IF NOT EXISTS gitlab_linked_by_type TEXT,
    ADD COLUMN IF NOT EXISTS gitlab_linked_by_id UUID,
    ADD COLUMN IF NOT EXISTS gitlab_linked_at TIMESTAMPTZ;
ALTER TABLE issue_vcs_pull_request
    ADD COLUMN IF NOT EXISTS vcs_linked BOOLEAN NOT NULL DEFAULT TRUE;

