package gitlabmigration

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/multica-ai/multica/server/internal/util/secretbox"
)


const migrationAdvisoryLock int64 = 746031

type Runner struct {
	pool                  *pgxpool.Pool
	env                   Environment
	systemHookAttestation *systemHookInventoryAttestationInput
}

type MigrationState struct {
	Phase                           string                            `json:"phase"`
	MaintenanceMode                 bool                              `json:"maintenance_mode"`
	Generation                      int64                             `json:"generation"`
	BeganAt                         time.Time                         `json:"began_at,omitempty"`
	UpdatedAt                       time.Time                         `json:"updated_at"`
	LastError                       string                            `json:"last_error,omitempty"`
	RecoveryDeliveryIDs             []string                          `json:"recovery_delivery_ids,omitempty"`
	SystemHookInventoryAttestations []SystemHookInventoryAttestation `json:"system_hook_inventory_attestations,omitempty"`
}

type Finding struct {
	Code        string `json:"code"`
	Severity    string `json:"severity"`
	WorkspaceID string `json:"workspace_id,omitempty"`
	Resource    string `json:"resource,omitempty"`
	Message     string `json:"message"`
}

type Report struct {
	NoOp                            bool                              `json:"no_op"`
	State                           string                            `json:"state"`
	LegacyConnections               int64                             `json:"legacy_connections"`
	LegacyProjects                  int64                             `json:"legacy_projects"`
	LegacyMergeRequests             int64                             `json:"legacy_merge_requests"`
	LegacyPipelines                 int64                             `json:"legacy_pipelines"`
	IssueLinks                      int64                             `json:"issue_links"`
	ActiveIssueWakeups              int64                             `json:"active_issue_wakeups"`
	LegacyChatWakeups               int64                             `json:"legacy_chat_wakeups"`
	ActiveChatWakeups               int64                             `json:"active_chat_wakeups"`
	UnifiedConnections              int64                             `json:"unified_gitlab_connections"`
	UnifiedMergeRequests            int64                             `json:"unified_gitlab_merge_requests"`
	ManagedHooks                    int64                             `json:"managed_hooks"`
	ManualHooks                     int64                             `json:"manual_hooks"`
	SystemHookInventoryAttestations []SystemHookInventoryAttestation `json:"system_hook_inventory_attestations,omitempty"`
	Findings                        []Finding                         `json:"findings"`
}

func NewRunner(pool *pgxpool.Pool, env Environment) *Runner {
	return &Runner{pool: pool, env: env}
}

func (r *Runner) beginMigrationLock(ctx context.Context, operation string) (pgx.Tx, error) {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("begin GitLab %s lock: %w", operation, err)
	}
	if _, err := tx.Exec(ctx, "SELECT pg_advisory_xact_lock($1)", migrationAdvisoryLock); err != nil {
		_ = tx.Rollback(context.Background())
		return nil, fmt.Errorf("lock GitLab %s: %w", operation, err)
	}
	return tx, nil
}

func releaseMigrationLock(tx pgx.Tx) {
	if tx != nil {
		_ = tx.Rollback(context.Background())
	}
}


func (r *Runner) State(ctx context.Context) (MigrationState, error) {
	var state MigrationState
	var beganAt pgtype.Timestamptz
	if err := r.pool.QueryRow(ctx, `
		SELECT phase, maintenance_mode, generation, began_at, updated_at, COALESCE(last_error, '')
		FROM vcs_gitlab_migration_state WHERE id=1
	`).Scan(&state.Phase, &state.MaintenanceMode, &state.Generation, &beganAt, &state.UpdatedAt, &state.LastError); err != nil {
		return MigrationState{}, fmt.Errorf("read GitLab migration state (run cmd/migrate up first): %w", err)
	}
	if beganAt.Valid {
		state.BeganAt = beganAt.Time
	}
	rows, err := r.pool.Query(ctx, `
		SELECT DISTINCT delivery_id
		FROM vcs_gitlab_migration_delivery
		WHERE payload IS NOT NULL
		ORDER BY delivery_id
	`)
	if err != nil {
		return MigrationState{}, fmt.Errorf("list retained GitLab delivery IDs: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var deliveryID string
		if err := rows.Scan(&deliveryID); err != nil {
			return MigrationState{}, fmt.Errorf("read retained GitLab delivery ID: %w", err)
		}
		state.RecoveryDeliveryIDs = append(state.RecoveryDeliveryIDs, deliveryID)
	}
	if err := rows.Err(); err != nil {
		return MigrationState{}, fmt.Errorf("iterate retained GitLab delivery IDs: %w", err)
	}
	rows.Close()
	attestations, err := r.loadSystemHookInventoryAttestations(ctx)
	if err != nil {
		return MigrationState{}, err
	}
	state.SystemHookInventoryAttestations = attestations
	return state, nil
}

func (r *Runner) preflightState(ctx context.Context) (MigrationState, bool, error) {
	cutoverRelations := []string{
		"vcs_gitlab_project",
		"vcs_gitlab_migration_id_map",
		"vcs_gitlab_migration_state",
		"vcs_gitlab_migration_snapshot",
		"vcs_gitlab_migration_hook",
		"vcs_gitlab_migration_delivery",
		"vcs_gitlab_mr_detail",
	}
	present, missing, err := r.inspectRelations(ctx, cutoverRelations)
	if err != nil {
		return MigrationState{}, false, fmt.Errorf("inspect GitLab cutover schema: %w", err)
	}
	stateExists := false
	for _, relation := range present {
		if relation == "vcs_gitlab_migration_state" {
			stateExists = true
			break
		}
	}
	presentColumns, missingColumns, err := r.inspectCutoverColumns(ctx)
	if err != nil {
		return MigrationState{}, false, fmt.Errorf("inspect GitLab cutover columns: %w", err)
	}
	if !stateExists {
		markers := []string{
			"gitlab_mr_approval_vcs_pr_uidx",
			"gitlab_mr_discussion_vcs_pr_idx",
			"gitlab_mr_note_vcs_pr_idx",
			"gitlab_pipeline_job_vcs_pr_idx",
			"gitlab_pipeline_job_vcs_pr_pipeline_idx",
			"gitlab_mr_discussion_vcs_pr_key_uidx",
			"gitlab_mr_note_vcs_pr_key_uidx",
			"gitlab_pipeline_job_vcs_pr_key_uidx",
			"gitlab_mr_approval_legacy_id_uidx",
		}
		presentMarkers, _, err := r.inspectRelations(ctx, markers)
		if err != nil {
			return MigrationState{}, false, fmt.Errorf("inspect GitLab cutover migration artifacts: %w", err)
		}
		nullableLegacyIDs, err := r.countNullableLegacyDetailIDs(ctx)
		if err != nil {
			return MigrationState{}, false, fmt.Errorf("inspect legacy detail nullability: %w", err)
		}
		artifacts := append(append(append([]string{}, present...), presentColumns...), presentMarkers...)
		if nullableLegacyIDs != 0 {
			artifacts = append(artifacts, fmt.Sprintf("nullable legacy detail ID columns=%d", nullableLegacyIDs))
		}
		if len(artifacts) != 0 {
			return MigrationState{}, true, fmt.Errorf(
				"cutover schema is partial: vcs_gitlab_migration_state is absent while cutover artifacts exist (%s)",
				strings.Join(artifacts, ", "),
			)
		}
		return MigrationState{Phase: "not_initialized"}, false, nil
	}
	if len(missing) != 0 {
		return MigrationState{}, true, fmt.Errorf("cutover schema is partial; missing relations: %s", strings.Join(missing, ", "))
	}
	if len(missingColumns) != 0 {
		return MigrationState{}, true, fmt.Errorf("cutover schema is partial; missing columns: %s", strings.Join(missingColumns, ", "))
	}
	state, err := r.State(ctx)
	if err != nil {
		return MigrationState{}, true, err
	}
	return state, false, nil
}

func (r *Runner) inspectRelations(ctx context.Context, names []string) (present, missing []string, err error) {
	rows, err := r.pool.Query(ctx, `
		SELECT relation_name,
		       to_regclass(format('%I.%I', current_schema(), relation_name)) IS NOT NULL
		FROM unnest($1::text[]) AS required(relation_name)
		ORDER BY relation_name
	`, names)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var name string
		var exists bool
		if err := rows.Scan(&name, &exists); err != nil {
			return nil, nil, err
		}
		if exists {
			present = append(present, name)
		} else {
			missing = append(missing, name)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, nil, err
	}
	return present, missing, nil
}

func (r *Runner) inspectCutoverColumns(ctx context.Context) (present, missing []string, err error) {
	rows, err := r.pool.Query(ctx, `
		WITH required(table_name, column_name) AS (
			VALUES
				('vcs_gitlab_project','legacy_link_policy'),
				('vcs_gitlab_mr_detail','vcs_pull_request_id'),
				('gitlab_mr_approval_state','vcs_pull_request_id'),
				('gitlab_mr_discussion','vcs_pull_request_id'),
				('gitlab_mr_note','vcs_pull_request_id'),
				('gitlab_pipeline_job','vcs_pull_request_id'),
				('issue_vcs_pull_request','gitlab_close_intent'),
				('issue_vcs_pull_request','gitlab_linked_by_type'),
				('issue_vcs_pull_request','gitlab_linked_by_id'),
				('issue_vcs_pull_request','gitlab_linked_at'),
				('issue_vcs_pull_request','vcs_linked')
		)
		SELECT required.table_name || '.' || required.column_name,
		       column_info.column_name IS NOT NULL
		FROM required
		LEFT JOIN information_schema.columns column_info
		  ON column_info.table_schema=current_schema()
		 AND column_info.table_name=required.table_name
		 AND column_info.column_name=required.column_name
		ORDER BY required.table_name, required.column_name
	`)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var name string
		var exists bool
		if err := rows.Scan(&name, &exists); err != nil {
			return nil, nil, err
		}
		if exists {
			present = append(present, name)
		} else {
			missing = append(missing, name)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, nil, err
	}
	return present, missing, nil
}

func (r *Runner) countNullableLegacyDetailIDs(ctx context.Context) (int64, error) {
	var count int64
	err := r.pool.QueryRow(ctx, `
		SELECT count(*)
		FROM pg_attribute attribute
		JOIN pg_class relation ON relation.oid=attribute.attrelid
		JOIN pg_namespace namespace ON namespace.oid=relation.relnamespace
		WHERE namespace.nspname=current_schema()
		  AND relation.relname IN ('gitlab_mr_approval_state','gitlab_mr_discussion','gitlab_mr_note','gitlab_pipeline_job')
		  AND attribute.attname='merge_request_id'
		  AND attribute.attnum>0
		  AND NOT attribute.attisdropped
		  AND NOT attribute.attnotnull
	`).Scan(&count)
	return count, err
}

func (r *Runner) legacyGitLabSchemaApplied(ctx context.Context) (bool, error) {
	present, _, err := r.inspectRelations(ctx, []string{"schema_migrations"})
	if err != nil {
		return false, fmt.Errorf("inspect schema migration history: %w", err)
	}
	if len(present) == 0 {
		return false, nil
	}
	rows, err := r.pool.Query(ctx, `SELECT version FROM schema_migrations`)
	if err != nil {
		return false, fmt.Errorf("read schema migration history: %w", err)
	}
	defer rows.Close()
	applied := false
	for rows.Next() {
		var version string
		if err := rows.Scan(&version); err != nil {
			return false, fmt.Errorf("read schema migration version: %w", err)
		}
		prefix, _, hasName := strings.Cut(version, "_")
		migrationNumber, parseErr := strconv.Atoi(prefix)
		if !hasName || parseErr != nil || migrationNumber < 0 {
			return false, fmt.Errorf("schema_migrations contains an invalid version %q", version)
		}
		if migrationNumber >= 131 {
			applied = true
		}
	}
	if err := rows.Err(); err != nil {
		return false, fmt.Errorf("iterate schema migration history: %w", err)
	}
	return applied, nil
}

func (r *Runner) Preflight(ctx context.Context) (Report, error) {
	report := Report{}
	state, partialSchema, err := r.preflightState(ctx)
	if err != nil {
		code := "migration_state_unavailable"
		if partialSchema {
			code = "cutover_schema_partial"
		}
		report.Findings = append(report.Findings, Finding{
			Code: code, Severity: "error",
			Message: "GitLab migration state/schema is incomplete or cannot be read; inspect the command error",
		})
		return report, err
	}
	report.State = state.Phase
	legacyTables := [5]bool{}
	if err := r.pool.QueryRow(ctx, `
		SELECT
			to_regclass(format('%I.%I',current_schema(),'gitlab_connection')) IS NOT NULL,
			to_regclass(format('%I.%I',current_schema(),'gitlab_project_binding')) IS NOT NULL,
			to_regclass(format('%I.%I',current_schema(),'gitlab_merge_request')) IS NOT NULL,
			to_regclass(format('%I.%I',current_schema(),'issue_gitlab_merge_request')) IS NOT NULL,
			to_regclass(format('%I.%I',current_schema(),'gitlab_mr_pipeline')) IS NOT NULL
	`).Scan(&legacyTables[0], &legacyTables[1], &legacyTables[2], &legacyTables[3], &legacyTables[4]); err != nil {
		report.Findings = append(report.Findings, Finding{
			Code: "legacy_schema_unreadable", Severity: "error",
			Message: "required legacy GitLab relations could not be inspected",
		})
		return report, fmt.Errorf("inspect legacy GitLab schema: %w", err)
	}
	presentLegacyTables := 0
	for _, present := range legacyTables {
		if present {
			presentLegacyTables++
		}
	}
	if presentLegacyTables == 0 {
		if state.Phase == "complete" {
			report.NoOp = true
			return report, nil
		}
		if state.Phase == "not_initialized" {
			legacySchemaApplied, err := r.legacyGitLabSchemaApplied(ctx)
			if err != nil {
				report.Findings = append(report.Findings, Finding{
					Code: "schema_migration_history_unavailable", Severity: "error",
					Message: "the existing schema migration history could not be safely inspected",
				})
				return report, err
			}
			if legacySchemaApplied {
				report.Findings = append(report.Findings, Finding{
					Code: "legacy_schema_absent", Severity: "error",
					Message: "legacy GitLab relations are absent although the migration history says the legacy GitLab schema was applied",
				})
				return report, errors.New("legacy GitLab schema is absent despite migration history")
			}
			report.NoOp = true
			return report, nil
		}
	}
	if presentLegacyTables != len(legacyTables) {
		report.Findings = append(report.Findings, Finding{Code: "legacy_schema_partial", Severity: "error", Message: "legacy GitLab schema is partially present; expected all legacy tables or none after finalization"})
		return report, errors.New("legacy GitLab schema is partially present")
	}
	_, missingDependencies, err := r.inspectRelations(ctx, []string{
		"workspace",
		"issue",
		"gitlab_mr_approval_state",
		"gitlab_mr_discussion",
		"gitlab_mr_note",
		"gitlab_pipeline_job",
		"issue_wakeup",
		"chat_wakeup",
		"issue_vcs_pull_request",
		"vcs_connection",
		"vcs_pull_request",
	})
	if err != nil {
		report.Findings = append(report.Findings, Finding{
			Code: "legacy_schema_unreadable", Severity: "error",
			Message: "required legacy GitLab migration dependencies could not be inspected",
		})
		return report, fmt.Errorf("inspect legacy GitLab migration dependencies: %w", err)
	}
	if len(missingDependencies) != 0 {
		dependencies := strings.Join(missingDependencies, ", ")
		report.Findings = append(report.Findings, Finding{
			Code: "legacy_schema_dependency_missing", Severity: "error",
			Resource: dependencies, Message: "required legacy and shared VCS relations are missing",
		})
		return report, fmt.Errorf("required legacy GitLab migration dependencies are missing: %s", dependencies)
	}
	if err := r.pool.QueryRow(ctx, `
		SELECT
			(SELECT count(*) FROM gitlab_connection),
			(SELECT count(*) FROM gitlab_project_binding),
			(SELECT count(*) FROM gitlab_merge_request),
			(SELECT count(*) FROM gitlab_mr_pipeline),
			(SELECT count(*) FROM issue_gitlab_merge_request),
			(SELECT count(*) FROM issue_wakeup w
			 WHERE w.enabled AND w.condition IS NOT NULL AND w.condition->>'type'='pull_request'
			   AND EXISTS (SELECT 1 FROM issue_gitlab_merge_request l WHERE l.issue_id=w.issue_id)),
			(SELECT count(*) FROM chat_wakeup
			 WHERE condition->>'provider'='gitlab' AND condition->'gitlab'->>'source'='gitlab_connection'),
			(SELECT count(*) FROM chat_wakeup WHERE status='active'
			 AND condition->>'provider'='gitlab' AND condition->'gitlab'->>'source'='gitlab_connection'),
			(SELECT count(*) FROM vcs_connection WHERE provider='gitlab'),
			(SELECT count(*) FROM vcs_pull_request WHERE provider='gitlab')
	`).Scan(&report.LegacyConnections, &report.LegacyProjects, &report.LegacyMergeRequests, &report.LegacyPipelines, &report.IssueLinks,
		&report.ActiveIssueWakeups, &report.LegacyChatWakeups, &report.ActiveChatWakeups, &report.UnifiedConnections, &report.UnifiedMergeRequests); err != nil {
		report.Findings = append(report.Findings, Finding{
			Code: "legacy_schema_unreadable", Severity: "error",
			Message: "legacy GitLab migration inputs could not be counted; inspect the command error",
		})
		return report, fmt.Errorf("count GitLab migration inputs: %w", err)
	}
	report.NoOp = report.LegacyConnections == 0 && report.LegacyProjects == 0 && report.LegacyMergeRequests == 0 && report.LegacyPipelines == 0 && report.IssueLinks == 0 && report.LegacyChatWakeups == 0
	if report.NoOp {
		return report, nil
	}
	if state.Phase == "complete" {
		report.Findings = append(report.Findings, Finding{Code: "legacy_data_after_complete", Severity: "error", Message: "legacy GitLab data exists although the cutover was marked complete"})
		return report, errors.New("legacy GitLab data exists after the cutover was marked complete")
	}
	if !r.env.VCSIntegrationEnabled {
		report.Findings = append(report.Findings, Finding{Code: "vcs_integration_disabled", Severity: "error", Message: "set MULTICA_VCS_INTEGRATION_ENABLED=true before migrating existing GitLab data"})
		return report, errors.New("MULTICA_VCS_INTEGRATION_ENABLED=true is required to cut over existing GitLab data")
	}
	publicURL, err := url.Parse(strings.TrimSpace(r.env.PublicURL))
	if err != nil || (publicURL.Scheme != "https" && publicURL.Scheme != "http") || publicURL.Host == "" || publicURL.User != nil || publicURL.RawQuery != "" || publicURL.Fragment != "" {
		report.Findings = append(report.Findings, Finding{Code: "public_url_missing", Severity: "error", Message: "MULTICA_PUBLIC_URL must be an absolute HTTP(S) API origin before the GitLab maintenance cutover"})
		return report, errors.New("MULTICA_PUBLIC_URL is required for GitLab Hook cutover")
	}
	box, err := r.env.box()
	if err != nil {
		report.Findings = append(report.Findings, Finding{Code: "vcs_encryption_key", Severity: "error", Message: err.Error()})
		return report, err
	}
	rows, err := r.pool.Query(ctx, `SELECT id::text, workspace_id::text, base_url, access_token_encrypted, webhook_secret_encrypted FROM gitlab_connection ORDER BY workspace_id, id`)
	if err != nil {
		return report, fmt.Errorf("list legacy GitLab connections: %w", err)
	}
	defer rows.Close()
	apis := make(map[string]*gitlabAPI)
	var blockers []error
	for rows.Next() {
		var id, workspaceID, baseURL, encryptedToken, encryptedSecret string
		if err := rows.Scan(&id, &workspaceID, &baseURL, &encryptedToken, &encryptedSecret); err != nil {
			return report, fmt.Errorf("read legacy GitLab connection: %w", err)
		}
		token, _, _, err := r.env.legacyCredentials(box, baseURL, encryptedToken, encryptedSecret)
		if err != nil {
			blockers = append(blockers, err)
			report.Findings = append(report.Findings, Finding{Code: "legacy_credentials_missing", Severity: "error", WorkspaceID: workspaceID, Resource: safeInstanceURL(baseURL), Message: "the encrypted legacy credentials could not be opened and no exact-instance environment fallback is available"})
			continue
		}
		key := workspaceID + "|" + normalizeInstanceURL(baseURL)
		api := apis[key]
		if api == nil {
			api, err = newGitLabAPI(baseURL, token, r.env.GitLabProxyURL)
			if err != nil {
				blockers = append(blockers, err)
				continue
			}
			if _, err := api.currentUser(ctx); err != nil {
				blockers = append(blockers, fmt.Errorf("validate credentials for %s: %w", safeInstanceURL(baseURL), err))
				report.Findings = append(report.Findings, Finding{Code: "instance_unreachable", Severity: "error", WorkspaceID: workspaceID, Resource: safeInstanceURL(baseURL), Message: "GitLab credentials or instance connectivity could not be verified"})
				continue
			}
			apis[key] = api
		}
		if err := r.checkExistingConnectionCredentials(ctx, box, workspaceID, baseURL, token); err != nil {
			blockers = append(blockers, err)
			report.Findings = append(report.Findings, Finding{Code: "connection_conflict", Severity: "error", WorkspaceID: workspaceID, Resource: safeInstanceURL(baseURL), Message: "an existing workspace connection has incompatible provider or access token"})
		}
	}
	if err := rows.Err(); err != nil {
		return report, fmt.Errorf("iterate legacy GitLab connections: %w", err)
	}
	if len(blockers) == 0 {
		if err := r.checkProjects(ctx, apis, report); err != nil {
			blockers = append(blockers, err)
		}
		if len(blockers) == 0 {
			if err := r.checkHookInventory(ctx, state, apis, &report); err != nil {
				blockers = append(blockers, err)
			}
		}
	}
	return report, errors.Join(blockers...)
}

func (r *Runner) checkExistingConnectionCredentials(ctx context.Context, box *secretbox.Box, workspaceID, baseURL, token string) error {
	rows, err := r.pool.Query(ctx, `
		SELECT provider, instance_url, access_token_encrypted, webhook_secret_encrypted
		FROM vcs_connection WHERE workspace_id=$1
	`, workspaceID)
	if err != nil {
		return fmt.Errorf("check existing workspace connection: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var provider, instanceURL, encryptedToken, encryptedSecret string
		if err := rows.Scan(&provider, &instanceURL, &encryptedToken, &encryptedSecret); err != nil {
			return err
		}
		if normalizeInstanceURL(instanceURL) != normalizeInstanceURL(baseURL) {
			continue
		}
		if provider != "gitlab" {
			return errors.New("an existing workspace connection uses a different provider")
		}
		storedToken, err := openSecret(box, encryptedToken)
		if err != nil {
			return err
		}
		if _, err := openSecret(box, encryptedSecret); err != nil {
			return err
		}
		if storedToken != token {
			return errors.New("an existing workspace GitLab connection has a different access token")
		}
	}
	return rows.Err()
}

func (r *Runner) checkProjects(ctx context.Context, apis map[string]*gitlabAPI, report Report) error {
	rows, err := r.pool.Query(ctx, `
		SELECT pb.workspace_id::text, gc.base_url, pb.gitlab_project_id, pb.path_with_namespace, pb.hook_id
		FROM gitlab_project_binding pb
		JOIN gitlab_connection gc ON gc.id=pb.connection_id AND gc.workspace_id=pb.workspace_id
		ORDER BY pb.workspace_id, pb.gitlab_project_id
	`)
	if err != nil {
		return fmt.Errorf("list GitLab project bindings: %w", err)
	}
	defer rows.Close()
	var blockers []error
	for rows.Next() {
		var workspaceID, baseURL, path string
		var projectID int64
		var hookID pgtype.Int8
		if err := rows.Scan(&workspaceID, &baseURL, &projectID, &path, &hookID); err != nil {
			return err
		}
		api := apis[workspaceID+"|"+normalizeInstanceURL(baseURL)]
		if api == nil {
			continue
		}
		project, err := api.getProject(ctx, projectID)
		if err != nil {
			blockers = append(blockers, fmt.Errorf("verify project %d in workspace %s: %w", projectID, workspaceID, err))
			report.Findings = append(report.Findings, Finding{Code: "project_unreadable", Severity: "error", WorkspaceID: workspaceID, Resource: path, Message: "GitLab project permissions or connectivity could not be verified"})
			continue
		}
		if remotePath, _ := project["path_with_namespace"].(string); remotePath != path {
			blockers = append(blockers, fmt.Errorf("project %d path changed from %q to %q; resolve identity before migration", projectID, path, remotePath))
			report.Findings = append(report.Findings, Finding{Code: "project_path_conflict", Severity: "error", WorkspaceID: workspaceID, Resource: path, Message: "the GitLab project path differs from the stored binding; resolve the identity before migration"})
		}
		if hookID.Valid && hookID.Int64 > 0 {
			if _, err := api.getHook(ctx, "project", projectID, hookID.Int64); err != nil {
				blockers = append(blockers, fmt.Errorf("verify managed Hook for project %d: %w", projectID, err))
				report.Findings = append(report.Findings, Finding{Code: "managed_hook_unreadable", Severity: "error", WorkspaceID: workspaceID, Resource: path, Message: "the managed project Hook could not be read"})
			}
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	return errors.Join(blockers...)
}

func (r *Runner) checkHookInventory(ctx context.Context, state MigrationState, apis map[string]*gitlabAPI, report *Report) error {
	type legacyConnection struct {
		ID        string
		Workspace string
		BaseURL   string
	}
	type legacyProject struct {
		Workspace string
		ID        int64
		Path      string
		HookID    pgtype.Int8
	}
	connectionRows, err := r.pool.Query(ctx, `
		SELECT id::text,workspace_id::text,base_url FROM gitlab_connection ORDER BY workspace_id,id
	`)
	if err != nil {
		return fmt.Errorf("list GitLab connections for Hook preflight: %w", err)
	}
	var connections []legacyConnection
	for connectionRows.Next() {
		var item legacyConnection
		if err := connectionRows.Scan(&item.ID, &item.Workspace, &item.BaseURL); err != nil {
			connectionRows.Close()
			return err
		}
		connections = append(connections, item)
	}
	if err := connectionRows.Err(); err != nil {
		connectionRows.Close()
		return err
	}
	connectionRows.Close()
	projectRows, err := r.pool.Query(ctx, `
		SELECT connection_id::text,workspace_id::text,gitlab_project_id,path_with_namespace,hook_id
		FROM gitlab_project_binding ORDER BY workspace_id,gitlab_project_id
	`)
	if err != nil {
		return fmt.Errorf("list GitLab projects for Hook preflight: %w", err)
	}
	projects := make(map[string][]legacyProject)
	for projectRows.Next() {
		var connectionID string
		var project legacyProject
		if err := projectRows.Scan(&connectionID, &project.Workspace, &project.ID, &project.Path, &project.HookID); err != nil {
			projectRows.Close()
			return err
		}
		projects[connectionID] = append(projects[connectionID], project)
	}
	if err := projectRows.Err(); err != nil {
		projectRows.Close()
		return err
	}
	projectRows.Close()
	instanceConnections := make(map[string]int)
	for _, connection := range connections {
		instanceConnections[normalizeInstanceURL(connection.BaseURL)]++
	}
	var blockers []error
	seenHooks := make(map[string]struct{})
	for _, connection := range connections {
		api := apis[connection.Workspace+"|"+normalizeInstanceURL(connection.BaseURL)]
		if api == nil {
			continue
		}
		observe := func(scopeType, scopeID, scopePath string, remote map[string]any, managedID pgtype.Int8) {
			id, err := hookID(remote)
			if err != nil {
				blockers = append(blockers, err)
				return
			}
			key := connection.ID + "|" + scopeType + "|" + scopeID + "|" + strconv.FormatInt(id, 10)
			if _, ok := seenHooks[key]; ok {
				return
			}
			seenHooks[key] = struct{}{}
			rawURL := hookURL(remote)
			legacyID, legacyRoute := legacyHookConnection(rawURL)
			if scopeType == "project" && managedID.Valid && managedID.Int64 == id {
				report.ManagedHooks++
				if !legacyRoute || (legacyID != "" && legacyID != connection.ID) {
					blockers = append(blockers, fmt.Errorf("managed GitLab Hook %d for %s has no reversible legacy route", id, scopePath))
					report.Findings = append(report.Findings, Finding{Code: "managed_hook_not_reversible", Severity: "error", WorkspaceID: connection.Workspace, Resource: scopePath, Message: "the managed Hook does not use a recoverable legacy route; restore its legacy URL before beginning cutover"})
				} else if legacyID == "" && instanceConnections[normalizeInstanceURL(connection.BaseURL)] > 1 {
					blockers = append(blockers, fmt.Errorf("managed GitLab Hook %d for %s is ambiguous across workspace connections", id, scopePath))
					report.Findings = append(report.Findings, Finding{Code: "shared_legacy_hook_ambiguous", Severity: "error", WorkspaceID: connection.Workspace, Resource: scopePath, Message: "the legacy Hook is shared across workspace connections without an unambiguous connection identity"})
				}
				return
			}
			if !legacyRoute || (legacyID != "" && legacyID != connection.ID) {
				return
			}
			if legacyID == "" && instanceConnections[normalizeInstanceURL(connection.BaseURL)] > 1 {
				blockers = append(blockers, fmt.Errorf("legacy GitLab Hook %d in %s scope %s is ambiguous across workspace connections", id, scopeType, scopePath))
				report.Findings = append(report.Findings, Finding{Code: "shared_legacy_hook_ambiguous", Severity: "error", WorkspaceID: connection.Workspace, Resource: scopePath, Message: "the legacy Hook is shared across workspace connections without an unambiguous connection identity"})
				return
			}
			report.ManualHooks++
			resource := scopePath
			if resource == "" {
				resource = scopeType + " Hook " + scopeID
			}
			report.Findings = append(report.Findings, Finding{Code: "manual_hook_review", Severity: "warning", WorkspaceID: connection.Workspace, Resource: resource, Message: "switch this manual Hook to its workspace-specific VCS route and verify real supported-event deliveries before finalization"})
		}
		for _, project := range projects[connection.ID] {
			hooks, err := api.listHooks(ctx, "project", project.ID)
			if err != nil {
				blockers = append(blockers, fmt.Errorf("list project Hooks for %s: %w", project.Path, err))
				report.Findings = append(report.Findings, Finding{Code: "project_hook_inventory_failed", Severity: "error", WorkspaceID: connection.Workspace, Resource: project.Path, Message: "project Hooks could not be inventoried; verify GitLab read permissions"})
				continue
			}
			for _, remote := range hooks {
				observe("project", strconv.FormatInt(project.ID, 10), project.Path, remote, project.HookID)
			}
		}
		if api.groupHooksUnsupportedByMetadata(ctx) {
			report.Findings = append(report.Findings, Finding{Code: "group_hooks_unsupported", Severity: "warning", WorkspaceID: connection.Workspace, Resource: safeInstanceURL(connection.BaseURL), Message: "the confirmed GitLab plan does not support group webhooks; project and System Hook inventory will continue"})
		} else {
			groups, err := api.listGroups(ctx)
			if err != nil {
				blockers = append(blockers, fmt.Errorf("list groups for Hook preflight: %w", err))
				report.Findings = append(report.Findings, Finding{Code: "group_hook_inventory_failed", Severity: "error", WorkspaceID: connection.Workspace, Resource: safeInstanceURL(connection.BaseURL), Message: "group Hooks could not be inventoried; verify GitLab read permissions"})
			} else {
				groupsByPath := make(map[string]map[string]any, len(groups))
				for _, group := range groups {
					groupPath, _ := group["full_path"].(string)
					if groupPath != "" {
						groupsByPath[groupPath] = group
					}
				}
				seenGroups := make(map[int64]struct{})
				missingGroups := make(map[string]struct{})
				for _, project := range projects[connection.ID] {
					for _, groupPath := range projectGroupPaths(project.Path) {
						group, ok := groupsByPath[groupPath]
						if !ok {
							if _, seen := missingGroups[groupPath]; !seen {
								missingGroups[groupPath] = struct{}{}
								blockers = append(blockers, fmt.Errorf("cannot verify GitLab group Hook inventory: ancestor group %q is not visible to this connection", groupPath))
								report.Findings = append(report.Findings, Finding{Code: "group_hook_inventory_failed", Severity: "error", WorkspaceID: connection.Workspace, Resource: groupPath, Message: "the ancestor group is not visible to this connection; existing group Hooks cannot be safely ruled out"})
							}
							continue
						}
						groupID, idErr := hookID(group)
						if idErr != nil {
							blockers = append(blockers, fmt.Errorf("read GitLab group identity for %s: %w", groupPath, idErr))
							continue
						}
						if _, seen := seenGroups[groupID]; seen {
							continue
						}
						seenGroups[groupID] = struct{}{}
						groupHooks, listErr := api.listHooks(ctx, "group", groupID)
						if listErr != nil {
							unsupported, accessErr := api.groupHooksUnsupportedForGroup(ctx, groupID, listErr)
							if accessErr != nil {
								blockers = append(blockers, fmt.Errorf("inspect GitLab group Hooks for %s: %w", groupPath, accessErr))
								report.Findings = append(report.Findings, Finding{Code: "group_hook_inventory_failed", Severity: "error", WorkspaceID: connection.Workspace, Resource: groupPath, Message: "group Hook inventory returned an ambiguous result and Owner/admin access could not prove the feature unsupported"})
							} else if unsupported {
								report.Findings = append(report.Findings, Finding{Code: "group_hooks_unsupported", Severity: "warning", WorkspaceID: connection.Workspace, Resource: groupPath, Message: "the confirmed group Owner/admin received HTTP 404 from the group Hook API; group hooks are unavailable for this scope"})
							} else {
								blockers = append(blockers, fmt.Errorf("list group Hooks for %s: %w", groupPath, listErr))
								report.Findings = append(report.Findings, Finding{Code: "group_hook_inventory_failed", Severity: "error", WorkspaceID: connection.Workspace, Resource: groupPath, Message: "group Hooks could not be inventoried; verify GitLab read permissions"})
							}
							continue
						}
						for _, remote := range groupHooks {
							observe("group", strconv.FormatInt(groupID, 10), groupPath, remote, pgtype.Int8{})
						}
					}
				}
			}
		}
		systemHooks, err := api.listHooks(ctx, "system", 0)
		if err != nil {
			if gitLabHTTPStatus(err) == 403 {
				attestation, attestationErr := r.preflightSystemHookAttestation(ctx, state, connection.BaseURL)
				if attestationErr != nil {
					blockers = append(blockers, fmt.Errorf("read System Hook inventory attestation: %w", attestationErr))
					report.Findings = append(report.Findings, Finding{Code: "system_hook_inventory_failed", Severity: "error", WorkspaceID: connection.Workspace, Resource: safeInstanceURL(connection.BaseURL), Message: "System Hooks could not be inventoried; an instance administrator must verify read access"})
					continue
				}
				if shouldSkipSystemHookInventory(connection.BaseURL, err, attestation) {
					addSystemHookInventoryAttestation(report, *attestation)
					report.Findings = append(report.Findings, Finding{
						Code: "system_hook_inventory_attested", Severity: "warning",
						WorkspaceID: connection.Workspace, Resource: safeInstanceURL(connection.BaseURL),
						Message: "an operator attested that no legacy Multica System Hook exists on this exact instance; only its unreadable System Hook inventory was skipped. No System Hook is modified, and real-delivery verification and maintenance gates still apply",
					})
					continue
				}
			}
			blockers = append(blockers, fmt.Errorf("list System Hooks for Hook preflight: %w", err))
			report.Findings = append(report.Findings, Finding{Code: "system_hook_inventory_failed", Severity: "error", WorkspaceID: connection.Workspace, Resource: safeInstanceURL(connection.BaseURL), Message: "System Hooks could not be inventoried; an instance administrator must verify read access"})
			continue
		}
		for _, remote := range systemHooks {
			id, idErr := hookID(remote)
			if idErr != nil {
				blockers = append(blockers, idErr)
				continue
			}
			observe("system", strconv.FormatInt(id, 10), "", remote, pgtype.Int8{})
		}
	}
	return errors.Join(blockers...)
}

func (r *Runner) Begin(ctx context.Context) error {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin GitLab migration maintenance window: %w", err)
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, "SELECT pg_advisory_xact_lock($1)", migrationAdvisoryLock); err != nil {
		return fmt.Errorf("lock GitLab migration: %w", err)
	}
	state, err := readState(ctx, tx)
	if err != nil {
		return err
	}
	if state.Phase == "complete" {
		return tx.Commit(ctx)
	}
	if !r.env.VCSIntegrationEnabled {
		return errors.New("MULTICA_VCS_INTEGRATION_ENABLED=true is required to begin cutover with existing GitLab data")
	}
	switch {
	case state.Phase == "maintenance" && state.MaintenanceMode && state.Generation == 0:
		if _, err := tx.Exec(ctx, `UPDATE vcs_gitlab_migration_state
			SET generation=1, began_at=now(), updated_at=now(), last_error=NULL WHERE id=1`); err != nil {
			return fmt.Errorf("record maintenance window: %w", err)
		}
	case state.Phase == "maintenance" && state.MaintenanceMode:
		if _, err := tx.Exec(ctx, `UPDATE vcs_gitlab_migration_state SET updated_at=now(), last_error=NULL WHERE id=1`); err != nil {
			return fmt.Errorf("record maintenance window: %w", err)
		}
	case state.Phase == "restored" && state.MaintenanceMode:
		if _, err := tx.Exec(ctx, `UPDATE vcs_gitlab_migration_state
			SET phase='maintenance', generation=generation+1, began_at=now(), updated_at=now(), last_error=NULL WHERE id=1`); err != nil {
			return fmt.Errorf("begin new maintenance attempt: %w", err)
		}
	default:
		return fmt.Errorf("cannot begin cutover from state %q", state.Phase)
	}
	targetGeneration := systemHookAttestationGeneration(state)
	if err := r.persistSystemHookAttestation(ctx, tx, targetGeneration); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func readState(ctx context.Context, tx pgx.Tx) (MigrationState, error) {
	var state MigrationState
	var beganAt pgtype.Timestamptz
	if err := tx.QueryRow(ctx, `
		SELECT phase, maintenance_mode, generation, began_at, updated_at, COALESCE(last_error, '')
		FROM vcs_gitlab_migration_state WHERE id=1 FOR UPDATE
	`).Scan(&state.Phase, &state.MaintenanceMode, &state.Generation, &beganAt, &state.UpdatedAt, &state.LastError); err != nil {
		return MigrationState{}, fmt.Errorf("read GitLab migration state: %w", err)
	}
	if beganAt.Valid {
		state.BeganAt = beganAt.Time
	}
	return state, nil
}

func (r *Runner) setPhase(ctx context.Context, from []string, phase string, maintenance bool, lastError string) error {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, "SELECT pg_advisory_xact_lock($1)", migrationAdvisoryLock); err != nil {
		return err
	}
	state, err := readState(ctx, tx)
	if err != nil {
		return err
	}
	allowed := false
	for _, candidate := range from {
		if state.Phase == candidate {
			allowed = true
			break
		}
	}
	if !allowed {
		return fmt.Errorf("cannot change GitLab migration phase from %q to %q", state.Phase, phase)
	}
	if _, err := tx.Exec(ctx, `
		UPDATE vcs_gitlab_migration_state
		SET phase=$1, maintenance_mode=$2,
		    updated_at=now(), last_error=NULLIF($3,'')
		WHERE id=1
	`, phase, maintenance, lastError); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func requireMaintenance(state MigrationState) error {
	if !state.MaintenanceMode || state.Phase == "complete" {
		return errors.New("GitLab cutover requires the server to remain in maintenance mode")
	}
	return nil
}

func (r *Runner) failMaintenance(ctx context.Context, message string) {
	_, _ = r.pool.Exec(ctx, `UPDATE vcs_gitlab_migration_state SET last_error=$1, maintenance_mode=true, updated_at=now() WHERE id=1 AND phase<>'complete'`, strings.TrimSpace(message))
}
