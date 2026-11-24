package gitlabmigration

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/util/secretbox"
)

type legacyConnection struct {
	ID, WorkspaceID, BaseURL, Host, ConnectedBy string
	CreatedAt, UpdatedAt                        time.Time
	WebhookSecretEncrypted                      string
}

type connectionMapping struct {
	Legacy legacyConnection
	ID     string
	Login  string
}

type projectMapping struct {
	ID           string
	WorkspaceID  string
	ConnectionID string
	ProjectID    int64
	Path         string
	WebURL       string
	HookID       pgtype.Int8
}

func (r *Runner) Migrate(ctx context.Context) error {
	if _, err := r.Preflight(ctx); err != nil {
		return fmt.Errorf("GitLab preflight failed: %w", err)
	}
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin GitLab data migration: %w", err)
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, "SELECT pg_advisory_xact_lock($1)", migrationAdvisoryLock); err != nil {
		return fmt.Errorf("lock GitLab data migration: %w", err)
	}
	state, err := readState(ctx, tx)
	if err != nil {
		return err
	}
	switch state.Phase {
	case "complete":
		return tx.Commit(ctx)
	case "database_migrated", "hooks_pending", "verification_pending", "ready":
		return tx.Commit(ctx)
	case "maintenance":
		if !state.MaintenanceMode {
			return errors.New("GitLab database migration requires maintenance mode")
		}
	default:
		return fmt.Errorf("cannot migrate GitLab data from phase %q", state.Phase)
	}
	if !r.env.VCSIntegrationEnabled {
		var hasLegacyData bool
		if err := tx.QueryRow(ctx, `
			SELECT EXISTS (SELECT 1 FROM gitlab_connection)
			    OR EXISTS (SELECT 1 FROM gitlab_project_binding)
			    OR EXISTS (SELECT 1 FROM gitlab_merge_request)
			    OR EXISTS (SELECT 1 FROM gitlab_mr_pipeline)
			    OR EXISTS (SELECT 1 FROM issue_gitlab_merge_request)
			    OR EXISTS (
					SELECT 1 FROM chat_wakeup
					WHERE status='active' AND condition->>'provider'='gitlab'
					  AND condition->'gitlab'->>'source'='gitlab_connection'
				)
		`).Scan(&hasLegacyData); err != nil {
			return fmt.Errorf("check legacy GitLab data before migration: %w", err)
		}
		if hasLegacyData {
			return errors.New("MULTICA_VCS_INTEGRATION_ENABLED=true is required to migrate legacy GitLab data")
		}
	}
	box, err := r.env.box()
	if err != nil {
		return err
	}
	connections, err := r.importConnections(ctx, tx, box)
	if err != nil {
		return err
	}
	projects, err := r.importProjects(ctx, tx, connections)
	if err != nil {
		return err
	}
	mappings, err := r.importMergeRequests(ctx, tx, connections, projects)
	if err != nil {
		return err
	}
	if err := r.importPipelines(ctx, tx, connections, projects, mappings); err != nil {
		return err
	}
	if err := r.importIssueLinks(ctx, tx, mappings); err != nil {
		return err
	}
	if err := r.importWakeups(ctx, tx, mappings); err != nil {
		return err
	}
	if err := r.attachGitLabDetails(ctx, tx); err != nil {
		return err
	}
	if err := verifyImportedRows(ctx, tx); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `
		UPDATE vcs_gitlab_migration_state
		SET phase='database_migrated', maintenance_mode=true,
		    updated_at=now(), last_error=NULL
		WHERE id=1 AND phase='maintenance'
	`); err != nil {
		return fmt.Errorf("record committed GitLab migration phase: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit GitLab data migration: %w", err)
	}
	return nil
}

func (r *Runner) importConnections(ctx context.Context, tx pgx.Tx, box *secretbox.Box) (map[string]connectionMapping, error) {
	rows, err := tx.Query(ctx, `
		SELECT id::text, workspace_id::text, base_url, host,
		       COALESCE(connected_by_id::text, ''), created_at, updated_at,
		       access_token_encrypted, webhook_secret_encrypted
		FROM gitlab_connection ORDER BY workspace_id, id
	`)
	if err != nil {
		return nil, fmt.Errorf("read legacy GitLab connections: %w", err)
	}
	type encryptedConnection struct {
		legacyConnection
		encryptedToken, encryptedSecret string
	}
	var legacyRows []encryptedConnection
	for rows.Next() {
		var item encryptedConnection
		if err := rows.Scan(&item.ID, &item.WorkspaceID, &item.BaseURL, &item.Host, &item.ConnectedBy,
			&item.CreatedAt, &item.UpdatedAt, &item.encryptedToken, &item.encryptedSecret); err != nil {
			rows.Close()
			return nil, fmt.Errorf("read legacy GitLab connection: %w", err)
		}
		legacyRows = append(legacyRows, item)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	rows.Close()
	connections := make(map[string]connectionMapping, len(legacyRows))
	for _, item := range legacyRows {
		old := item.legacyConnection
		token, _, restoreSecret, err := r.env.legacyCredentials(box, old.BaseURL, item.encryptedToken, item.encryptedSecret)
		if err != nil {
			return nil, err
		}
		old.WebhookSecretEncrypted = restoreSecret
		api, err := newGitLabAPI(old.BaseURL, token, r.env.GitLabProxyURL)
		if err != nil {
			return nil, err
		}
		login, err := api.currentUser(ctx)
		if err != nil {
			return nil, fmt.Errorf("validate GitLab instance %s: %w", safeInstanceURL(old.BaseURL), err)
		}
		id, err := r.findOrCreateConnection(ctx, tx, box, old, login, token, item.encryptedToken, restoreSecret)
		if err != nil {
			return nil, err
		}
		if err := upsertIDMapping(ctx, tx, old.WorkspaceID, "connection", old.ID, id); err != nil {
			return nil, err
		}
		connections[workspaceIdentity(old.WorkspaceID, old.ID)] = connectionMapping{Legacy: old, ID: id, Login: login}
	}
	return connections, nil
}

func (r *Runner) findOrCreateConnection(ctx context.Context, tx pgx.Tx, box *secretbox.Box, old legacyConnection, login, token, tokenCiphertext, hookSecretEncrypted string) (string, error) {
	rows, err := tx.Query(ctx, `
		SELECT id::text, provider, instance_url, access_token_encrypted, webhook_secret_encrypted
		FROM vcs_connection WHERE workspace_id=$1
	`, old.WorkspaceID)
	if err != nil {
		return "", fmt.Errorf("check unified GitLab connection: %w", err)
	}
	var existingID string
	for rows.Next() {
		var id, provider, instanceURL, encryptedToken, encryptedSecret string
		if err := rows.Scan(&id, &provider, &instanceURL, &encryptedToken, &encryptedSecret); err != nil {
			rows.Close()
			return "", err
		}
		if normalizeInstanceURL(instanceURL) != normalizeInstanceURL(old.BaseURL) {
			continue
		}
		if existingID != "" {
			rows.Close()
			return "", fmt.Errorf("workspace %s has multiple normalized VCS connections for %s", old.WorkspaceID, safeInstanceURL(old.BaseURL))
		}
		if provider != "gitlab" {
			rows.Close()
			return "", fmt.Errorf("workspace %s has a non-GitLab VCS connection for %s", old.WorkspaceID, safeInstanceURL(old.BaseURL))
		}
		storedToken, err := openSecret(box, encryptedToken)
		if err != nil {
			rows.Close()
			return "", err
		}
		if _, err := openSecret(box, encryptedSecret); err != nil {
			rows.Close()
			return "", err
		}
		if storedToken != token {
			rows.Close()
			return "", fmt.Errorf("workspace %s has a different GitLab API account for %s", old.WorkspaceID, safeInstanceURL(old.BaseURL))
		}
		existingID = id
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return "", err
	}
	rows.Close()
	if existingID != "" {
		return existingID, nil
	}
	id := old.ID
	var idExists bool
	if err := tx.QueryRow(ctx, "SELECT EXISTS (SELECT 1 FROM vcs_connection WHERE id=$1)", id).Scan(&idExists); err != nil {
		return "", err
	}
	if idExists {
		if err := tx.QueryRow(ctx, "SELECT gen_random_uuid()::text").Scan(&id); err != nil {
			return "", err
		}
	}
	if tokenCiphertext == "" {
		tokenCiphertext, err = sealSecret(box, token)
		if err != nil {
			return "", err
		}
	}
	hookCiphertext := hookSecretEncrypted
	if hookCiphertext == "" {
		return "", errors.New("legacy GitLab webhook secret ciphertext is empty")
	}
	if err := snapshotRow(ctx, tx, "connection", old.WorkspaceID, id,
		`SELECT to_jsonb(c) FROM vcs_connection c WHERE workspace_id=$1 AND id=$2`, old.WorkspaceID, id); err != nil {
		return "", fmt.Errorf("snapshot unified GitLab connection %s: %w", id, err)
	}
	_, err = tx.Exec(ctx, `
		INSERT INTO vcs_connection (
			id, workspace_id, provider, instance_url, account_login,
			access_token_encrypted, webhook_secret_encrypted, connected_by_id,
			created_at, updated_at
		) VALUES ($1,$2,'gitlab',$3,$4,$5,$6,NULLIF($7,'')::uuid,$8,$9)
	`, id, old.WorkspaceID, normalizeInstanceURL(old.BaseURL), login, tokenCiphertext, hookCiphertext, old.ConnectedBy, old.CreatedAt, old.UpdatedAt)
	if err != nil {
		return "", fmt.Errorf("create encrypted unified GitLab connection for workspace %s: %w", old.WorkspaceID, err)
	}
	return id, nil
}

func (r *Runner) importProjects(ctx context.Context, tx pgx.Tx, connections map[string]connectionMapping) (map[string]projectMapping, error) {
	rows, err := tx.Query(ctx, `
		SELECT pb.id::text, pb.workspace_id::text, pb.connection_id::text, pb.gitlab_project_id,
		       pb.path_with_namespace, pb.web_url, pb.hook_id, pb.hook_enabled,
		       pb.last_sync_error, pb.last_refresh_at, pb.last_refresh_error,
		       pb.last_event_at, pb.last_event_type, pb.refresh_in_progress_at,
		       pb.created_at, pb.updated_at
		FROM gitlab_project_binding pb ORDER BY pb.workspace_id, pb.connection_id, pb.gitlab_project_id
	`)
	if err != nil {
		return nil, fmt.Errorf("read legacy GitLab projects: %w", err)
	}
	type legacyProject struct {
		projectMapping
		hookEnabled                               bool
		syncError, refreshError, eventType        pgtype.Text
		lastRefresh, lastEvent, refreshInProgress pgtype.Timestamptz
		createdAt, updatedAt                      time.Time
	}
	var legacyRows []legacyProject
	for rows.Next() {
		var item legacyProject
		if err := rows.Scan(
			&item.ID, &item.WorkspaceID, &item.ConnectionID, &item.ProjectID, &item.Path, &item.WebURL, &item.HookID, &item.hookEnabled,
			&item.syncError, &item.lastRefresh, &item.refreshError, &item.lastEvent, &item.eventType, &item.refreshInProgress, &item.createdAt, &item.updatedAt,
		); err != nil {
			rows.Close()
			return nil, err
		}
		legacyRows = append(legacyRows, item)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	rows.Close()
	projects := make(map[string]projectMapping, len(legacyRows))
	for _, item := range legacyRows {
		old := item.projectMapping
		legacyID := old.ID
		connection, ok := connections[workspaceIdentity(old.WorkspaceID, old.ConnectionID)]
		if !ok || connection.Legacy.WorkspaceID != old.WorkspaceID {
			return nil, fmt.Errorf("GitLab project %s has no unambiguous workspace connection mapping", old.Path)
		}
		old.ConnectionID = connection.ID
		canonicalID, err := r.findOrCreateProject(ctx, tx, old, item.hookEnabled, item.syncError, item.lastRefresh, item.refreshError,
			item.lastEvent, item.eventType, item.refreshInProgress, item.createdAt, item.updatedAt)
		if err != nil {
			return nil, err
		}
		old.ID = canonicalID
		if err := r.registerManagedProjectHook(ctx, tx, old, connection.Legacy.WebhookSecretEncrypted); err != nil {
			return nil, err
		}
		projects[workspaceIdentity(old.WorkspaceID, legacyID)] = old
	}
	return projects, nil
}

func (r *Runner) findOrCreateProject(ctx context.Context, tx pgx.Tx, project projectMapping, hookEnabled bool, syncError pgtype.Text, lastRefresh pgtype.Timestamptz, refreshError pgtype.Text, lastEvent pgtype.Timestamptz, eventType pgtype.Text, refreshInProgress pgtype.Timestamptz, createdAt, updatedAt time.Time) (string, error) {
	var id, path string
	var legacyLinkPolicy bool
	var currentSyncError, currentRefreshError, currentEventType pgtype.Text
	var currentLastRefresh, currentLastEvent, currentRefreshInProgress pgtype.Timestamptz
	var currentCreatedAt, currentUpdatedAt time.Time
	err := tx.QueryRow(ctx, `
		SELECT id::text,path_with_namespace,legacy_link_policy,last_sync_error,
		       last_refresh_at,last_refresh_error,last_event_at,last_event_type,
		       refresh_in_progress_at,created_at,updated_at
		FROM vcs_gitlab_project
		WHERE workspace_id=$1 AND connection_id=$2 AND gitlab_project_id=$3
		FOR UPDATE
	`, project.WorkspaceID, project.ConnectionID, project.ProjectID).Scan(
		&id, &path, &legacyLinkPolicy, &currentSyncError, &currentLastRefresh, &currentRefreshError,
		&currentLastEvent, &currentEventType, &currentRefreshInProgress, &currentCreatedAt, &currentUpdatedAt)
	if err == nil {
		if path != project.Path {
			return "", fmt.Errorf("GitLab project %d path conflict: stored %q, legacy %q", project.ProjectID, path, project.Path)
		}
		if updatedAt.Equal(currentUpdatedAt) && !sameText(currentSyncError, syncError) {
			return "", fmt.Errorf("GitLab project %d has conflicting same-version sync errors: %w", project.ProjectID, errAmbiguousMR)
		}
		mergedSyncError := currentSyncError
		if updatedAt.After(currentUpdatedAt) {
			mergedSyncError = syncError
		}
		mergedLastRefresh, mergedRefreshError, err := newerSync(currentLastRefresh, currentRefreshError, lastRefresh, refreshError)
		if err != nil {
			return "", fmt.Errorf("GitLab project %d has conflicting refresh versions: %w", project.ProjectID, err)
		}
		mergedLastEvent, mergedEventType, err := newerSync(currentLastEvent, currentEventType, lastEvent, eventType)
		if err != nil {
			return "", fmt.Errorf("GitLab project %d has conflicting event versions: %w", project.ProjectID, err)
		}
		mergedRefreshInProgress := currentRefreshInProgress
		if refreshInProgress.Valid && (!mergedRefreshInProgress.Valid || refreshInProgress.Time.After(mergedRefreshInProgress.Time)) {
			mergedRefreshInProgress = refreshInProgress
		}
		mergedCreatedAt, mergedUpdatedAt := currentCreatedAt, currentUpdatedAt
		if createdAt.Before(mergedCreatedAt) {
			mergedCreatedAt = createdAt
		}
		if updatedAt.After(mergedUpdatedAt) {
			mergedUpdatedAt = updatedAt
		}
		if err := snapshotRow(ctx, tx, "project", project.WorkspaceID, id,
			`SELECT to_jsonb(p) FROM vcs_gitlab_project p WHERE workspace_id=$1 AND id=$2 FOR UPDATE`, project.WorkspaceID, id); err != nil {
			return "", fmt.Errorf("snapshot overlapping GitLab project %s: %w", project.Path, err)
		}
		if _, err := tx.Exec(ctx, `
			UPDATE vcs_gitlab_project SET legacy_link_policy=true,last_sync_error=$3,
				last_refresh_at=$4,last_refresh_error=$5,last_event_at=$6,last_event_type=$7,
				refresh_in_progress_at=$8,created_at=$9,updated_at=$10
			WHERE workspace_id=$1 AND id=$2
		`, project.WorkspaceID, id, mergedSyncError, mergedLastRefresh, mergedRefreshError, mergedLastEvent,
			mergedEventType, mergedRefreshInProgress, mergedCreatedAt, mergedUpdatedAt); err != nil {
			return "", fmt.Errorf("merge overlapping GitLab project %s: %w", project.Path, err)
		}
		return id, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return "", err
	}
	id = project.ID
	var idExists bool
	if err := tx.QueryRow(ctx, "SELECT EXISTS (SELECT 1 FROM vcs_gitlab_project WHERE id=$1)", id).Scan(&idExists); err != nil {
		return "", err
	}
	if idExists {
		if err := tx.QueryRow(ctx, "SELECT gen_random_uuid()::text").Scan(&id); err != nil {
			return "", err
		}
	}
	if err := snapshotRow(ctx, tx, "project", project.WorkspaceID, id,
		`SELECT to_jsonb(p) FROM vcs_gitlab_project p WHERE workspace_id=$1 AND id=$2`, project.WorkspaceID, id); err != nil {
		return "", fmt.Errorf("snapshot new GitLab project %s: %w", project.Path, err)
	}
	_, err = tx.Exec(ctx, `
		INSERT INTO vcs_gitlab_project (
			id, workspace_id, connection_id, gitlab_project_id, path_with_namespace, web_url,
			hook_id, hook_enabled, last_sync_error, last_refresh_at, last_refresh_error,
			last_event_at, last_event_type, refresh_in_progress_at, created_at, updated_at, legacy_link_policy
		) VALUES (
			$1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,true
		)
	`, id, project.WorkspaceID, project.ConnectionID, project.ProjectID, project.Path, project.WebURL,
		project.HookID, hookEnabled, syncError, lastRefresh, refreshError, lastEvent, eventType, refreshInProgress, createdAt, updatedAt)
	if err != nil {
		return "", fmt.Errorf("create unified GitLab project %s: %w", project.Path, err)
	}
	return id, nil
}

func (r *Runner) registerManagedProjectHook(ctx context.Context, tx pgx.Tx, project projectMapping, previousSecretEncrypted string) error {
	legacyHookID := int64(0)
	if project.HookID.Valid {
		legacyHookID = project.HookID.Int64
	}
	_, err := tx.Exec(ctx, `
		INSERT INTO vcs_gitlab_migration_hook (
			connection_id,workspace_id,scope_type,scope_id,scope_path,legacy_hook_id,
			new_hook_id,managed,previous_config,previous_secret_encrypted,target_url,state
		) VALUES ($1,$2,'project',$3,$4,$5,NULL,true,'{}'::jsonb,$6,'','pending')
		ON CONFLICT (connection_id,scope_type,scope_id,legacy_hook_id) DO UPDATE
		SET previous_secret_encrypted=CASE WHEN vcs_gitlab_migration_hook.previous_secret_encrypted='' THEN EXCLUDED.previous_secret_encrypted ELSE vcs_gitlab_migration_hook.previous_secret_encrypted END
	`, project.ConnectionID, project.WorkspaceID, fmt.Sprint(project.ProjectID), project.Path, legacyHookID, previousSecretEncrypted)
	if err != nil {
		return fmt.Errorf("record managed project Hook for %s: %w", project.Path, err)
	}
	return nil
}

func upsertIDMapping(ctx context.Context, tx pgx.Tx, workspaceID, kind, legacyID, unifiedID string) error {
	var existing string
	err := tx.QueryRow(ctx, `
		SELECT unified_id::text FROM vcs_gitlab_migration_id_map
		WHERE workspace_id=$1 AND entity_kind=$2 AND legacy_id=$3
	`, workspaceID, kind, legacyID).Scan(&existing)
	if err == nil {
		if existing != unifiedID {
			return fmt.Errorf("workspace-scoped GitLab %s ID mapping for %s changed from %s to %s", kind, legacyID, existing, unifiedID)
		}
		return nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return err
	}
	var otherLegacyID string
	err = tx.QueryRow(ctx, `
		SELECT legacy_id::text FROM vcs_gitlab_migration_id_map
		WHERE workspace_id=$1 AND entity_kind=$2 AND unified_id=$3
	`, workspaceID, kind, unifiedID).Scan(&otherLegacyID)
	if err == nil {
		return fmt.Errorf("workspace-scoped GitLab %s identity %s is already mapped from legacy ID %s", kind, unifiedID, otherLegacyID)
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return err
	}
	if err := snapshotRow(ctx, tx, "id_mapping", workspaceID, kind+"|"+legacyID,
		`SELECT to_jsonb(m) FROM vcs_gitlab_migration_id_map m WHERE workspace_id=$1 AND entity_kind=$2 AND legacy_id=$3`,
		workspaceID, kind, legacyID); err != nil {
		return fmt.Errorf("snapshot GitLab %s ID mapping %s: %w", kind, legacyID, err)
	}
	_, err = tx.Exec(ctx, `
		INSERT INTO vcs_gitlab_migration_id_map (workspace_id,entity_kind,legacy_id,unified_id)
		VALUES ($1,$2,$3,$4)
	`, workspaceID, kind, legacyID, unifiedID)
	if err != nil {
		return fmt.Errorf("write workspace-scoped GitLab %s ID mapping: %w", kind, err)
	}
	return nil
}

func snapshotRow(ctx context.Context, tx pgx.Tx, kind, workspaceID, key, selectSQL string, args ...any) error {
	var beforeImage []byte
	err := tx.QueryRow(ctx, selectSQL, args...).Scan(&beforeImage)
	existed := err == nil
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return err
	}
	var image any
	if existed {
		image = string(beforeImage)
	}
	_, err = tx.Exec(ctx, `
		INSERT INTO vcs_gitlab_migration_snapshot
			(generation,entity_kind,workspace_id,record_key,existed,before_image)
		SELECT generation,$1,$2,$3,$4,$5::jsonb
		FROM vcs_gitlab_migration_state WHERE id=1
		ON CONFLICT (generation,entity_kind,workspace_id,record_key) DO NOTHING
	`, kind, workspaceID, key, existed, image)
	if err != nil {
		return err
	}
	var saved bool
	if err := tx.QueryRow(ctx, `
		SELECT EXISTS (
			SELECT 1 FROM vcs_gitlab_migration_snapshot s
			JOIN vcs_gitlab_migration_state state ON state.id=1 AND state.generation=s.generation
			WHERE s.entity_kind=$1 AND s.workspace_id=$2 AND s.record_key=$3
		)
	`, kind, workspaceID, key).Scan(&saved); err != nil {
		return err
	}
	if !saved {
		return errors.New("GitLab migration snapshot was not stored for the current generation")
	}
	return nil
}
func databaseInstanceKey(raw string) string { return strings.TrimSpace(normalizeInstanceURL(raw)) }

func validPipelineStatus(status string) string {
	switch strings.ToLower(strings.TrimSpace(status)) {
	case "success", "passed", "skipped":
		return "passed"
	case "failed", "canceled", "cancelled":
		return "failed"
	default:
		return "pending"
	}
}

func nullableInt(value pgtype.Int8) any {
	if !value.Valid {
		return nil
	}
	return value.Int64
}

func nullableText(value pgtype.Text) any {
	if !value.Valid {
		return nil
	}
	return value.String
}

func nullableBool(value pgtype.Bool) any {
	if !value.Valid {
		return nil
	}
	return value.Bool
}

type databaseSnapshot struct {
	kind, workspaceID, recordKey string
	existed                      bool
	beforeImage                  []byte
}

// RestoreDatabase rolls back only rows touched by one GitLab migration
// attempt. It is safe to retry while the service remains in maintenance.
func (r *Runner) RestoreDatabase(ctx context.Context) error {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin GitLab database restore: %w", err)
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, "SELECT pg_advisory_xact_lock($1)", migrationAdvisoryLock); err != nil {
		return fmt.Errorf("lock GitLab database restore: %w", err)
	}
	state, err := readState(ctx, tx)
	if err != nil {
		return err
	}
	if !state.MaintenanceMode || (state.Phase != "restoring" && state.Phase != "restore_blocked" && state.Phase != "restored") {
		return fmt.Errorf("GitLab database restore requires maintenance mode, got phase %q", state.Phase)
	}
	// Restore canonical detail before-images before legacy rows to release
	// unique GitLab keys changed by the legacy-ID compatibility backfill.
	rows, err := tx.Query(ctx, `
		SELECT entity_kind,workspace_id::text,record_key,existed,before_image
		FROM vcs_gitlab_migration_snapshot
		WHERE generation=$1
		ORDER BY CASE
			WHEN entity_kind='pull_request' AND NOT existed THEN 1
			WHEN entity_kind='project' AND NOT existed THEN 2
			WHEN entity_kind='connection' AND NOT existed THEN 3
			WHEN entity_kind='connection' THEN 4
			WHEN entity_kind='project' THEN 5
			WHEN entity_kind='pull_request' THEN 6
			WHEN entity_kind LIKE 'legacy_detail:%' THEN 7
			WHEN entity_kind='gitlab_detail' THEN 8
			WHEN entity_kind='pipeline' THEN 9
			WHEN entity_kind='issue_link' THEN 10
			WHEN entity_kind='chat_wakeup' THEN 11
			WHEN entity_kind='id_mapping' THEN 12
			ELSE 13
		END,entity_kind,workspace_id,
		CASE WHEN entity_kind LIKE 'legacy_detail:%' AND before_image->>'vcs_pull_request_id' IS NOT NULL THEN 0 ELSE 1 END,
		record_key
	`, state.Generation)
	if err != nil {
		return fmt.Errorf("read GitLab database before-images: %w", err)
	}
	var snapshots []databaseSnapshot
	for rows.Next() {
		var snapshot databaseSnapshot
		if err := rows.Scan(&snapshot.kind, &snapshot.workspaceID, &snapshot.recordKey, &snapshot.existed, &snapshot.beforeImage); err != nil {
			rows.Close()
			return fmt.Errorf("read GitLab database before-image: %w", err)
		}
		snapshot.beforeImage = append([]byte(nil), snapshot.beforeImage...)
		snapshots = append(snapshots, snapshot)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return fmt.Errorf("iterate GitLab database before-images: %w", err)
	}
	rows.Close()
	detailUpdateQueries := make(map[string]string)
	for _, snapshot := range snapshots {
		if !strings.HasPrefix(snapshot.kind, "legacy_detail:") {
			continue
		}
		table := strings.TrimPrefix(snapshot.kind, "legacy_detail:")
		switch table {
		case "gitlab_mr_discussion", "gitlab_mr_note", "gitlab_pipeline_job":
			if _, ok := detailUpdateQueries[table]; ok {
				continue
			}
			query, err := legacyDetailRestoreUpdateQuery(ctx, tx, table)
			if err != nil {
				return fmt.Errorf("prepare %s restore: %w", table, err)
			}
			detailUpdateQueries[table] = query
		}
	}

	for _, snapshot := range snapshots {
		if err := restoreDatabaseSnapshot(ctx, tx, snapshot, detailUpdateQueries); err != nil {
			return fmt.Errorf("restore GitLab %s row %q in workspace %s: %w", snapshot.kind, snapshot.recordKey, snapshot.workspaceID, err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit GitLab database restore: %w", err)
	}
	return nil
}

func restoreDatabaseSnapshot(ctx context.Context, tx pgx.Tx, snapshot databaseSnapshot, detailUpdateQueries map[string]string) error {
	switch snapshot.kind {
	case "connection":
		return restoreFullSnapshot(ctx, tx, snapshot, "vcs_connection",
			`DELETE FROM vcs_connection WHERE workspace_id=$1 AND id=$2`, []any{snapshot.workspaceID, snapshot.recordKey})
	case "project":
		return restoreFullSnapshot(ctx, tx, snapshot, "vcs_gitlab_project",
			`DELETE FROM vcs_gitlab_project WHERE workspace_id=$1 AND id=$2`, []any{snapshot.workspaceID, snapshot.recordKey})
	case "pull_request":
		return restoreFullSnapshot(ctx, tx, snapshot, "vcs_pull_request",
			`DELETE FROM vcs_pull_request WHERE workspace_id=$1 AND id=$2`, []any{snapshot.workspaceID, snapshot.recordKey})
	case "gitlab_detail":
		return restoreFullSnapshot(ctx, tx, snapshot, "vcs_gitlab_mr_detail",
			`DELETE FROM vcs_gitlab_mr_detail WHERE vcs_pull_request_id=$1`, []any{snapshot.recordKey})
	case "pipeline":
		parts := strings.SplitN(snapshot.recordKey, "|", 3)
		if len(parts) != 3 {
			return fmt.Errorf("invalid pipeline snapshot identity")
		}
		pipelineID, err := strconv.ParseInt(parts[2], 10, 64)
		if err != nil {
			return fmt.Errorf("invalid pipeline snapshot ID: %w", err)
		}
		return restoreFullSnapshot(ctx, tx, snapshot, "vcs_gitlab_pipeline",
			`DELETE FROM vcs_gitlab_pipeline WHERE workspace_id=$1 AND pull_request_id=$2 AND pipeline_project_path=$3 AND pipeline_id=$4`,
			[]any{snapshot.workspaceID, parts[0], parts[1], pipelineID})
	case "issue_link":
		parts := strings.SplitN(snapshot.recordKey, "|", 2)
		if len(parts) != 2 {
			return fmt.Errorf("invalid issue-link snapshot identity")
		}
		return restoreFullSnapshot(ctx, tx, snapshot, "issue_vcs_pull_request",
			`DELETE FROM issue_vcs_pull_request WHERE issue_id=$1 AND pull_request_id=$2`, []any{parts[0], parts[1]})
	case "chat_wakeup":
		if !snapshot.existed {
			return errors.New("chat wakeup before-image must preserve an existing row")
		}
		_, err := tx.Exec(ctx, `
			UPDATE chat_wakeup SET condition=($3::jsonb->'condition'),condition_state=($3::jsonb->>'condition_state')
			WHERE workspace_id=$1 AND id=$2
		`, snapshot.workspaceID, snapshot.recordKey, string(snapshot.beforeImage))
		return err
	case "id_mapping":
		parts := strings.SplitN(snapshot.recordKey, "|", 2)
		if len(parts) != 2 {
			return fmt.Errorf("invalid ID-mapping snapshot identity")
		}
		return restoreFullSnapshot(ctx, tx, snapshot, "vcs_gitlab_migration_id_map",
			`DELETE FROM vcs_gitlab_migration_id_map WHERE workspace_id=$1 AND entity_kind=$2 AND legacy_id=$3`,
			[]any{snapshot.workspaceID, parts[0], parts[1]})
	default:
		if strings.HasPrefix(snapshot.kind, "legacy_detail:") {
			return restoreLegacyDetailSnapshot(ctx, tx, snapshot, detailUpdateQueries)
		}
		return fmt.Errorf("unsupported snapshot kind %q", snapshot.kind)
	}
}

func restoreFullSnapshot(ctx context.Context, tx pgx.Tx, snapshot databaseSnapshot, table, deleteSQL string, deleteArgs []any) error {
	if _, err := tx.Exec(ctx, deleteSQL, deleteArgs...); err != nil {
		return err
	}
	if !snapshot.existed {
		return nil
	}
	query := fmt.Sprintf(`INSERT INTO %s SELECT (jsonb_populate_record(NULL::%s,$1::jsonb)).*`, table, table)
	if _, err := tx.Exec(ctx, query, string(snapshot.beforeImage)); err != nil {
		return fmt.Errorf("restore saved %s row: %w", table, err)
	}
	return nil
}

func legacyDetailRestoreUpdateQuery(ctx context.Context, tx pgx.Tx, table string) (string, error) {
	rows, err := tx.Query(ctx, `
		SELECT attname
		FROM pg_catalog.pg_attribute
		WHERE attrelid=pg_catalog.to_regclass($1) AND attnum>0 AND NOT attisdropped AND attgenerated=''
		ORDER BY attnum
	`, table)
	if err != nil {
		return "", fmt.Errorf("read %s columns: %w", table, err)
	}
	defer rows.Close()
	var columns, savedColumns []string
	hasID, hasWorkspaceID := false, false
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return "", fmt.Errorf("read %s column: %w", table, err)
		}
		column := pgx.Identifier{name}.Sanitize()
		columns = append(columns, column)
		savedColumns = append(savedColumns, "before_image."+column)
		hasID = hasID || name == "id"
		hasWorkspaceID = hasWorkspaceID || name == "workspace_id"
	}
	if err := rows.Err(); err != nil {
		return "", fmt.Errorf("iterate %s columns: %w", table, err)
	}
	if len(columns) == 0 || !hasID || !hasWorkspaceID {
		return "", fmt.Errorf("%s has no restorable workspace-scoped row shape", table)
	}
	quotedTable := pgx.Identifier{table}.Sanitize()
	return fmt.Sprintf(`UPDATE %s AS target SET (%s)=(SELECT %s
		FROM jsonb_populate_record(NULL::%s,$3::jsonb) AS before_image)
		WHERE target.workspace_id=$1 AND target.id=$2`,
		quotedTable, strings.Join(columns, ","), strings.Join(savedColumns, ","), quotedTable), nil
}

func restoreLegacyDetailSnapshot(ctx context.Context, tx pgx.Tx, snapshot databaseSnapshot, detailUpdateQueries map[string]string) error {
	if !snapshot.existed {
		return errors.New("GitLab legacy detail before-image must preserve an existing row")
	}
	table := strings.TrimPrefix(snapshot.kind, "legacy_detail:")
	var query string
	switch table {
	case "gitlab_mr_approval_state":
		query = `UPDATE gitlab_mr_approval_state
			SET merge_request_id=($3::jsonb->>'merge_request_id')::uuid,
			    vcs_pull_request_id=($3::jsonb->>'vcs_pull_request_id')::uuid
			WHERE workspace_id=$1 AND (merge_request_id::text=$2 OR vcs_pull_request_id::text=$2)`
	case "gitlab_mr_discussion", "gitlab_mr_note", "gitlab_pipeline_job":
		query, ok := detailUpdateQueries[table]
		if !ok {
			return fmt.Errorf("missing restore update query for %s", table)
		}
		// Do not delete discussions here: their cascade could remove notes outside
		// this workspace-scoped snapshot.
		tag, err := tx.Exec(ctx, query, snapshot.workspaceID, snapshot.recordKey, string(snapshot.beforeImage))
		if err != nil {
			return fmt.Errorf("restore existing %s row in place: %w", table, err)
		}
		if tag.RowsAffected() == 1 {
			return nil
		}
		if tag.RowsAffected() != 0 {
			return fmt.Errorf("expected to update at most one %s row, updated %d", table, tag.RowsAffected())
		}
		return restoreFullSnapshot(ctx, tx, snapshot, table,
			fmt.Sprintf(`DELETE FROM %s WHERE workspace_id=$1 AND id=$2`, table),
			[]any{snapshot.workspaceID, snapshot.recordKey})
	default:
		return fmt.Errorf("unsupported legacy detail table %q", table)
	}
	tag, err := tx.Exec(ctx, query, snapshot.workspaceID, snapshot.recordKey, string(snapshot.beforeImage))
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return fmt.Errorf("expected one legacy detail row, restored %d", tag.RowsAffected())
	}
	return nil
}
