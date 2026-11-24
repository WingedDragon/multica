package gitlabmigration

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// Replay replays persisted maintenance deliveries through the caller's real
// authoritative-sync path. Delivery payloads are never returned or printed.
func (r *Runner) Replay(ctx context.Context, since time.Time, replay func(context.Context, string) error) error {
	if replay == nil {
		return errors.New("GitLab receipt replay handler is required")
	}
	conn, err := r.pool.Acquire(ctx)
	if err != nil {
		return fmt.Errorf("acquire GitLab migration lock: %w", err)
	}
	defer conn.Release()
	if _, err := conn.Exec(ctx, "SELECT pg_advisory_lock($1)", migrationAdvisoryLock); err != nil {
		return fmt.Errorf("lock GitLab delivery replay: %w", err)
	}
	defer func() { _, _ = conn.Exec(context.Background(), "SELECT pg_advisory_unlock($1)", migrationAdvisoryLock) }()
	state, err := r.State(ctx)
	if err != nil {
		return err
	}
	if err := requireMaintenance(state); err != nil {
		return err
	}
	if state.Phase != "hooks_pending" && state.Phase != "verification_pending" && state.Phase != "ready" {
		return fmt.Errorf("cannot replay GitLab deliveries from phase %q", state.Phase)
	}

	rows, err := conn.Query(ctx, `
		SELECT delivery_id, min(received_at)
		FROM vcs_gitlab_migration_delivery
		WHERE received_at >= $1 AND payload IS NOT NULL AND reconciled_at IS NULL
		GROUP BY delivery_id
		ORDER BY min(received_at), delivery_id
	`, since)
	if err != nil {
		return fmt.Errorf("list pending GitLab delivery receipts: %w", err)
	}
	var deliveryIDs []string
	for rows.Next() {
		var id string
		var receivedAt time.Time
		if err := rows.Scan(&id, &receivedAt); err != nil {
			rows.Close()
			return fmt.Errorf("read pending GitLab delivery identity: %w", err)
		}
		deliveryIDs = append(deliveryIDs, id)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return fmt.Errorf("iterate pending GitLab delivery receipts: %w", err)
	}
	rows.Close()
	if state.Phase == "ready" && len(deliveryIDs) != 0 {
		tag, err := conn.Exec(ctx, `
			UPDATE vcs_gitlab_migration_state
			SET phase='verification_pending',updated_at=now(),last_error=NULL
			WHERE id=1 AND phase='ready' AND maintenance_mode
		`)
		if err != nil {
			return fmt.Errorf("reopen verification after late GitLab deliveries: %w", err)
		}
		if tag.RowsAffected() != 1 {
			return errors.New("GitLab migration phase changed while replay was preparing")
		}
	}
	for _, id := range deliveryIDs {
		if err := replay(ctx, id); err != nil {
			r.failMaintenance(ctx, fmt.Sprintf("GitLab delivery %s could not be reconciled", id))
			return fmt.Errorf("reconcile GitLab delivery %s: %w", id, err)
		}
		var remaining int64
		if err := conn.QueryRow(ctx, `
			SELECT count(*) FROM vcs_gitlab_migration_delivery
			WHERE delivery_id=$1 AND received_at >= $2
			  AND (reconciled_at IS NULL OR payload IS NULL)
		`, id, since).Scan(&remaining); err != nil {
			return fmt.Errorf("verify GitLab delivery reconciliation: %w", err)
		}
		if remaining != 0 {
			r.failMaintenance(ctx, "GitLab delivery replay returned before every receipt was reconciled")
			return fmt.Errorf("GitLab delivery %s has %d unreconciled receipt rows", id, remaining)
		}
	}
	return nil
}

// Finalize removes legacy GitLab tables only after database import, Hook
// verification, and every persisted receipt have been proven complete.
func (r *Runner) Finalize(ctx context.Context) error {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin GitLab finalization: %w", err)
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, "SELECT pg_advisory_xact_lock($1)", migrationAdvisoryLock); err != nil {
		return fmt.Errorf("lock GitLab finalization: %w", err)
	}
	state, err := readState(ctx, tx)
	if err != nil {
		return err
	}
	if state.Phase == "complete" && !state.MaintenanceMode {
		return tx.Commit(ctx)
	}
	if !r.env.VCSIntegrationEnabled {
		return errors.New("MULTICA_VCS_INTEGRATION_ENABLED=true is required to finalize GitLab cutover")
	}
	if state.Phase != "ready" || !state.MaintenanceMode {
		return fmt.Errorf("cannot finalize GitLab cutover from phase %q", state.Phase)
	}
	var pendingHooks, pendingDeliveries int64
	if err := tx.QueryRow(ctx, `
		SELECT count(*) FROM vcs_gitlab_migration_hook WHERE state <> 'verified'
	`).Scan(&pendingHooks); err != nil {
		return fmt.Errorf("check verified GitLab Hooks: %w", err)
	}
	if pendingHooks != 0 {
		return fmt.Errorf("GitLab finalization blocked by %d unverified Hooks", pendingHooks)
	}
	if err := tx.QueryRow(ctx, `
		SELECT count(*) FROM vcs_gitlab_migration_delivery
		WHERE reconciled_at IS NULL OR payload IS NULL
	`).Scan(&pendingDeliveries); err != nil {
		return fmt.Errorf("check reconciled GitLab delivery receipts: %w", err)
	}
	if pendingDeliveries != 0 {
		return fmt.Errorf("GitLab finalization blocked by %d unreconciled delivery receipts", pendingDeliveries)
	}
	if _, err := tx.Exec(ctx, `UPDATE vcs_gitlab_migration_delivery SET payload=NULL WHERE payload IS NOT NULL`); err != nil {
		return fmt.Errorf("clear reconciled GitLab receipt payloads after verification: %w", err)
	}
	if _, err := tx.Exec(ctx, `UPDATE vcs_gitlab_migration_hook SET previous_secret_encrypted='' WHERE previous_secret_encrypted<>''`); err != nil {
		return fmt.Errorf("clear legacy Hook secrets after rollback window: %w", err)
	}
	if err := dropLegacyGitLabSchema(ctx, tx); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `DELETE FROM vcs_gitlab_migration_snapshot`); err != nil {
		return fmt.Errorf("clear GitLab rollback snapshots after finalization: %w", err)
	}
	if _, err := tx.Exec(ctx, `
		UPDATE vcs_gitlab_migration_state
		SET phase='complete', maintenance_mode=false,
		    updated_at=now(), last_error=NULL
		WHERE id=1 AND phase='ready' AND maintenance_mode
	`); err != nil {
		return fmt.Errorf("record completed GitLab cutover: %w", err)
	}
	return tx.Commit(ctx)
}

func dropLegacyGitLabSchema(ctx context.Context, tx pgx.Tx) error {
	// Keep the nullable legacy ID columns for SQLC compatibility; clear their values
	// and remove references to the old core MR table without dropping the columns.
	statements := []string{
		`DROP TABLE issue_gitlab_merge_request`,
		`DROP TABLE gitlab_mr_pipeline`,
		`ALTER TABLE gitlab_mr_approval_state ALTER COLUMN vcs_pull_request_id SET NOT NULL`,
		`ALTER TABLE gitlab_mr_discussion ALTER COLUMN vcs_pull_request_id SET NOT NULL`,
		`ALTER TABLE gitlab_mr_note ALTER COLUMN vcs_pull_request_id SET NOT NULL`,
		`ALTER TABLE gitlab_pipeline_job ALTER COLUMN vcs_pull_request_id SET NOT NULL`,
		`UPDATE gitlab_mr_approval_state SET merge_request_id=NULL`,
		`UPDATE gitlab_mr_discussion SET merge_request_id=NULL`,
		`UPDATE gitlab_mr_note SET merge_request_id=NULL`,
		`UPDATE gitlab_pipeline_job SET merge_request_id=NULL`,
		`DO $$
		DECLARE legacy_fk RECORD;
		BEGIN
			FOR legacy_fk IN
				SELECT namespace.nspname AS schema_name, relation.relname AS table_name, fk.conname AS constraint_name
				FROM pg_constraint fk
				JOIN pg_class relation ON relation.oid=fk.conrelid
				JOIN pg_namespace namespace ON namespace.oid=relation.relnamespace
				WHERE fk.contype='f'
				  AND fk.confrelid=to_regclass(format('%I.%I',current_schema(),'gitlab_merge_request'))
			LOOP
				EXECUTE format('ALTER TABLE %I.%I DROP CONSTRAINT %I',
					legacy_fk.schema_name, legacy_fk.table_name, legacy_fk.constraint_name);
			END LOOP;
		END $$`,
		`ALTER TABLE gitlab_mr_approval_state ADD CONSTRAINT gitlab_mr_approval_state_pkey PRIMARY KEY USING INDEX gitlab_mr_approval_vcs_pr_uidx`,
		`DROP TABLE gitlab_merge_request`,
		`DROP TABLE gitlab_project_binding`,
		`DROP TABLE gitlab_connection`,
		`ALTER TABLE issue DROP CONSTRAINT IF EXISTS issue_workspace_id_id_key`,
	}
	for _, statement := range statements {
		if _, err := tx.Exec(ctx, statement); err != nil {
			return fmt.Errorf("drop legacy GitLab schema during finalization (%s): %w", statement, err)
		}
	}
	return nil
}

// Restore rolls back recorded remote Hook changes, then the transactionally
// snapshotted data migration. Maintenance remains enabled on every success and
// failure path until the operator explicitly reopens the legacy application.
func (r *Runner) Restore(ctx context.Context) error {
	state, err := r.State(ctx)
	if err != nil {
		return err
	}
	if state.Phase == "complete" {
		return errors.New("GitLab Hooks cannot be restored after finalization")
	}
	if state.Phase == "restored" && state.MaintenanceMode {
		return nil
	}
	if !state.MaintenanceMode {
		return errors.New("GitLab Hook restore requires maintenance mode")
	}
	if err := r.setPhase(ctx, []string{state.Phase}, "restoring", true, ""); err != nil {
		return err
	}
	if err := r.restoreHooks(ctx); err != nil {
		_ = r.setPhase(ctx, []string{"restoring"}, "restore_blocked", true, err.Error())
		return fmt.Errorf("GitLab Hook restore is blocked; keep both versions in maintenance: %w", err)
	}
	if err := r.backfillLegacyDetailIDs(ctx); err != nil {
		_ = r.setPhase(ctx, []string{"restoring"}, "restore_blocked", true, err.Error())
		return fmt.Errorf("legacy detail compatibility backfill is blocked; keep both versions in maintenance: %w", err)
	}
	if err := r.RestoreDatabase(ctx); err != nil {
		_ = r.setPhase(ctx, []string{"restoring"}, "restore_blocked", true, err.Error())
		return fmt.Errorf("GitLab database rollback is blocked; keep both versions in maintenance: %w", err)
	}
	return r.setPhase(ctx, []string{"restoring"}, "restored", true, "")
}
func (r *Runner) backfillLegacyDetailIDs(ctx context.Context) error {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin legacy detail compatibility backfill: %w", err)
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, "SELECT pg_advisory_xact_lock($1)", migrationAdvisoryLock); err != nil {
		return err
	}
	for _, table := range []string{"gitlab_mr_approval_state", "gitlab_mr_discussion", "gitlab_mr_note", "gitlab_pipeline_job"} {
		conflictCheck := fmt.Sprintf(`SELECT count(*)
			FROM %s d
			JOIN vcs_gitlab_migration_id_map m
			  ON m.workspace_id=d.workspace_id
			 AND m.entity_kind='pull_request'
			 AND m.unified_id=d.vcs_pull_request_id
			WHERE d.merge_request_id IS NOT NULL
			  AND d.merge_request_id IS DISTINCT FROM m.legacy_id`, table)
		var conflicts int64
		if err := tx.QueryRow(ctx, conflictCheck).Scan(&conflicts); err != nil {
			return fmt.Errorf("check legacy identity conflicts for %s: %w", table, err)
		}
		if conflicts != 0 {
			return fmt.Errorf("%s has %d detail rows whose legacy MR ID conflicts with the workspace-scoped VCS mapping", table, conflicts)
		}
		update := fmt.Sprintf(`UPDATE %s d SET merge_request_id=m.legacy_id
			FROM vcs_gitlab_migration_id_map m
			WHERE d.merge_request_id IS NULL
			  AND m.workspace_id=d.workspace_id
			  AND m.entity_kind='pull_request'
			  AND m.unified_id=d.vcs_pull_request_id`, table)
		if _, err := tx.Exec(ctx, update); err != nil {
			return fmt.Errorf("restore legacy MR identities in %s: %w", table, err)
		}
		verify := fmt.Sprintf(`SELECT count(*)
			FROM %s d
			JOIN vcs_gitlab_migration_id_map m
			  ON m.workspace_id=d.workspace_id
			 AND m.entity_kind='pull_request'
			 AND m.unified_id=d.vcs_pull_request_id
			WHERE d.merge_request_id IS DISTINCT FROM m.legacy_id`, table)
		var remaining int64
		if err := tx.QueryRow(ctx, verify).Scan(&remaining); err != nil {
			return fmt.Errorf("verify legacy identity backfill for %s: %w", table, err)
		}
		if remaining != 0 {
			return fmt.Errorf("%s has %d mapped detail rows without their expected legacy MR identity", table, remaining)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit legacy detail compatibility backfill: %w", err)
	}
	return nil
}
