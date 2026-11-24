package gitlabmigration

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
)

type migrationConnection struct {
	ID                           string
	LegacyID                     string
	WorkspaceID                  string
	BaseURL                      string
	Token                        string
	WebhookSecret                string
	LegacyWebhookSecret          string
	LegacyWebhookSecretEncrypted string
	API                          *gitlabAPI
}

type projectScope struct {
	WorkspaceID  string
	ConnectionID string
	ProjectID    int64
	Path         string
	HookID       pgtype.Int8
}

type migrationHook struct {
	ConnectionID            string
	WorkspaceID             string
	ScopeType               string
	ScopeID                 string
	ScopePath               string
	LegacyID                int64
	NewID                   pgtype.Int8
	Managed                 bool
	CreatedByTool           bool
	Previous                []byte
	PreviousSecretEncrypted string
	TargetURL               string
	State                   string
	LastError               string
}

var hookEventFields = map[string]string{
	"merge_request": "merge_requests_events",
	"pipeline":      "pipeline_events",
	"job":           "job_events",
	"note":          "note_events",
}

func (r *Runner) SwitchHooks(ctx context.Context) error {
	lockTx, err := r.beginMigrationLock(ctx, "Hook switch")
	if err != nil {
		return err
	}
	defer releaseMigrationLock(lockTx)
	state, err := r.State(ctx)
	if err != nil {
		return err
	}
	if err := requireMaintenance(state); err != nil {
		return err
	}
	if !r.env.VCSIntegrationEnabled {
		return errors.New("MULTICA_VCS_INTEGRATION_ENABLED=true is required before switching GitLab Hooks")
	}
	if state.Phase != "database_migrated" && state.Phase != "hooks_pending" && state.Phase != "verification_pending" {
		return fmt.Errorf("cannot switch GitLab Hooks from phase %q", state.Phase)
	}
	if strings.TrimSpace(r.env.PublicURL) == "" {
		return errors.New("MULTICA_PUBLIC_URL is required to switch GitLab Hooks")
	}
	if err := r.recoverManagedHookCreationIntents(ctx); err != nil {
		return fmt.Errorf("recover interrupted managed Hook creation: %w", err)
	}
	if err := r.inventoryHooks(ctx); err != nil {
		return fmt.Errorf("inventory GitLab Hooks: %w", err)
	}
	connections, err := r.loadMigrationConnections(ctx)
	if err != nil {
		return err
	}
	hooks, err := r.loadMigrationHooks(ctx)
	if err != nil {
		return err
	}
	var failures []error
	for _, hook := range hooks {
		if hook.State == "restored" || strings.Contains(hook.LastError, "shared legacy Hook") {
			if strings.Contains(hook.LastError, "shared legacy Hook") {
				failures = append(failures, fmt.Errorf("Hook %d in %s scope %s is shared across workspace connections; split it into connection-specific Hooks, remove the legacy Hook, then rerun switch-hooks", hook.LegacyID, hook.ScopeType, hook.ScopeID))
			}
			continue
		}
		connection, ok := connections[hook.ConnectionID]
		if !ok {
			failures = append(failures, fmt.Errorf("Hook %s/%s has no workspace-scoped VCS connection", hook.ScopeType, hook.ScopeID))
			_ = r.markHookFailure(ctx, hook, "workspace-scoped VCS connection is unavailable")
			continue
		}
		remoteID := hook.LegacyID
		if hook.NewID.Valid {
			remoteID = hook.NewID.Int64
		}
		if remoteID <= 0 {
			if hook.Managed && hook.ScopeType == "project" {
				if err := r.createManagedProjectHook(ctx, connection, hook); err != nil {
					failures = append(failures, err)
					_ = r.markHookFailure(ctx, hook, err.Error())
					continue
				}
				continue
			}
			err := fmt.Errorf("manual %s Hook for scope %s is not configured; create it in GitLab and rerun switch-hooks", hook.ScopeType, hook.ScopeID)
			failures = append(failures, err)
			_ = r.markHookFailure(ctx, hook, err.Error())
			continue
		}
		remote, err := connection.API.getHook(ctx, hook.ScopeType, scopeNumericID(hook), remoteID)
		if err != nil {
			if hook.LegacyID > 0 && strings.Contains(hook.LastError, "shared legacy Hook") && isGitLabNotFound(err) {
				continue
			}
			failures = append(failures, fmt.Errorf("read GitLab %s Hook %d: %w", hook.ScopeType, remoteID, err))
			_ = r.markHookFailure(ctx, hook, "Hook could not be read or updated; verify GitLab permissions and the remote identity")
			continue
		}
		target, err := manualHookURL(r.env.PublicURL, connection.ID, hook.ScopeType, hook.ScopeID, remoteID)
		if err != nil {
			failures = append(failures, err)
			continue
		}
		if hookURLMatches(hookURL(remote), target) {
			if hook.LegacyID > 0 {
				if len(hook.Previous) == 0 || string(hook.Previous) == "{}" || hook.PreviousSecretEncrypted == "" {
					err := fmt.Errorf("legacy GitLab Hook %d has no recoverable pre-switch configuration and secret", remoteID)
					failures = append(failures, err)
					_ = r.markHookFailure(ctx, hook, err.Error())
					continue
				}
				body := hookUpdateBody(remote, target)
				if hook.Managed {
					for _, field := range hookEventFields {
						body[field] = true
					}
				}
				body["token"] = connection.WebhookSecret
				updated, err := connection.API.updateHook(ctx, hook.ScopeType, scopeNumericID(hook), remoteID, body)
				if err != nil || !hookURLMatches(hookURL(updated), target) {
					if err == nil {
						err = errors.New("GitLab did not retain the workspace-specific Hook URL")
					}
					failures = append(failures, err)
					_ = r.markHookFailure(ctx, hook, "Hook update or token rotation could not be verified")
					continue
				}
			}
			if err := r.recordHookUpdate(ctx, hook, remoteID, target, hook.Previous, "updated", "", false); err != nil {
				failures = append(failures, err)
				continue
			}
			if hook.Managed {
				if err := r.updateManagedProjectHook(ctx, hook, remoteID); err != nil {
					failures = append(failures, err)
				}
			}
			continue
		}
		if hook.LegacyID <= 0 && !hook.Managed && !hookURLMatches(hookURL(remote), target) {
			message := fmt.Sprintf("manual Hook %d for %s must be set to the exact workspace-specific route %s", remoteID, hook.ScopePath, target)
			failures = append(failures, errors.New(message))
			_ = r.markHookFailure(ctx, hook, message)
			continue
		}
		previous := hook.Previous
		if len(previous) == 0 || string(previous) == "{}" {
			previous, err = jsonSafeHookConfig(remote)
			if err != nil {
				failures = append(failures, fmt.Errorf("snapshot GitLab Hook %d: %w", remoteID, err))
				continue
			}
		}
		body := hookUpdateBody(remote, target)
		if hook.Managed {
			for _, field := range hookEventFields {
				body[field] = true
			}
		}
		if hook.LegacyID > 0 {
			if hook.PreviousSecretEncrypted == "" {
				err := fmt.Errorf("legacy GitLab Hook %d has no recoverable secret", remoteID)
				failures = append(failures, err)
				_ = r.markHookFailure(ctx, hook, err.Error())
				continue
			}
			body["token"] = connection.WebhookSecret
		}
		// GitLab may deliver to the new URL before this API call returns. Persist
		// the exact Hook intent with its rollback snapshot before changing it.
		if err := r.recordHookUpdate(ctx, hook, remoteID, target, previous, "pending", "", false); err != nil {
			failures = append(failures, err)
			continue
		}
		updated, err := connection.API.updateHook(ctx, hook.ScopeType, scopeNumericID(hook), remoteID, body)
		if err != nil {
			message := "Hook update permission was denied; update the displayed workspace-specific URL in GitLab and rerun switch-hooks"
			if persistErr := r.recordHookUpdate(ctx, hook, remoteID, target, previous, "pending", message, false); persistErr != nil {
				failures = append(failures, fmt.Errorf("retain pending GitLab %s Hook %d update intent: %w", hook.ScopeType, remoteID, persistErr))
			}
			failures = append(failures, fmt.Errorf("update GitLab %s Hook %d: %w", hook.ScopeType, remoteID, err))
			continue
		}
		updatedID, err := hookID(updated)
		if err != nil {
			failures = append(failures, err)
			continue
		}
		if !hookURLMatches(hookURL(updated), target) {
			failures = append(failures, fmt.Errorf("GitLab %s Hook %d did not retain its target URL", hook.ScopeType, updatedID))
			continue
		}
		if err := r.recordHookUpdate(ctx, hook, updatedID, target, previous, "updated", "", false); err != nil {
			failures = append(failures, err)
			continue
		}
		if hook.Managed {
			if err := r.updateManagedProjectHook(ctx, hook, updatedID); err != nil {
				failures = append(failures, err)
			}
		}
	}
	releaseMigrationLock(lockTx)
	if len(failures) != 0 {
		message := errors.Join(failures...).Error()
		r.failMaintenance(ctx, message)
		current, stateErr := r.State(ctx)
		if stateErr == nil && current.Phase != "hooks_pending" {
			_ = r.setPhase(ctx, []string{current.Phase}, "hooks_pending", true, message)
		}
		return errors.Join(failures...)
	}
	current, err := r.State(ctx)
	if err != nil {
		return err
	}
	if current.Phase != "verification_pending" {
		return r.setPhase(ctx, []string{current.Phase}, "verification_pending", true, "")
	}
	return nil
}

func (r *Runner) createManagedProjectHook(ctx context.Context, connection migrationConnection, hook migrationHook) error {
	projectID, err := strconv.ParseInt(hook.ScopeID, 10, 64)
	if err != nil || projectID <= 0 {
		return errors.New("managed GitLab project Hook has an invalid project ID")
	}
	placeholder, err := manualHookURL(r.env.PublicURL, connection.ID, "project", hook.ScopeID, 0)
	if err != nil {
		return err
	}
	if err := r.recordHookCreationIntent(ctx, hook, placeholder); err != nil {
		return err
	}
	remoteHooks, err := connection.API.listHooks(ctx, "project", projectID)
	if err != nil {
		return fmt.Errorf("inspect managed project Hooks before create: %w", err)
	}
	var created map[string]any
	for _, candidate := range remoteHooks {
		if hookURLMatches(hookURL(candidate), placeholder) {
			if created != nil {
				return fmt.Errorf("multiple project Hooks match the pending creation intent for %s; remove duplicates and rerun", hook.ScopePath)
			}
			created = candidate
		}
	}
	if created == nil {
		body := map[string]any{
			"url": placeholder, "token": connection.WebhookSecret, "enable_ssl_verification": true,
			"merge_requests_events": true, "pipeline_events": true, "job_events": true, "note_events": true,
		}
		created, err = connection.API.createProjectHook(ctx, projectID, body)
		if err != nil {
			return fmt.Errorf("create managed project Hook for %s: %w", hook.ScopePath, err)
		}
	}
	id, err := hookID(created)
	if err != nil {
		return fmt.Errorf("read created project Hook identity: %w", err)
	}
	target, err := manualHookURL(r.env.PublicURL, connection.ID, "project", hook.ScopeID, id)
	if err != nil {
		return err
	}
	if err := r.recordHookUpdate(ctx, hook, id, target, []byte(`{}`), "pending", "", true); err != nil {
		return err
	}
	updated, err := connection.API.updateHook(ctx, "project", projectID, id, hookUpdateBody(created, target))
	if err != nil {
		return fmt.Errorf("set canonical URL on new project Hook %d: %w", id, err)
	}
	if !hookURLMatches(hookURL(updated), target) {
		return fmt.Errorf("new project Hook %d did not retain its canonical URL", id)
	}
	if err := r.recordHookUpdate(ctx, hook, id, target, []byte(`{}`), "updated", "", true); err != nil {
		return err
	}
	return r.updateManagedProjectHook(ctx, hook, id)
}
func (r *Runner) recordHookCreationIntent(ctx context.Context, hook migrationHook, placeholderURL string) error {
	tag, err := r.pool.Exec(ctx, `
		UPDATE vcs_gitlab_migration_hook
		SET created_by_tool=TRUE,target_url=$4,state='pending',last_error=NULL,updated_at=now()
		WHERE connection_id=$1 AND scope_type=$2 AND scope_id=$3 AND legacy_hook_id=0
		  AND new_hook_id IS NULL
	`, hook.ConnectionID, hook.ScopeType, hook.ScopeID, placeholderURL)
	if err != nil {
		return fmt.Errorf("persist managed Hook creation intent: %w", err)
	}
	if tag.RowsAffected() != 1 {
		return errors.New("managed Hook creation intent is no longer pending; rerun after checking migration status")
	}
	return nil
}

func (r *Runner) recoverManagedHookCreationIntents(ctx context.Context) error {
	rows, err := r.pool.Query(ctx, `
		SELECT connection_id::text,workspace_id::text,scope_id,scope_path,target_url
		FROM vcs_gitlab_migration_hook
		WHERE scope_type='project' AND legacy_hook_id=0 AND created_by_tool
		  AND new_hook_id IS NULL AND state<>'restored'
		ORDER BY connection_id,scope_id
	`)
	if err != nil {
		return fmt.Errorf("list pending managed Hook creation intents: %w", err)
	}
	type intent struct {
		hook migrationHook
	}
	var intents []intent
	for rows.Next() {
		var item intent
		item.hook.ScopeType = "project"
		if err := rows.Scan(&item.hook.ConnectionID, &item.hook.WorkspaceID, &item.hook.ScopeID, &item.hook.ScopePath, &item.hook.TargetURL); err != nil {
			rows.Close()
			return err
		}
		intents = append(intents, item)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	rows.Close()
	if len(intents) == 0 {
		return nil
	}
	connections, err := r.loadMigrationConnections(ctx)
	if err != nil {
		return err
	}
	for _, item := range intents {
		connection, ok := connections[item.hook.ConnectionID]
		if !ok {
			return fmt.Errorf("pending managed Hook for %s has no workspace-scoped connection", item.hook.ScopePath)
		}
		projectID, err := strconv.ParseInt(item.hook.ScopeID, 10, 64)
		if err != nil || projectID <= 0 {
			return fmt.Errorf("pending managed Hook for %s has an invalid project ID", item.hook.ScopePath)
		}
		remoteHooks, err := connection.API.listHooks(ctx, "project", projectID)
		if err != nil {
			return fmt.Errorf("reconcile pending managed Hook for %s: %w", item.hook.ScopePath, err)
		}
		var found map[string]any
		for _, remote := range remoteHooks {
			if hookURLMatches(hookURL(remote), item.hook.TargetURL) {
				if found != nil {
					return fmt.Errorf("multiple project Hooks match the pending creation intent for %s; remove duplicates and rerun", item.hook.ScopePath)
				}
				found = remote
			}
		}
		if found == nil {
			continue
		}
		id, err := hookID(found)
		if err != nil {
			return fmt.Errorf("read interrupted managed Hook identity for %s: %w", item.hook.ScopePath, err)
		}
		if err := r.recordHookUpdate(ctx, item.hook, id, item.hook.TargetURL, []byte(`{}`), "pending", "", true); err != nil {
			return err
		}
	}
	return nil
}

func (r *Runner) updateManagedProjectHook(ctx context.Context, hook migrationHook, hookID int64) error {
	projectID, err := strconv.ParseInt(hook.ScopeID, 10, 64)
	if err != nil || projectID <= 0 {
		return fmt.Errorf("managed GitLab project Hook has invalid project ID %q", hook.ScopeID)
	}
	tag, err := r.pool.Exec(ctx, `
		UPDATE vcs_gitlab_project
		SET hook_id=$3, hook_enabled=true, last_sync_error=NULL, updated_at=now()
		WHERE workspace_id=$1 AND connection_id=$2 AND gitlab_project_id=$4
	`, hook.WorkspaceID, hook.ConnectionID, hookID, projectID)
	if err != nil {
		return fmt.Errorf("record managed GitLab project Hook state: %w", err)
	}
	if tag.RowsAffected() != 1 {
		return fmt.Errorf("canonical GitLab project %s was missing while recording its Hook", hook.ScopePath)
	}
	return nil
}

func (r *Runner) recordHookUpdate(ctx context.Context, hook migrationHook, remoteID int64, target string, previous []byte, state, lastError string, createdByTool bool) error {
	tag, err := r.pool.Exec(ctx, `
		UPDATE vcs_gitlab_migration_hook
		SET new_hook_id=$5, target_url=$6, created_by_tool=created_by_tool OR $10,
		    previous_config=CASE WHEN previous_config='{}'::jsonb AND $7::jsonb <> '{}'::jsonb THEN $7::jsonb ELSE previous_config END,
		    state=$8, last_error=NULLIF($9,''), updated_at=now()
		WHERE connection_id=$1 AND scope_type=$2 AND scope_id=$3 AND legacy_hook_id=$4
	`, hook.ConnectionID, hook.ScopeType, hook.ScopeID, hook.LegacyID, remoteID, target, string(previous), state, lastError, createdByTool)
	if err != nil {
		return fmt.Errorf("persist GitLab Hook cutover state: %w", err)
	}
	if tag.RowsAffected() != 1 {
		return errors.New("GitLab Hook migration ledger row disappeared during update")
	}
	return nil
}

func (r *Runner) markHookFailure(ctx context.Context, hook migrationHook, message string) error {
	_, err := r.pool.Exec(ctx, `
		UPDATE vcs_gitlab_migration_hook SET state='manual_required',last_error=$5,updated_at=now()
		WHERE connection_id=$1 AND scope_type=$2 AND scope_id=$3 AND legacy_hook_id=$4
	`, hook.ConnectionID, hook.ScopeType, hook.ScopeID, hook.LegacyID, redactHookError(message))
	return err
}

func redactHookError(message string) string {
	message = strings.TrimSpace(message)
	if len(message) > 1000 {
		message = message[:1000]
	}
	return message
}

func (r *Runner) VerifyHooks(ctx context.Context, since time.Time) error {
	return r.verifyHooks(ctx, since, true)
}

func (r *Runner) VerifyConfiguredHooks(ctx context.Context, since time.Time) error {
	return r.verifyHooks(ctx, since, false)
}

func (r *Runner) verifyHooks(ctx context.Context, since time.Time, requireDeliveries bool) error {
	state, err := r.State(ctx)
	if err != nil {
		return err
	}
	if err := requireMaintenance(state); err != nil {
		return err
	}
	if !r.env.VCSIntegrationEnabled {
		return errors.New("MULTICA_VCS_INTEGRATION_ENABLED=true is required to verify GitLab Hooks")
	}
	if state.Phase != "verification_pending" && state.Phase != "ready" {
		return fmt.Errorf("cannot verify GitLab Hooks from phase %q; run switch-hooks first", state.Phase)
	}
	if state.Phase == "ready" {
		if err := r.setPhase(ctx, []string{"ready"}, "verification_pending", true, ""); err != nil {
			return err
		}
	}
	lockTx, err := r.beginMigrationLock(ctx, "Hook verification")
	if err != nil {
		return err
	}
	defer func() { releaseMigrationLock(lockTx) }()
	state, err = r.State(ctx)
	if err != nil {
		return err
	}
	if err := requireMaintenance(state); err != nil {
		return err
	}
	if state.Phase != "verification_pending" {
		return fmt.Errorf("cannot verify GitLab Hooks from phase %q; retry verification from maintenance", state.Phase)
	}
	var pendingDeliveries, orphanDeliveries int64
	if err := r.pool.QueryRow(ctx, `
		SELECT count(*) FROM vcs_gitlab_migration_delivery
		WHERE received_at >= $1 AND (reconciled_at IS NULL OR payload IS NULL)
	`, since).Scan(&pendingDeliveries); err != nil {
		return fmt.Errorf("check replayed GitLab delivery receipts: %w", err)
	}
	if pendingDeliveries != 0 {
		return fmt.Errorf("GitLab verification blocked by %d delivery receipts not yet authoritatively reconciled; run replay", pendingDeliveries)
	}
	if err := r.pool.QueryRow(ctx, `
		SELECT count(*) FROM vcs_gitlab_migration_delivery d
		WHERE d.received_at >= $1 AND NOT EXISTS (
			SELECT 1 FROM vcs_gitlab_migration_hook h
			WHERE h.connection_id=d.connection_id AND h.workspace_id=d.workspace_id
			  AND h.scope_type=d.scope_type AND h.scope_id=d.scope_id
			  AND (h.legacy_hook_id=d.hook_id OR h.new_hook_id=d.hook_id)
		)
	`, since).Scan(&orphanDeliveries); err != nil {
		return fmt.Errorf("check delivery Hook identities: %w", err)
	}
	if orphanDeliveries != 0 {
		return fmt.Errorf("GitLab verification found %d receipts without a matching Hook ledger identity", orphanDeliveries)
	}
	connections, err := r.loadMigrationConnections(ctx)
	if err != nil {
		return err
	}
	hooks, err := r.loadMigrationHooks(ctx)
	if err != nil {
		return err
	}
	projects, err := r.loadProjectScopes(ctx)
	if err != nil {
		return err
	}
	coverage := make(map[string]map[string]bool)
	var failures []error
	for _, hook := range hooks {
		if hook.State != "updated" && hook.State != "verified" {
			failures = append(failures, fmt.Errorf("GitLab %s Hook for %s is not switched (state %s)", hook.ScopeType, hook.ScopePath, hook.State))
			continue
		}
		connection, ok := connections[hook.ConnectionID]
		if !ok {
			failures = append(failures, fmt.Errorf("Hook %s/%s has no workspace-scoped connection", hook.ScopeType, hook.ScopeID))
			continue
		}
		hookID := hook.LegacyID
		if hook.NewID.Valid {
			hookID = hook.NewID.Int64
		}
		if hookID <= 0 {
			failures = append(failures, fmt.Errorf("GitLab %s Hook for %s has no verified remote identity", hook.ScopeType, hook.ScopePath))
			continue
		}
		remote, err := connection.API.getHook(ctx, hook.ScopeType, scopeNumericID(hook), hookID)
		if err != nil {
			failures = append(failures, fmt.Errorf("verify GitLab %s Hook %d: %w", hook.ScopeType, hookID, err))
			continue
		}
		target, err := manualHookURL(r.env.PublicURL, connection.ID, hook.ScopeType, hook.ScopeID, hookID)
		if err != nil || !hookURLMatches(hookURL(remote), target) {
			failures = append(failures, fmt.Errorf("GitLab %s Hook %d does not use its exact workspace-scoped URL", hook.ScopeType, hookID))
			continue
		}
		receipts := map[string]string{}
		if requireDeliveries {
			receipts, err = r.hookReceipts(ctx, hook, hookID, since)
			if err != nil {
				failures = append(failures, err)
				continue
			}
		}
		enabled := make(map[string]bool)
		for category, field := range hookEventFields {
			if hook.ScopeType == "system" && category != "merge_request" && category != "pipeline" {
				continue
			}
			if hookEnabled(remote, field) {
				enabled[category] = true
			}
		}
		verifiedDelivery := ""
		if requireDeliveries {
			var missing []string
			for category := range enabled {
				if deliveryID := receipts[category]; deliveryID != "" {
					if verifiedDelivery == "" {
						verifiedDelivery = deliveryID
					}
				} else {
					missing = append(missing, category)
				}
			}
			sort.Strings(missing)
			if len(missing) != 0 {
				failures = append(failures, fmt.Errorf("GitLab %s Hook %d has enabled event types without real reconciled deliveries since %s: %s", hook.ScopeType, hookID, since.UTC().Format(time.RFC3339), strings.Join(missing, ", ")))
				continue
			}
		}
		if _, err := r.pool.Exec(ctx, `
			UPDATE vcs_gitlab_migration_hook
			SET state='verified',verified_delivery_id=NULLIF($5,''),verified_at=now(),last_error=NULL,updated_at=now()
			WHERE connection_id=$1 AND scope_type=$2 AND scope_id=$3 AND legacy_hook_id=$4
		`, hook.ConnectionID, hook.ScopeType, hook.ScopeID, hook.LegacyID, verifiedDelivery); err != nil {
			failures = append(failures, fmt.Errorf("record verified GitLab Hook: %w", err))
			continue
		}
		for category := range enabled {
			if requireDeliveries && receipts[category] == "" {
				continue
			}
			for _, project := range projects {
				if project.ConnectionID == hook.ConnectionID && hookApplies(hook, project) {
					key := project.ConnectionID + ":" + strconv.FormatInt(project.ProjectID, 10)
					if coverage[key] == nil {
						coverage[key] = make(map[string]bool)
					}
					coverage[key][category] = true
				}
			}
		}
	}
	for _, project := range projects {
		key := project.ConnectionID + ":" + strconv.FormatInt(project.ProjectID, 10)
		for _, category := range []string{"merge_request", "pipeline", "job", "note"} {
			if !coverage[key][category] {
				failures = append(failures, fmt.Errorf("project %s has no verified Hook for %s events", project.Path, category))
			}
		}
	}
	if len(failures) != 0 {
		releaseMigrationLock(lockTx)
		lockTx = nil
		message := errors.Join(failures...).Error()
		r.failMaintenance(ctx, message)
		current, stateErr := r.State(ctx)
		if stateErr == nil && current.Phase == "ready" {
			_ = r.setPhase(ctx, []string{"ready"}, "verification_pending", true, message)
		}
		return errors.Join(failures...)
	}
	current, err := r.State(ctx)
	if err != nil {
		return err
	}
	if current.Phase != "ready" {
		releaseMigrationLock(lockTx)
		lockTx = nil
		return r.setPhase(ctx, []string{current.Phase}, "ready", true, "")
	}
	releaseMigrationLock(lockTx)
	lockTx = nil
	return nil
}


func (r *Runner) hookReceipts(ctx context.Context, hook migrationHook, hookID int64, since time.Time) (map[string]string, error) {
	rows, err := r.pool.Query(ctx, `
		SELECT delivery_id,event_type
		FROM vcs_gitlab_migration_delivery
		WHERE workspace_id=$1 AND connection_id=$2 AND scope_type=$3 AND scope_id=$4 AND hook_id=$5
		  AND received_at >= $6 AND reconciled_at IS NOT NULL AND payload IS NOT NULL
		ORDER BY received_at,delivery_id
	`, hook.WorkspaceID, hook.ConnectionID, hook.ScopeType, hook.ScopeID, hookID, since)
	if err != nil {
		return nil, fmt.Errorf("read GitLab Hook delivery evidence: %w", err)
	}
	defer rows.Close()
	out := make(map[string]string)
	for rows.Next() {
		var id, eventType string
		if err := rows.Scan(&id, &eventType); err != nil {
			return nil, err
		}
		category := receiptEventCategory(eventType)
		if category != "" && out[category] == "" {
			out[category] = id
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

func receiptEventCategory(eventType string) string {
	value := strings.ToLower(strings.TrimSpace(eventType))
	if strings.Contains(value, "merge request") || strings.HasSuffix(value, "/merge_request") || value == "merge_request" {
		return "merge_request"
	}
	if strings.Contains(value, "pipeline") {
		return "pipeline"
	}
	if strings.Contains(value, "job") {
		return "job"
	}
	if strings.Contains(value, "note") {
		return "note"
	}
	return ""
}

func hookApplies(hook migrationHook, project projectScope) bool {
	switch hook.ScopeType {
	case "project":
		return hook.ScopeID == strconv.FormatInt(project.ProjectID, 10)
	case "group":
		return projectGroupAncestor(hook.ScopePath, project.Path)
	case "system":
		return true
	default:
		return false
	}
}

func shouldDeleteHookOnRestore(hook migrationHook) bool {
	return hook.LegacyID <= 0 && hook.CreatedByTool
}

func (r *Runner) restoreHooks(ctx context.Context) error {
	lockTx, err := r.beginMigrationLock(ctx, "Hook restore")
	if err != nil {
		return err
	}
	defer releaseMigrationLock(lockTx)
	var count int64
	if err := r.pool.QueryRow(ctx, `SELECT count(*) FROM vcs_gitlab_migration_hook WHERE state <> 'restored'`).Scan(&count); err != nil {
		return fmt.Errorf("count GitLab Hooks to restore: %w", err)
	}
	if count == 0 {
		return nil
	}
	if err := r.recoverManagedHookCreationIntents(ctx); err != nil {
		return fmt.Errorf("recover interrupted managed Hook creation before rollback: %w", err)
	}
	box, err := r.env.box()
	if err != nil {
		return err
	}
	connections, err := r.loadMigrationConnections(ctx)
	if err != nil {
		return err
	}
	hooks, err := r.loadMigrationHooks(ctx)
	if err != nil {
		return err
	}
	var failures []error
	for _, hook := range hooks {
		if hook.State == "restored" {
			continue
		}
		connection, ok := connections[hook.ConnectionID]
		if !ok {
			message := "workspace-scoped VCS connection is unavailable"
			_ = r.markRestoreFailure(ctx, hook, message)
			failures = append(failures, fmt.Errorf("restore Hook %s/%s: %s", hook.ScopeType, hook.ScopeID, message))
			continue
		}
		remoteID := hook.NewID.Int64
		if !hook.NewID.Valid {
			remoteID = hook.LegacyID
		}
		if hook.LegacyID <= 0 {
			if shouldDeleteHookOnRestore(hook) && remoteID > 0 {
				err := connection.API.deleteHook(ctx, hook.ScopeType, scopeNumericID(hook), remoteID)
				if err != nil && !isGitLabNotFound(err) {
					_ = r.markRestoreFailure(ctx, hook, "tool-created Hook could not be deleted")
					failures = append(failures, fmt.Errorf("delete tool-created GitLab %s Hook %d: %w", hook.ScopeType, remoteID, err))
					continue
				}
			}
			if err := r.markRestored(ctx, hook, remoteID); err != nil {
				failures = append(failures, err)
			}
			continue
		}
		if len(hook.Previous) == 0 || string(hook.Previous) == "{}" {
			message := "previous Hook configuration snapshot is missing"
			_ = r.markRestoreFailure(ctx, hook, message)
			failures = append(failures, fmt.Errorf("restore GitLab %s Hook %d: %s", hook.ScopeType, hook.LegacyID, message))
			continue
		}
		var previous map[string]any
		if err := json.Unmarshal(hook.Previous, &previous); err != nil {
			message := "previous Hook configuration snapshot is invalid"
			_ = r.markRestoreFailure(ctx, hook, message)
			failures = append(failures, fmt.Errorf("restore GitLab %s Hook %d: %s", hook.ScopeType, hook.LegacyID, message))
			continue
		}
		oldURL := hookURL(previous)
		if oldURL == "" {
			message := "previous Hook URL is missing from snapshot"
			_ = r.markRestoreFailure(ctx, hook, message)
			failures = append(failures, fmt.Errorf("restore GitLab %s Hook %d: %s", hook.ScopeType, hook.LegacyID, message))
			continue
		}
		if hook.PreviousSecretEncrypted == "" {
			message := "encrypted pre-cutover Hook secret is missing"
			_ = r.markRestoreFailure(ctx, hook, message)
			failures = append(failures, fmt.Errorf("restore GitLab %s Hook %d: %s", hook.ScopeType, hook.LegacyID, message))
			continue
		}
		previousSecret, err := openSecret(box, hook.PreviousSecretEncrypted)
		if err != nil {
			_ = r.markRestoreFailure(ctx, hook, "encrypted pre-cutover Hook secret could not be decrypted")
			failures = append(failures, fmt.Errorf("restore GitLab %s Hook %d: previous secret could not be decrypted", hook.ScopeType, hook.LegacyID))
			continue
		}
		_, getErr := connection.API.getHook(ctx, hook.ScopeType, scopeNumericID(hook), hook.LegacyID)
		if getErr != nil && !isGitLabNotFound(getErr) {
			_ = r.markRestoreFailure(ctx, hook, "original Hook could not be read")
			failures = append(failures, fmt.Errorf("read original GitLab %s Hook %d: %w", hook.ScopeType, hook.LegacyID, getErr))
			continue
		}
		body := hookUpdateBody(previous, oldURL)
		body["token"] = previousSecret
		var restored map[string]any
		if isGitLabNotFound(getErr) {
			restored, err = connection.API.createHook(ctx, hook.ScopeType, scopeNumericID(hook), body)
		} else {
			restored, err = connection.API.updateHook(ctx, hook.ScopeType, scopeNumericID(hook), hook.LegacyID, body)
		}
		if err != nil {
			_ = r.markRestoreFailure(ctx, hook, "original Hook configuration could not be restored")
			failures = append(failures, fmt.Errorf("restore GitLab %s Hook %d: %w", hook.ScopeType, hook.LegacyID, err))
			continue
		}
		restoredID, err := hookID(restored)
		if err != nil {
			_ = r.markRestoreFailure(ctx, hook, "restored Hook identity could not be read")
			failures = append(failures, err)
			continue
		}
		if !hookURLMatches(hookURL(restored), oldURL) {
			_ = r.markRestoreFailure(ctx, hook, "restored Hook did not retain its previous URL")
			failures = append(failures, fmt.Errorf("restored GitLab %s Hook %d did not retain its previous URL", hook.ScopeType, restoredID))
			continue
		}
		if err := r.markRestored(ctx, hook, restoredID); err != nil {
			failures = append(failures, err)
		}
	}
	return errors.Join(failures...)
}

func (r *Runner) markRestoreFailure(ctx context.Context, hook migrationHook, message string) error {
	_, err := r.pool.Exec(ctx, `
		UPDATE vcs_gitlab_migration_hook SET state='restore_blocked',last_error=$5,updated_at=now()
		WHERE connection_id=$1 AND scope_type=$2 AND scope_id=$3 AND legacy_hook_id=$4
	`, hook.ConnectionID, hook.ScopeType, hook.ScopeID, hook.LegacyID, message)
	return err
}

func (r *Runner) markRestored(ctx context.Context, hook migrationHook, restoredID int64) error {
	_, err := r.pool.Exec(ctx, `
		UPDATE vcs_gitlab_migration_hook
		SET state='restored',new_hook_id=NULLIF($5,0),last_error=NULL,updated_at=now()
		WHERE connection_id=$1 AND scope_type=$2 AND scope_id=$3 AND legacy_hook_id=$4
	`, hook.ConnectionID, hook.ScopeType, hook.ScopeID, hook.LegacyID, restoredID)
	if err != nil {
		return fmt.Errorf("record restored GitLab Hook: %w", err)
	}
	return nil
}

func (r *Runner) inventoryHooks(ctx context.Context) error {
	connections, err := r.loadMigrationConnections(ctx)
	if err != nil {
		return err
	}
	projects, err := r.loadProjectScopes(ctx)
	if err != nil {
		return err
	}
	var failures []error
	connectionCountByInstance := make(map[string]int)
	for _, connection := range connections {
		connectionCountByInstance[normalizeInstanceURL(connection.BaseURL)]++
	}
	for _, connection := range connections {
		projectRows := make([]projectScope, 0)
		for _, project := range projects {
			if project.ConnectionID == connection.ID {
				projectRows = append(projectRows, project)
			}
		}
		for _, project := range projectRows {
			hooks, err := connection.API.listHooks(ctx, "project", project.ProjectID)
			if err != nil {
				failures = append(failures, fmt.Errorf("list project Hooks for %s: %w", project.Path, err))
				continue
			}
			for _, remote := range hooks {
				managed := project.HookID.Valid && project.HookID.Int64 > 0 && project.HookID.Int64 == hookIDMust(remote)
				if err := r.captureHook(ctx, connection, "project", project.ProjectID, project.Path, remote, managed, connectionCountByInstance[normalizeInstanceURL(connection.BaseURL)]); err != nil {
					failures = append(failures, err)
				}
			}
			if err := r.ensureManagedProjectHookPlaceholder(ctx, connection, project); err != nil {
				failures = append(failures, err)
			}
		}
		if !connection.API.groupHooksUnsupportedByMetadata(ctx) {
			groups, err := connection.API.listGroups(ctx)
			if err != nil {
				failures = append(failures, fmt.Errorf("list groups to inspect GitLab Hooks for %s: %w", safeInstanceURL(connection.BaseURL), err))
			} else {
				groupsByPath := make(map[string]map[string]any, len(groups))
				for _, group := range groups {
					groupPath, _ := group["full_path"].(string)
					if groupPath != "" {
						groupsByPath[groupPath] = group
					}
				}
				seenGroups := make(map[int64]struct{})
				for _, project := range projectRows {
					for _, groupPath := range projectGroupPaths(project.Path) {
						group, ok := groupsByPath[groupPath]
						if !ok {
							failures = append(failures, fmt.Errorf("cannot verify GitLab group Hook inventory for %s: ancestor group %q is not visible to this connection", project.Path, groupPath))
							continue
						}
						groupID, idErr := hookID(group)
						if idErr != nil {
							failures = append(failures, fmt.Errorf("read GitLab group identity for %s: %w", groupPath, idErr))
							continue
						}
						if _, seen := seenGroups[groupID]; seen {
							continue
						}
						seenGroups[groupID] = struct{}{}
						groupHooks, listErr := connection.API.listHooks(ctx, "group", groupID)
						if listErr != nil {
							unsupported, accessErr := connection.API.groupHooksUnsupportedForGroup(ctx, groupID, listErr)
							if accessErr != nil {
								failures = append(failures, fmt.Errorf("inspect GitLab group Hooks for %s: %w", groupPath, accessErr))
							} else if !unsupported {
								failures = append(failures, fmt.Errorf("list GitLab group Hooks for %s: %w", groupPath, listErr))
							}
							continue
						}
						for _, remote := range groupHooks {
							if err := r.captureHook(ctx, connection, "group", groupID, groupPath, remote, false, connectionCountByInstance[normalizeInstanceURL(connection.BaseURL)]); err != nil {
								failures = append(failures, err)
							}
						}
					}
				}
			}
		}
		systemHooks, err := connection.API.listHooks(ctx, "system", 0)
		if err != nil {
			if gitLabHTTPStatus(err) == 403 {
				attestation, attestationErr := r.activeSystemHookAttestation(ctx, connection.BaseURL)
				if attestationErr != nil {
					failures = append(failures, fmt.Errorf("read System Hook inventory attestation: %w", attestationErr))
					continue
				}
				if shouldSkipSystemHookInventory(connection.BaseURL, err, attestation) {
					continue
				}
			}
			failures = append(failures, fmt.Errorf("list GitLab System Hooks for %s: %w", safeInstanceURL(connection.BaseURL), err))
			continue
		}
		for _, remote := range systemHooks {
			if err := r.captureHook(ctx, connection, "system", 0, "", remote, false, connectionCountByInstance[normalizeInstanceURL(connection.BaseURL)]); err != nil {
				failures = append(failures, err)
			}
		}
	}
	return errors.Join(failures...)
}

func (r *Runner) ensureManagedProjectHookPlaceholder(ctx context.Context, connection migrationConnection, project projectScope) error {
	scopeID := strconv.FormatInt(project.ProjectID, 10)
	var tracked int64
	if err := r.pool.QueryRow(ctx, `
		SELECT count(*) FROM vcs_gitlab_migration_hook
		WHERE connection_id=$1 AND scope_type='project' AND scope_id=$2 AND state<>'restored'
	`, connection.ID, scopeID).Scan(&tracked); err != nil {
		return fmt.Errorf("check managed Hook ledger for %s: %w", project.Path, err)
	}
	if tracked != 0 {
		return nil
	}
	target, err := manualHookURL(r.env.PublicURL, connection.ID, "project", scopeID, 0)
	if err != nil {
		return err
	}
	_, err = r.pool.Exec(ctx, `
		INSERT INTO vcs_gitlab_migration_hook (
			connection_id,workspace_id,scope_type,scope_id,scope_path,legacy_hook_id,new_hook_id,
			managed,created_by_tool,previous_config,previous_secret_encrypted,target_url,state
		) VALUES ($1,$2,'project',$3,$4,0,NULL,TRUE,FALSE,'{}'::jsonb,'',$5,'pending')
		ON CONFLICT (connection_id,scope_type,scope_id,legacy_hook_id) DO UPDATE SET
			new_hook_id=NULL,managed=TRUE,created_by_tool=FALSE,previous_config='{}'::jsonb,
			previous_secret_encrypted='',target_url=EXCLUDED.target_url,state='pending',
			last_error=NULL,updated_at=now()
		WHERE vcs_gitlab_migration_hook.state='restored'
	`, connection.ID, connection.WorkspaceID, scopeID, project.Path, target)
	if err != nil {
		return fmt.Errorf("record pending managed Hook for %s: %w", project.Path, err)
	}
	return nil
}

func (r *Runner) captureHook(ctx context.Context, connection migrationConnection, scopeType string, actualScopeID int64, scopePath string, remote map[string]any, managed bool, instanceConnections int) error {
	id, err := hookID(remote)
	if err != nil {
		return err
	}
	remoteURL := hookURL(remote)
	legacyConnection, legacyRoute := legacyHookConnection(remoteURL)
	newConnection := hookURLConnection(remoteURL)
	if !legacyRoute && newConnection != connection.ID {
		return nil
	}
	if legacyRoute && legacyConnection != "" && legacyConnection != connection.LegacyID {
		return nil
	}
	if legacyRoute && legacyConnection == "" && instanceConnections > 1 {
		return fmt.Errorf("shared legacy Hook %d in %s scope %s is ambiguous across workspace connections", id, scopeType, scopePath)
	}
	if !legacyRoute && newConnection == "" {
		return nil
	}
	scopeID := strconv.FormatInt(actualScopeID, 10)
	if scopeType == "system" {
		scopeID = strconv.FormatInt(id, 10)
	}
	if strings.TrimSpace(r.env.PublicURL) == "" {
		return errors.New("MULTICA_PUBLIC_URL is required to inventory GitLab Hooks")
	}
	target, err := manualHookURL(r.env.PublicURL, connection.ID, scopeType, scopeID, id)
	if err != nil {
		return err
	}
	legacyID := int64(0)
	var newID pgtype.Int8
	previous := []byte(`{}`)
	previousSecret := ""
	state := "pending"
	if legacyRoute {
		legacyID = id
		previous, err = jsonSafeHookConfig(remote)
		if err != nil {
			return fmt.Errorf("snapshot GitLab Hook %d: %w", id, err)
		}
		previousSecret = connection.LegacyWebhookSecretEncrypted
		if previousSecret == "" {
			return fmt.Errorf("legacy webhook secret is unavailable for GitLab Hook %d", id)
		}
	} else {
		rows, queryErr := r.pool.Query(ctx, `
			SELECT legacy_hook_id FROM vcs_gitlab_migration_hook
			WHERE connection_id=$1 AND scope_type=$2 AND scope_id=$3
			  AND (legacy_hook_id=$4 OR new_hook_id=$4)
			ORDER BY CASE WHEN legacy_hook_id=$4 THEN 0 ELSE 1 END
			LIMIT 2
		`, connection.ID, scopeType, scopeID, id)
		if queryErr != nil {
			return fmt.Errorf("find existing GitLab Hook migration record: %w", queryErr)
		}
		var existing []int64
		for rows.Next() {
			var oldID int64
			if err := rows.Scan(&oldID); err != nil {
				rows.Close()
				return err
			}
			existing = append(existing, oldID)
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return err
		}
		rows.Close()
		if len(existing) > 1 {
			return fmt.Errorf("GitLab Hook %d matches multiple migration ledger records", id)
		}
		if len(existing) == 1 {
			legacyID = existing[0]
		} else if managed {
			return fmt.Errorf("managed GitLab Hook %d already uses the canonical route without a recoverable pre-switch snapshot", id)
		}
		newID = pgtype.Int8{Int64: id, Valid: true}
		state = "updated"
		if !hookURLMatches(remoteURL, target) {
			state = "pending"
		}
	}
	_, err = r.pool.Exec(ctx, `
		INSERT INTO vcs_gitlab_migration_hook (
			connection_id,workspace_id,scope_type,scope_id,scope_path,legacy_hook_id,new_hook_id,
			managed,previous_config,previous_secret_encrypted,target_url,state
		) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9::jsonb,$10,$11,$12)
		ON CONFLICT (connection_id,scope_type,scope_id,legacy_hook_id) DO UPDATE SET
			new_hook_id=COALESCE(EXCLUDED.new_hook_id,vcs_gitlab_migration_hook.new_hook_id),
			target_url=EXCLUDED.target_url,
			previous_config=CASE WHEN vcs_gitlab_migration_hook.previous_config='{}'::jsonb AND EXCLUDED.previous_config<>'{}'::jsonb THEN EXCLUDED.previous_config ELSE vcs_gitlab_migration_hook.previous_config END,
			previous_secret_encrypted=CASE WHEN vcs_gitlab_migration_hook.previous_secret_encrypted='' THEN EXCLUDED.previous_secret_encrypted ELSE vcs_gitlab_migration_hook.previous_secret_encrypted END,
			state=CASE WHEN vcs_gitlab_migration_hook.state='verified' THEN 'verified' ELSE EXCLUDED.state END,
			updated_at=now()
	`, connection.ID, connection.WorkspaceID, scopeType, scopeID, scopePath, legacyID, newID, managed, string(previous), previousSecret, target, state)
	if err != nil {
		return fmt.Errorf("record GitLab %s Hook %d in migration ledger: %w", scopeType, id, err)
	}
	return nil
}

func (r *Runner) loadMigrationConnections(ctx context.Context) (map[string]migrationConnection, error) {
	rows, err := r.pool.Query(ctx, `
		SELECT c.id::text,m.legacy_id::text,c.workspace_id::text,c.instance_url,
		       c.access_token_encrypted,c.webhook_secret_encrypted,
		       COALESCE(l.access_token_encrypted,''),COALESCE(l.webhook_secret_encrypted,'')
		FROM vcs_connection c
		JOIN vcs_gitlab_migration_id_map m ON m.workspace_id=c.workspace_id AND m.unified_id=c.id AND m.entity_kind='connection'
		LEFT JOIN gitlab_connection l ON l.workspace_id=m.workspace_id AND l.id=m.legacy_id
		WHERE c.provider='gitlab'
		ORDER BY c.workspace_id,c.id
	`)
	if err != nil {
		return nil, fmt.Errorf("list migrated GitLab connections: %w", err)
	}
	defer rows.Close()
	connections := make(map[string]migrationConnection)
	var encrypted []migrationConnection
	var legacyCredentialsByID = make(map[string][2]string)
	for rows.Next() {
		var connection migrationConnection
		var encryptedToken, encryptedSecret, oldTokenCipher, oldSecretCipher string
		if err := rows.Scan(&connection.ID, &connection.LegacyID, &connection.WorkspaceID, &connection.BaseURL,
			&encryptedToken, &encryptedSecret, &oldTokenCipher, &oldSecretCipher); err != nil {
			return nil, fmt.Errorf("read migrated GitLab connection: %w", err)
		}
		connection.Token = encryptedToken
		connection.WebhookSecret = encryptedSecret
		legacyCredentialsByID[connection.ID] = [2]string{oldTokenCipher, oldSecretCipher}
		encrypted = append(encrypted, connection)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(encrypted) == 0 {
		return connections, nil
	}
	box, err := r.env.box()
	if err != nil {
		return nil, err
	}
	for _, connection := range encrypted {
		token, err := openSecret(box, connection.Token)
		if err != nil {
			return nil, fmt.Errorf("decrypt GitLab API credential for %s: %w", safeInstanceURL(connection.BaseURL), err)
		}
		secret, err := openSecret(box, connection.WebhookSecret)
		if err != nil {
			return nil, fmt.Errorf("decrypt GitLab webhook credential for %s: %w", safeInstanceURL(connection.BaseURL), err)
		}
		oldCredentials := legacyCredentialsByID[connection.ID]
		oldToken, oldSecret, oldSecretCipher, err := r.env.legacyCredentials(box, connection.BaseURL, oldCredentials[0], oldCredentials[1])
		if err != nil {
			return nil, err
		}
		if oldToken != token {
			return nil, fmt.Errorf("legacy and canonical GitLab access tokens differ for %s", safeInstanceURL(connection.BaseURL))
		}
		connection.Token = token
		connection.WebhookSecret = secret
		connection.LegacyWebhookSecret = oldSecret
		connection.LegacyWebhookSecretEncrypted = oldSecretCipher
		connection.API, err = newGitLabAPI(connection.BaseURL, token, r.env.GitLabProxyURL)
		if err != nil {
			return nil, err
		}
		connections[connection.ID] = connection
	}
	return connections, nil
}

func (r *Runner) loadProjectScopes(ctx context.Context) ([]projectScope, error) {
	rows, err := r.pool.Query(ctx, `
		SELECT workspace_id::text,connection_id::text,gitlab_project_id,path_with_namespace,hook_id
		FROM vcs_gitlab_project ORDER BY workspace_id,connection_id,gitlab_project_id
	`)
	if err != nil {
		return nil, fmt.Errorf("list canonical GitLab projects: %w", err)
	}
	defer rows.Close()
	var projects []projectScope
	for rows.Next() {
		var project projectScope
		if err := rows.Scan(&project.WorkspaceID, &project.ConnectionID, &project.ProjectID, &project.Path, &project.HookID); err != nil {
			return nil, err
		}
		projects = append(projects, project)
	}
	return projects, rows.Err()
}

func (r *Runner) loadMigrationHooks(ctx context.Context) ([]migrationHook, error) {
	rows, err := r.pool.Query(ctx, `
		SELECT connection_id::text,workspace_id::text,scope_type,scope_id,scope_path,legacy_hook_id,new_hook_id,
		       managed,created_by_tool,previous_config::text,previous_secret_encrypted,target_url,state,COALESCE(last_error,'')
		FROM vcs_gitlab_migration_hook
		ORDER BY connection_id,scope_type,scope_id,legacy_hook_id
	`)
	if err != nil {
		return nil, fmt.Errorf("list GitLab Hook migration records: %w", err)
	}
	defer rows.Close()
	var hooks []migrationHook
	for rows.Next() {
		var hook migrationHook
		if err := rows.Scan(&hook.ConnectionID, &hook.WorkspaceID, &hook.ScopeType, &hook.ScopeID, &hook.ScopePath,
			&hook.LegacyID, &hook.NewID, &hook.Managed, &hook.CreatedByTool, &hook.Previous, &hook.PreviousSecretEncrypted, &hook.TargetURL, &hook.State, &hook.LastError); err != nil {
			return nil, err
		}
		hooks = append(hooks, hook)
	}
	return hooks, rows.Err()
}

func scopeNumericID(hook migrationHook) int64 {
	if hook.ScopeType == "system" {
		id, _ := strconv.ParseInt(hook.ScopeID, 10, 64)
		return id
	}
	id, _ := strconv.ParseInt(hook.ScopeID, 10, 64)
	return id
}

func hookURLMatches(raw, target string) bool {
	actual, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return false
	}
	wanted, err := url.Parse(strings.TrimSpace(target))
	if err != nil {
		return false
	}
	return actual.Scheme == wanted.Scheme && strings.EqualFold(actual.Host, wanted.Host) && actual.Path == wanted.Path && actual.RawPath == wanted.RawPath && actual.User == nil && actual.Fragment == "" && actual.Query().Encode() == wanted.Query().Encode()
}

func legacyHookConnection(raw string) (string, bool) {
	parsed, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return "", false
	}
	const prefix = "/api/webhooks/gitlab"
	path := strings.TrimRight(parsed.Path, "/")
	if path == prefix {
		return "", true
	}
	if strings.HasPrefix(path, prefix+"/") {
		id := strings.TrimPrefix(path, prefix+"/")
		if id != "" && !strings.Contains(id, "/") {
			return id, true
		}
	}
	return "", false
}

func hookURLConnection(raw string) string {
	parsed, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return ""
	}
	const prefix = "/api/webhooks/vcs/"
	path := strings.TrimPrefix(parsed.Path, prefix)
	if path == parsed.Path || path == "" || strings.Contains(path, "/") {
		return ""
	}
	return path
}

func hookIDMust(hook map[string]any) int64 {
	id, _ := hookID(hook)
	return id
}

func projectGroupPaths(projectPath string) []string {
	parts := strings.Split(strings.Trim(projectPath, "/"), "/")
	if len(parts) < 2 {
		return nil
	}
	paths := make([]string, len(parts)-1)
	for i := 1; i < len(parts); i++ {
		paths[i-1] = strings.Join(parts[:i], "/")
	}
	return paths
}

func isGitLabNotFound(err error) bool {
	return gitLabHTTPStatus(err) == 404
}
