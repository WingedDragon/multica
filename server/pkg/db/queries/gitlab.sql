-- Canonical VCS GitLab rows. Legacy HTTP handlers are boundary adapters only.
-- name: GetVCSGitLabMigrationState :one
SELECT * FROM vcs_gitlab_migration_state
WHERE id = 1;

-- Maintenance ingress holds this row lock through receipt commit so Finalize
-- cannot pass its receipt check while an accepted webhook is still in flight.
-- name: GetVCSGitLabMigrationStateForShare :one
SELECT * FROM vcs_gitlab_migration_state
WHERE id = 1
FOR SHARE;


-- name: GetVCSGitLabProject :one
SELECT * FROM vcs_gitlab_project
WHERE id = $1 AND workspace_id = $2;

-- name: GetVCSGitLabProjectByConnectionProject :one
SELECT * FROM vcs_gitlab_project
WHERE workspace_id = $1 AND connection_id = $2 AND gitlab_project_id = $3;


-- name: DeleteVCSGitLabProject :one
DELETE FROM vcs_gitlab_project
WHERE id = $1 AND workspace_id = $2
RETURNING *;

-- name: MarkVCSGitLabProjectEvent :exec
UPDATE vcs_gitlab_project
SET last_event_at = now(), last_event_type = $3, updated_at = now()
WHERE id = $1 AND workspace_id = $2;



-- name: GetGitLabMigrationHookForDelivery :one
SELECT * FROM vcs_gitlab_migration_hook
WHERE workspace_id = sqlc.arg('workspace_id')
  AND connection_id = sqlc.arg('connection_id')
  AND scope_type = sqlc.arg('scope_type')
  AND scope_id = sqlc.arg('scope_id')
  AND (legacy_hook_id = sqlc.arg('hook_id') OR new_hook_id = sqlc.arg('hook_id'))
  AND (
    state IN ('updated', 'verified')
    OR (
      state = 'pending'
      AND new_hook_id = sqlc.arg('hook_id')
      AND target_url <> ''
    )
  );

-- name: ResolveGitLabMigrationID :one
SELECT unified_id FROM vcs_gitlab_migration_id_map
WHERE workspace_id = $1 AND entity_kind = $2 AND legacy_id = $3;


-- name: UpsertVCSGitLabMigrationDelivery :execrows
INSERT INTO vcs_gitlab_migration_delivery (
    delivery_id, workspace_id, connection_id, scope_type, scope_id, scope_path,
    hook_id, event_type, payload
) VALUES (
    $1, $2, $3, $4, $5, $6, $7, $8, sqlc.arg('payload')::jsonb
)
ON CONFLICT (delivery_id, workspace_id, connection_id, scope_type, scope_id, hook_id) DO NOTHING;

-- name: ListGitLabMigrationDeliveriesByID :many
SELECT * FROM vcs_gitlab_migration_delivery
WHERE delivery_id = $1 AND reconciled_at IS NULL
ORDER BY received_at, workspace_id, connection_id, scope_type, scope_id, hook_id;

-- name: GetGitLabMigrationDeliveryForUpdate :one
SELECT * FROM vcs_gitlab_migration_delivery
WHERE delivery_id = $1 AND workspace_id = $2 AND connection_id = $3
  AND scope_type = $4 AND scope_id = $5 AND hook_id = $6
FOR UPDATE;

-- name: MarkGitLabMigrationDeliveryReconciled :execrows
UPDATE vcs_gitlab_migration_delivery
SET reconciled_at = now(), reconciliation_note = $7
WHERE delivery_id = $1 AND workspace_id = $2 AND connection_id = $3
  AND scope_type = $4 AND scope_id = $5 AND hook_id = $6
  AND reconciled_at IS NULL
  AND payload IS NOT NULL;

-- name: GetVCSGitLabProjectByConnectionPath :one
SELECT p.*
FROM vcs_gitlab_project p
JOIN vcs_connection c
  ON c.id = p.connection_id
 AND c.workspace_id = p.workspace_id
 AND c.provider = 'gitlab'
WHERE p.workspace_id = sqlc.arg('workspace_id')
  AND p.connection_id = sqlc.arg('connection_id')
  AND lower(p.path_with_namespace) = lower(sqlc.arg('path_with_namespace'));
