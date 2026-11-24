package gitlabmigration

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

func TestCutoverSchemaSeedsMaintenanceAndKeepsLegacyTables(t *testing.T) {
	dbURL := os.Getenv("DATABASE_URL")
	if dbURL == "" {
		t.Skip("integration test requires Postgres at DATABASE_URL")
	}
	migration, err := os.ReadFile(filepath.Join("..", "..", "migrations", "745_gitlab_vcs_cutover.up.sql"))
	if err != nil {
		t.Fatalf("read additive migration: %v", err)
	}
	for _, legacyRows := range []bool{false, true} {
		name := "fresh"
		if legacyRows {
			name = "legacy"
		}
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			schema := fmt.Sprintf("gitlab_cutover_%d", time.Now().UnixNano())
			admin, err := pgxpool.New(ctx, dbURL)
			if err != nil {
				t.Fatalf("connect admin pool: %v", err)
			}
			if _, err := admin.Exec(ctx, "CREATE SCHEMA "+schema); err != nil {
				admin.Close()
				t.Fatalf("create sandbox schema: %v", err)
			}
			cfg, err := pgxpool.ParseConfig(dbURL)
			if err != nil {
				t.Fatalf("parse database URL: %v", err)
			}
			cfg.ConnConfig.RuntimeParams["search_path"] = schema
			pool, err := pgxpool.NewWithConfig(ctx, cfg)
			if err != nil {
				_, _ = admin.Exec(ctx, "DROP SCHEMA "+schema+" CASCADE")
				admin.Close()
				t.Fatalf("connect sandbox pool: %v", err)
			}
			t.Cleanup(func() {
				pool.Close()
				_, _ = admin.Exec(context.Background(), "DROP SCHEMA IF EXISTS "+schema+" CASCADE")
				admin.Close()
			})
			if _, err := pool.Exec(ctx, `
				CREATE TABLE workspace (id UUID PRIMARY KEY);
				CREATE TABLE issue (id UUID PRIMARY KEY, workspace_id UUID NOT NULL);
				CREATE TABLE gitlab_connection (id UUID PRIMARY KEY);
				CREATE TABLE gitlab_project_binding (id UUID PRIMARY KEY);
				CREATE TABLE gitlab_merge_request (id UUID PRIMARY KEY);
				CREATE TABLE gitlab_mr_pipeline (id UUID PRIMARY KEY);
				CREATE TABLE issue_gitlab_merge_request (issue_id UUID NOT NULL);
				CREATE TABLE chat_wakeup (status TEXT NOT NULL, condition JSONB);
				CREATE TABLE issue_wakeup (issue_id UUID NOT NULL, enabled BOOLEAN NOT NULL, condition JSONB);
				CREATE TABLE issue_vcs_pull_request (issue_id UUID NOT NULL, pull_request_id UUID NOT NULL, PRIMARY KEY (issue_id, pull_request_id));
				CREATE TABLE gitlab_mr_approval_state (merge_request_id UUID PRIMARY KEY);
				CREATE TABLE gitlab_mr_discussion (id UUID PRIMARY KEY, merge_request_id UUID NOT NULL, gitlab_discussion_id TEXT NOT NULL);
				CREATE TABLE gitlab_mr_note (id UUID PRIMARY KEY, merge_request_id UUID NOT NULL, gitlab_note_id BIGINT NOT NULL);
				CREATE TABLE gitlab_pipeline_job (id UUID PRIMARY KEY, merge_request_id UUID NOT NULL, pipeline_id BIGINT NOT NULL, job_id BIGINT NOT NULL);
				CREATE TABLE vcs_connection (provider TEXT NOT NULL);
				CREATE TABLE vcs_pull_request (provider TEXT NOT NULL);
			`); err != nil {
				t.Fatalf("create legacy schema fixture: %v", err)
			}
			if legacyRows {
				if _, err := pool.Exec(ctx, `INSERT INTO gitlab_connection VALUES ('10000000-0000-0000-0000-000000000001')`); err != nil {
					t.Fatalf("seed legacy connection: %v", err)
				}
			}
			if _, err := pool.Exec(ctx, string(migration)); err != nil {
				t.Fatalf("apply ordinary additive migration: %v", err)
			}
			var earlyUniqueIndexes int
			if err := pool.QueryRow(ctx, `
				SELECT count(*)
				FROM pg_index index_data
				JOIN pg_class table_data ON table_data.oid=index_data.indrelid
				WHERE table_data.relnamespace=current_schema()::regnamespace
				  AND table_data.relname IN (
					'vcs_gitlab_project','vcs_gitlab_migration_id_map',
					'vcs_gitlab_migration_state','vcs_gitlab_migration_snapshot',
					'vcs_gitlab_migration_hook','vcs_gitlab_migration_delivery',
					'vcs_gitlab_mr_detail'
				  )
				  AND index_data.indisunique
			`).Scan(&earlyUniqueIndexes); err != nil {
				t.Fatalf("inspect new-table indexes before concurrent builds: %v", err)
			}
			if earlyUniqueIndexes != 0 {
				t.Fatalf("ordinary migration created %d implicit unique indexes before concurrent builds", earlyUniqueIndexes)
			}
			if _, err := pool.Exec(ctx, string(migration)); err != nil {
				t.Fatalf("reapply additive migration: %v", err)
			}
			applyGitLabCutoverFollowupMigrations(t, ctx, pool)
			var keyConstraints int
			if err := pool.QueryRow(ctx, `
				SELECT count(*)
				FROM pg_constraint constraint_data
				JOIN pg_class table_data ON table_data.oid=constraint_data.conrelid
				WHERE table_data.relnamespace=current_schema()::regnamespace
				  AND table_data.relname IN (
					'vcs_gitlab_project','vcs_gitlab_migration_id_map',
					'vcs_gitlab_migration_state','vcs_gitlab_migration_snapshot',
					'vcs_gitlab_migration_hook','vcs_gitlab_migration_delivery',
					'vcs_gitlab_mr_detail','vcs_gitlab_migration_system_hook_attestation'
				  )
				  AND constraint_data.contype IN ('p','u')
			`).Scan(&keyConstraints); err != nil {
				t.Fatalf("inspect restored key constraints: %v", err)
			}
			if keyConstraints != 10 {
				t.Fatalf("restored primary/unique constraints = %d, want 10", keyConstraints)
			}
			if legacyRows {
				report, err := NewRunner(pool, Environment{}).Preflight(ctx)
				if err == nil || len(report.Findings) != 1 || report.Findings[0].Code != "vcs_integration_disabled" {
					t.Fatalf("preflight with legacy rows and disabled VCS = (%#v, %v), want the VCS feature-gate blocker", report, err)
				}
				if err := NewRunner(pool, Environment{}).Begin(ctx); err == nil {
					t.Fatal("begin accepted legacy cutover while VCS integration was disabled")
				}
			}
			var phase string
			var maintenance bool
			if err := pool.QueryRow(ctx, `SELECT phase,maintenance_mode FROM vcs_gitlab_migration_state WHERE id=1`).Scan(&phase, &maintenance); err != nil {
				t.Fatalf("read initialized gate: %v", err)
			}
			wantPhase, wantMaintenance := "complete", false
			if legacyRows {
				wantPhase, wantMaintenance = "maintenance", true
			}
			if phase != wantPhase || maintenance != wantMaintenance {
				t.Fatalf("initial state = (%q,%v), want (%q,%v)", phase, maintenance, wantPhase, wantMaintenance)
			}
			if !legacyRows {
				runner := NewRunner(pool, Environment{})
				if err := runner.Migrate(ctx); err != nil {
					t.Fatalf("fresh install migration unexpectedly requires production credentials: %v", err)
				}
				if err := runner.Migrate(ctx); err != nil {
					t.Fatalf("re-enter fresh install migration: %v", err)
				}
				if _, err := pool.Exec(ctx, `INSERT INTO gitlab_mr_pipeline VALUES ('30000000-0000-0000-0000-000000000001')`); err != nil {
					t.Fatalf("seed legacy pipeline after completion: %v", err)
				}
				report, err := runner.Preflight(ctx)
				if err == nil || report.NoOp || report.LegacyPipelines != 1 || len(report.Findings) != 1 || report.Findings[0].Code != "legacy_data_after_complete" {
					t.Fatalf("preflight with pipeline data after complete = (%#v, %v), want a counted fail-closed blocker", report, err)
				}
				if _, err := pool.Exec(ctx, `DELETE FROM gitlab_mr_pipeline`); err != nil {
					t.Fatalf("remove legacy pipeline fixture: %v", err)
				}
				if _, err := pool.Exec(ctx, `INSERT INTO gitlab_connection VALUES ('10000000-0000-0000-0000-000000000001')`); err != nil {
					t.Fatalf("seed legacy row after completion: %v", err)
				}
				report, err = runner.Preflight(ctx)
				if err == nil || len(report.Findings) != 1 || report.Findings[0].Code != "legacy_data_after_complete" {
					t.Fatalf("preflight with legacy data after complete = (%#v, %v), want fail-closed corruption", report, err)
				}
			} else {
				runner := NewRunner(pool, Environment{VCSIntegrationEnabled: true})
				if err := runner.Begin(ctx); err != nil {
					t.Fatalf("begin cutover attempt: %v", err)
				}
				state, err := runner.State(ctx)
				if err != nil {
					t.Fatal(err)
				}
				if state.Generation != 1 || state.Phase != "maintenance" {
					t.Fatalf("first attempt state = %#v, want generation 1 maintenance", state)
				}
				if err := runner.Begin(ctx); err != nil {
					t.Fatalf("repeat begin: %v", err)
				}
				if err := runner.setPhase(ctx, []string{"maintenance"}, "database_migrated", true, ""); err != nil {
					t.Fatalf("advance phase: %v", err)
				}
				state, err = runner.State(ctx)
				if err != nil {
					t.Fatal(err)
				}
				if state.Generation != 1 || state.Phase != "database_migrated" {
					t.Fatalf("phase transition changed attempt identity: %#v", state)
				}
				if err := runner.setPhase(ctx, []string{"database_migrated"}, "restored", true, ""); err != nil {
					t.Fatalf("record successful restore: %v", err)
				}
				if err := runner.Begin(ctx); err != nil {
					t.Fatalf("begin retry attempt: %v", err)
				}
				state, err = runner.State(ctx)
				if err != nil {
					t.Fatal(err)
				}
				if state.Generation != 2 || state.Phase != "maintenance" {
					t.Fatalf("retry attempt state = %#v, want generation 2 maintenance", state)
				}
			}
			if _, err := pool.Exec(ctx, `
				INSERT INTO vcs_gitlab_migration_state (id,phase,maintenance_mode)
				VALUES (1,'complete',FALSE) ON CONFLICT (id) DO NOTHING
			`); err != nil {
				t.Fatalf("exercise seeded-state ON CONFLICT target: %v", err)
			}
			if _, err := pool.Exec(ctx, `
				INSERT INTO vcs_gitlab_project (
					id,workspace_id,connection_id,gitlab_project_id,path_with_namespace,web_url
				)
				VALUES
					('20000000-0000-0000-0000-000000000001','30000000-0000-0000-0000-000000000001','40000000-0000-0000-0000-000000000001',77,'group/project','https://gitlab.example.test/group/project'),
					('20000000-0000-0000-0000-000000000002','30000000-0000-0000-0000-000000000001','40000000-0000-0000-0000-000000000001',77,'group/project','https://gitlab.example.test/group/project')
				ON CONFLICT (workspace_id,connection_id,gitlab_project_id) DO NOTHING
			`); err != nil {
				t.Fatalf("exercise project scope ON CONFLICT target: %v", err)
			}
			var projects int
			if err := pool.QueryRow(ctx, `
				SELECT count(*) FROM vcs_gitlab_project
				WHERE workspace_id='30000000-0000-0000-0000-000000000001'
				  AND connection_id='40000000-0000-0000-0000-000000000001'
				  AND gitlab_project_id=77
			`).Scan(&projects); err != nil {
				t.Fatalf("count project scope conflict rows: %v", err)
			}
			if projects != 1 {
				t.Fatalf("project scope conflict left %d rows, want 1", projects)
			}
			if _, err := pool.Exec(ctx, `
				INSERT INTO vcs_gitlab_migration_hook (
					connection_id,workspace_id,scope_type,scope_id,scope_path,legacy_hook_id,
					managed,previous_secret_encrypted,state
				) VALUES (
					'40000000-0000-0000-0000-000000000010','30000000-0000-0000-0000-000000000001',
					'project','77','group/project',0,TRUE,'','pending'
				)
				ON CONFLICT (connection_id,scope_type,scope_id,legacy_hook_id) DO UPDATE
				SET previous_secret_encrypted=EXCLUDED.previous_secret_encrypted
			`); err != nil {
				t.Fatalf("exercise managed Hook ON CONFLICT target: %v", err)
			}
			if _, err := pool.Exec(ctx, `
				INSERT INTO vcs_gitlab_migration_hook (
					connection_id,workspace_id,scope_type,scope_id,scope_path,legacy_hook_id,
					managed,previous_secret_encrypted,state
				) VALUES (
					'40000000-0000-0000-0000-000000000010','30000000-0000-0000-0000-000000000001',
					'project','77','group/project',0,TRUE,'','pending'
				)
				ON CONFLICT (connection_id,scope_type,scope_id,legacy_hook_id) DO UPDATE
				SET previous_secret_encrypted=EXCLUDED.previous_secret_encrypted
			`); err != nil {
				t.Fatalf("exercise managed Hook conflict update: %v", err)
			}
			var hooks int
			if err := pool.QueryRow(ctx, `
				SELECT count(*) FROM vcs_gitlab_migration_hook
				WHERE connection_id='40000000-0000-0000-0000-000000000010'
				  AND scope_type='project' AND scope_id='77' AND legacy_hook_id=0
			`).Scan(&hooks); err != nil {
				t.Fatalf("count managed Hook conflict rows: %v", err)
			}
			if hooks != 1 {
				t.Fatalf("managed Hook conflict left %d rows, want 1", hooks)
			}
			var legacyTablePresent bool
			if err := pool.QueryRow(ctx, `SELECT to_regclass('gitlab_connection') IS NOT NULL`).Scan(&legacyTablePresent); err != nil {
				t.Fatalf("check legacy table: %v", err)
			}
			if !legacyTablePresent {
				t.Fatal("ordinary migration dropped gitlab_connection before the explicit cutover tool")
			}
		})
	}
}
func applyGitLabCutoverFollowupMigrations(t *testing.T, ctx context.Context, pool *pgxpool.Pool) {
	t.Helper()
	for _, name := range []string{
		"746_vcs_gitlab_migration_delivery_scope_idx",
		"747_gitlab_mr_approval_vcs_pr_idx",
		"748_gitlab_mr_discussion_vcs_pr_idx",
		"749_gitlab_mr_note_vcs_pr_idx",
		"750_gitlab_pipeline_job_vcs_pr_idx",
		"751_gitlab_pipeline_job_vcs_pr_pipeline_idx",
		"752_gitlab_mr_discussion_vcs_pr_key",
		"753_gitlab_mr_note_vcs_pr_key",
		"754_gitlab_pipeline_job_vcs_pr_key",
		"755_gitlab_mr_approval_legacy_id_uidx",
		"756_gitlab_mr_detail_nullable",
		"757_gitlab_system_hook_inventory_attestation",
		"758_vcs_gitlab_project_pkey",
		"759_vcs_gitlab_project_scope_key",
		"760_vcs_gitlab_migration_id_map_pkey",
		"761_vcs_gitlab_migration_id_map_unified_key",
		"762_vcs_gitlab_migration_state_pkey",
		"763_vcs_gitlab_migration_snapshot_pkey",
		"764_vcs_gitlab_migration_hook_pkey",
		"765_vcs_gitlab_migration_delivery_pkey",
		"766_vcs_gitlab_mr_detail_pkey",
		"767_vcs_gitlab_system_hook_attestation_pkey",
		"768_gitlab_vcs_cutover_constraints",
	} {
		path := filepath.Join("..", "..", "migrations", name+".up.sql")
		content, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read cutover migration %s: %v", name, err)
		}
		if _, err := pool.Exec(ctx, string(content)); err != nil {
			t.Fatalf("apply cutover migration %s: %v", name, err)
		}
		if name == "757_gitlab_system_hook_inventory_attestation" {
			var implicitAttestationIndexes int
			if err := pool.QueryRow(ctx, `
				SELECT count(*)
				FROM pg_index index_data
				JOIN pg_class table_data ON table_data.oid=index_data.indrelid
				WHERE table_data.relnamespace=current_schema()::regnamespace
				  AND table_data.relname='vcs_gitlab_migration_system_hook_attestation'
				  AND index_data.indisunique
			`).Scan(&implicitAttestationIndexes); err != nil {
				t.Fatalf("inspect system hook attestation indexes before concurrent build: %v", err)
			}
			if implicitAttestationIndexes != 0 {
				t.Fatalf("system hook migration created %d implicit unique indexes", implicitAttestationIndexes)
			}
		}
	}
}

func TestPreflightRejectsPartiallyDroppedLegacySchema(t *testing.T) {
	ctx, pool := newIsolatedMigrationTestPool(t)
	if _, err := pool.Exec(ctx, `
		CREATE TABLE gitlab_connection (id UUID PRIMARY KEY);
		CREATE TABLE gitlab_project_binding (id UUID PRIMARY KEY);
		CREATE TABLE gitlab_merge_request (id UUID PRIMARY KEY);
		CREATE TABLE issue_gitlab_merge_request (issue_id UUID NOT NULL)
	`); err != nil {
		t.Fatalf("create partial legacy schema fixture: %v", err)
	}
	report, err := NewRunner(pool, Environment{}).Preflight(ctx)
	if err == nil || len(report.Findings) != 1 || report.Findings[0].Code != "legacy_schema_partial" {
		t.Fatalf("preflight with partially dropped legacy schema = (%#v, %v), want corruption blocker", report, err)
	}
}

func TestPreflightNoOpsBeforeAnyGitLabSchemaMigration(t *testing.T) {
	ctx, pool := newIsolatedMigrationTestPool(t)
	report, err := NewRunner(pool, Environment{}).Preflight(ctx)
	if err != nil || !report.NoOp || report.State != "not_initialized" {
		t.Fatalf("preflight on a clean unmigrated schema = (%#v, %v), want not_initialized no-op", report, err)
	}
}

func TestPreflightRejectsMissingLegacySchemaAfterMigrationHistory(t *testing.T) {
	ctx, pool := newIsolatedMigrationTestPool(t)
	if _, err := pool.Exec(ctx, `
		CREATE TABLE schema_migrations (version TEXT PRIMARY KEY, applied_at TIMESTAMPTZ NOT NULL DEFAULT now());
		INSERT INTO schema_migrations (version) VALUES ('716_migration')
	`); err != nil {
		t.Fatalf("create legacy migration history: %v", err)
	}
	report, err := NewRunner(pool, Environment{}).Preflight(ctx)
	if err == nil || len(report.Findings) != 1 || report.Findings[0].Code != "legacy_schema_absent" {
		t.Fatalf("preflight with completed GitLab migration history but no legacy relations = (%#v, %v), want missing-schema blocker", report, err)
	}
}

func TestPreflightRunsAgainstLegacySchemaBeforeCutoverMigration(t *testing.T) {
	ctx, pool := newIsolatedMigrationTestPool(t)
	createLegacyPreflightSchema(t, ctx, pool)
	if _, err := pool.Exec(ctx, `
		INSERT INTO gitlab_connection (id,workspace_id,base_url,access_token_encrypted,webhook_secret_encrypted)
		VALUES ('10000000-0000-0000-0000-000000000001','20000000-0000-0000-0000-000000000001',
		        'https://gitlab.example.com','','')
	`); err != nil {
		t.Fatalf("seed legacy connection: %v", err)
	}
	runner := NewRunner(pool, Environment{})
	report, err := runner.Preflight(ctx)
	if err == nil || len(report.Findings) != 1 || report.Findings[0].Code != "vcs_integration_disabled" {
		t.Fatalf("pre-migration preflight = (%#v, %v), want feature-gate blocker after reading old schema", report, err)
	}
	if report.State != "not_initialized" || report.NoOp || report.LegacyConnections != 1 {
		t.Fatalf("pre-migration report = %#v, want uninitialized state and one counted legacy connection", report)
	}
	if _, err := runner.State(ctx); err == nil {
		t.Fatal("strict state/status read unexpectedly succeeded before migration state was created")
	}
}

func TestPreflightRejectsPartialCutoverSchemaWithoutState(t *testing.T) {
	ctx, pool := newIsolatedMigrationTestPool(t)
	if _, err := pool.Exec(ctx, `CREATE TABLE vcs_gitlab_migration_delivery (delivery_id TEXT, payload JSONB)`); err != nil {
		t.Fatalf("create partial cutover marker: %v", err)
	}
	report, err := NewRunner(pool, Environment{}).Preflight(ctx)
	if err == nil || len(report.Findings) != 1 || report.Findings[0].Code != "cutover_schema_partial" {
		t.Fatalf("preflight with cutover artifact but no state = (%#v, %v), want partial-schema blocker", report, err)
	}
}

func TestPreflightRejectsIncompleteCutoverSchemaWithState(t *testing.T) {
	ctx, pool := newIsolatedMigrationTestPool(t)
	if _, err := pool.Exec(ctx, `CREATE TABLE vcs_gitlab_migration_state (id SMALLINT PRIMARY KEY)`); err != nil {
		t.Fatalf("create incomplete cutover state table: %v", err)
	}
	report, err := NewRunner(pool, Environment{}).Preflight(ctx)
	if err == nil || len(report.Findings) != 1 || report.Findings[0].Code != "cutover_schema_partial" {
		t.Fatalf("preflight with incomplete cutover schema = (%#v, %v), want partial-schema blocker", report, err)
	}
}

func TestPreflightReportsMissingLegacyDependenciesBeforeCounting(t *testing.T) {
	ctx, pool := newIsolatedMigrationTestPool(t)
	createLegacyPreflightSchema(t, ctx, pool)
	if _, err := pool.Exec(ctx, `DROP TABLE chat_wakeup`); err != nil {
		t.Fatalf("remove required legacy dependency: %v", err)
	}
	report, err := NewRunner(pool, Environment{}).Preflight(ctx)
	if err == nil || len(report.Findings) != 1 || report.Findings[0].Code != "legacy_schema_dependency_missing" || report.Findings[0].Resource != "chat_wakeup" {
		t.Fatalf("preflight with missing legacy dependency = (%#v, %v), want a named schema blocker", report, err)
	}
}

func createLegacyPreflightSchema(t *testing.T, ctx context.Context, pool *pgxpool.Pool) {
	t.Helper()
	if _, err := pool.Exec(ctx, `
		CREATE TABLE workspace (id UUID PRIMARY KEY);
		CREATE TABLE issue (id UUID PRIMARY KEY, workspace_id UUID NOT NULL);
		CREATE TABLE gitlab_connection (
			id UUID PRIMARY KEY, workspace_id UUID NOT NULL, base_url TEXT NOT NULL,
			access_token_encrypted TEXT NOT NULL, webhook_secret_encrypted TEXT NOT NULL
		);
		CREATE TABLE gitlab_project_binding (
			id UUID PRIMARY KEY, workspace_id UUID NOT NULL, connection_id UUID NOT NULL,
			gitlab_project_id BIGINT NOT NULL, path_with_namespace TEXT NOT NULL, hook_id BIGINT
		);
		CREATE TABLE gitlab_merge_request (
			id UUID PRIMARY KEY, workspace_id UUID NOT NULL, project_binding_id UUID NOT NULL,
			gitlab_project_id BIGINT NOT NULL, mr_iid INTEGER NOT NULL
		);
		CREATE TABLE gitlab_mr_pipeline (merge_request_id UUID NOT NULL, pipeline_id BIGINT NOT NULL);
		CREATE TABLE issue_gitlab_merge_request (workspace_id UUID NOT NULL, issue_id UUID NOT NULL, merge_request_id UUID NOT NULL);
		CREATE TABLE gitlab_mr_approval_state (workspace_id UUID NOT NULL, merge_request_id UUID NOT NULL);
		CREATE TABLE gitlab_mr_discussion (id UUID PRIMARY KEY, workspace_id UUID NOT NULL, merge_request_id UUID NOT NULL);
		CREATE TABLE gitlab_mr_note (id UUID PRIMARY KEY, workspace_id UUID NOT NULL, merge_request_id UUID NOT NULL);
		CREATE TABLE gitlab_pipeline_job (id UUID PRIMARY KEY, workspace_id UUID NOT NULL, merge_request_id UUID NOT NULL);
		CREATE TABLE issue_wakeup (issue_id UUID NOT NULL, enabled BOOLEAN NOT NULL, condition JSONB);
		CREATE TABLE chat_wakeup (status TEXT NOT NULL, condition JSONB);
		CREATE TABLE issue_vcs_pull_request (
			issue_id UUID NOT NULL, pull_request_id UUID NOT NULL, close_intent BOOLEAN NOT NULL,
			reference_only BOOLEAN NOT NULL, linked_by_type TEXT, linked_by_id UUID, linked_at TIMESTAMPTZ
		);
		CREATE TABLE vcs_connection (
			id UUID PRIMARY KEY, workspace_id UUID NOT NULL, provider TEXT NOT NULL, instance_url TEXT NOT NULL,
			access_token_encrypted TEXT NOT NULL, webhook_secret_encrypted TEXT NOT NULL
		);
		CREATE TABLE vcs_pull_request (
			id UUID PRIMARY KEY, workspace_id UUID NOT NULL, connection_id UUID NOT NULL,
			provider TEXT NOT NULL, pr_number INTEGER NOT NULL
		);
	`); err != nil {
		t.Fatalf("create pre-745 legacy schema: %v", err)
	}
}

func TestCanonicalDetailMigrationPreservesLegacyConflictTargets(t *testing.T) {
	ctx, pool := newIsolatedMigrationTestPool(t)
	if _, err := pool.Exec(ctx, `
		CREATE TABLE gitlab_connection (id UUID PRIMARY KEY);
		CREATE TABLE gitlab_project_binding (id UUID PRIMARY KEY);
		CREATE TABLE gitlab_merge_request (id UUID PRIMARY KEY);
		CREATE TABLE issue_gitlab_merge_request (issue_id UUID NOT NULL);
		CREATE TABLE chat_wakeup (status TEXT NOT NULL, condition JSONB);
		CREATE TABLE issue_vcs_pull_request (issue_id UUID NOT NULL, pull_request_id UUID NOT NULL, PRIMARY KEY (issue_id, pull_request_id));
		CREATE TABLE gitlab_mr_approval_state (merge_request_id UUID PRIMARY KEY);
		CREATE TABLE gitlab_mr_discussion (
			id UUID PRIMARY KEY,
			merge_request_id UUID NOT NULL,
			gitlab_discussion_id TEXT NOT NULL,
			UNIQUE (merge_request_id, gitlab_discussion_id)
		);
		CREATE TABLE gitlab_mr_note (
			id UUID PRIMARY KEY,
			merge_request_id UUID NOT NULL,
			gitlab_note_id BIGINT NOT NULL,
			UNIQUE (merge_request_id, gitlab_note_id)
		);
		CREATE TABLE gitlab_pipeline_job (
			id UUID PRIMARY KEY,
			merge_request_id UUID NOT NULL,
			job_id BIGINT NOT NULL,
			UNIQUE (merge_request_id, job_id)
		);
		CREATE TABLE vcs_connection (provider TEXT NOT NULL);
		CREATE TABLE vcs_pull_request (provider TEXT NOT NULL);
	`); err != nil {
		t.Fatalf("create legacy detail schema: %v", err)
	}
	readMigration := func(name string) string {
		t.Helper()
		content, err := os.ReadFile(filepath.Join("..", "..", "migrations", name))
		if err != nil {
			t.Fatalf("read migration %s: %v", name, err)
		}
		return string(content)
	}
	if _, err := pool.Exec(ctx, readMigration("745_gitlab_vcs_cutover.up.sql")); err != nil {
		t.Fatalf("apply additive cutover schema: %v", err)
	}
	var oldNotNullColumns int
	var oldApprovalPrimaryKey bool
	if err := pool.QueryRow(ctx, `
		SELECT
			(SELECT count(*) FROM pg_attribute
			 WHERE attname='merge_request_id' AND attnotnull
			   AND attrelid IN (
			     'gitlab_mr_approval_state'::regclass,
			     'gitlab_mr_discussion'::regclass,
			     'gitlab_mr_note'::regclass,
			     'gitlab_pipeline_job'::regclass
			   )),
			EXISTS (SELECT 1 FROM pg_constraint
			        WHERE conrelid='gitlab_mr_approval_state'::regclass
			          AND conname='gitlab_mr_approval_state_pkey' AND contype='p')
	`).Scan(&oldNotNullColumns, &oldApprovalPrimaryKey); err != nil {
		t.Fatalf("inspect constraints after additive migration: %v", err)
	}
	if oldNotNullColumns != 4 || !oldApprovalPrimaryKey {
		t.Fatalf("additive migration changed legacy constraints: NOT NULL count=%d approval PK=%v", oldNotNullColumns, oldApprovalPrimaryKey)
	}
	if _, err := pool.Exec(ctx, readMigration("755_gitlab_mr_approval_legacy_id_uidx.up.sql")); err != nil {
		t.Fatalf("create concurrent legacy approval index: %v", err)
	}
	if _, err := pool.Exec(ctx, readMigration("756_gitlab_mr_detail_nullable.up.sql")); err != nil {
		t.Fatalf("allow canonical-only detail rows: %v", err)
	}
	if err := pool.QueryRow(ctx, `
		SELECT count(*) FROM pg_attribute
		WHERE attname='merge_request_id' AND attnotnull
		  AND attrelid IN (
		    'gitlab_mr_approval_state'::regclass,
		    'gitlab_mr_discussion'::regclass,
		    'gitlab_mr_note'::regclass,
		    'gitlab_pipeline_job'::regclass
		  )
	`).Scan(&oldNotNullColumns); err != nil {
		t.Fatalf("inspect nullable legacy IDs: %v", err)
	}
	if oldNotNullColumns != 0 {
		t.Fatalf("nullable detail migration left %d legacy ID columns NOT NULL", oldNotNullColumns)
	}
	approvalID := "20000000-0000-0000-0000-000000000001"
	canonicalID := "30000000-0000-0000-0000-000000000001"
	insert := func(label, query string, args ...any) int64 {
		t.Helper()
		tag, err := pool.Exec(ctx, query, args...)
		if err != nil {
			t.Fatalf("exercise %s conflict target: %v", label, err)
		}
		return tag.RowsAffected()
	}
	if rows := insert("approval legacy", `INSERT INTO gitlab_mr_approval_state (merge_request_id,vcs_pull_request_id) VALUES ($1,$2) ON CONFLICT (merge_request_id) DO NOTHING`, approvalID, canonicalID); rows != 1 {
		t.Fatalf("first approval insert affected %d rows, want 1", rows)
	}
	if rows := insert("approval legacy duplicate", `INSERT INTO gitlab_mr_approval_state (merge_request_id,vcs_pull_request_id) VALUES ($1,$2) ON CONFLICT (merge_request_id) DO NOTHING`, approvalID, "30000000-0000-0000-0000-000000000002"); rows != 0 {
		t.Fatalf("legacy approval conflict affected %d rows, want 0", rows)
	}
	if rows := insert("approval canonical-only", `INSERT INTO gitlab_mr_approval_state (merge_request_id,vcs_pull_request_id) VALUES (NULL,$1) ON CONFLICT (merge_request_id) DO NOTHING`, "30000000-0000-0000-0000-000000000003"); rows != 1 {
		t.Fatalf("canonical-only approval insert affected %d rows, want 1", rows)
	}
	if rows := insert("discussion canonical-only", `INSERT INTO gitlab_mr_discussion (id,merge_request_id,gitlab_discussion_id,vcs_pull_request_id) VALUES ($1,NULL,$2,$3) ON CONFLICT (merge_request_id,gitlab_discussion_id) DO NOTHING`, "50000000-0000-0000-0000-000000000001", "thread", canonicalID); rows != 1 {
		t.Fatalf("canonical-only discussion insert affected %d rows, want 1", rows)
	}
	if rows := insert("note canonical-only", `INSERT INTO gitlab_mr_note (id,merge_request_id,gitlab_note_id,vcs_pull_request_id) VALUES ($1,NULL,$2,$3) ON CONFLICT (merge_request_id,gitlab_note_id) DO NOTHING`, "50000000-0000-0000-0000-000000000002", int64(81), canonicalID); rows != 1 {
		t.Fatalf("canonical-only note insert affected %d rows, want 1", rows)
	}
	if rows := insert("job canonical-only", `INSERT INTO gitlab_pipeline_job (id,merge_request_id,job_id,vcs_pull_request_id) VALUES ($1,NULL,$2,$3) ON CONFLICT (merge_request_id,job_id) DO NOTHING`, "50000000-0000-0000-0000-000000000003", int64(91), canonicalID); rows != 1 {
		t.Fatalf("canonical-only job insert affected %d rows, want 1", rows)
	}
}

func TestBackfillLegacyDetailIDsLeavesCanonicalOnlyRows(t *testing.T) {
	ctx, pool := newIsolatedMigrationTestPool(t)
	if _, err := pool.Exec(ctx, `
		CREATE TABLE vcs_gitlab_migration_id_map (
			workspace_id UUID NOT NULL, entity_kind TEXT NOT NULL, legacy_id UUID NOT NULL, unified_id UUID NOT NULL
		);
		CREATE TABLE gitlab_mr_approval_state (workspace_id UUID NOT NULL, merge_request_id UUID, vcs_pull_request_id UUID);
		CREATE TABLE gitlab_mr_discussion (workspace_id UUID NOT NULL, merge_request_id UUID, vcs_pull_request_id UUID);
		CREATE TABLE gitlab_mr_note (workspace_id UUID NOT NULL, merge_request_id UUID, vcs_pull_request_id UUID);
		CREATE TABLE gitlab_pipeline_job (workspace_id UUID NOT NULL, merge_request_id UUID, vcs_pull_request_id UUID);
		INSERT INTO vcs_gitlab_migration_id_map (workspace_id,entity_kind,legacy_id,unified_id)
		VALUES ('10000000-0000-0000-0000-000000000001','pull_request',
		        '20000000-0000-0000-0000-000000000001','30000000-0000-0000-0000-000000000001')
	`); err != nil {
		t.Fatalf("create rollback detail fixture: %v", err)
	}
	for _, table := range []string{"gitlab_mr_approval_state", "gitlab_mr_discussion", "gitlab_mr_note", "gitlab_pipeline_job"} {
		if _, err := pool.Exec(ctx, fmt.Sprintf(`
			INSERT INTO %s (workspace_id,merge_request_id,vcs_pull_request_id) VALUES
				('10000000-0000-0000-0000-000000000001',NULL,'30000000-0000-0000-0000-000000000001'),
				('10000000-0000-0000-0000-000000000002',NULL,'30000000-0000-0000-0000-000000000002')
		`, table)); err != nil {
			t.Fatalf("seed %s legacy and canonical-only rows: %v", table, err)
		}
	}
	if err := NewRunner(pool, Environment{}).backfillLegacyDetailIDs(ctx); err != nil {
		t.Fatalf("backfill legacy detail identities: %v", err)
	}
	for _, table := range []string{"gitlab_mr_approval_state", "gitlab_mr_discussion", "gitlab_mr_note", "gitlab_pipeline_job"} {
		for _, testCase := range []struct {
			workspaceID   string
			pullRequestID string
			wantLegacyID  string
		}{
			{"10000000-0000-0000-0000-000000000001", "30000000-0000-0000-0000-000000000001", "20000000-0000-0000-0000-000000000001"},
			{"10000000-0000-0000-0000-000000000002", "30000000-0000-0000-0000-000000000002", ""},
		} {
			var got string
			if err := pool.QueryRow(ctx, fmt.Sprintf(`
				SELECT COALESCE(merge_request_id::text,'') FROM %s
				WHERE workspace_id=$1 AND vcs_pull_request_id=$2
			`, table), testCase.workspaceID, testCase.pullRequestID).Scan(&got); err != nil {
				t.Fatalf("read %s rollback identity: %v", table, err)
			}
			if got != testCase.wantLegacyID {
				t.Fatalf("%s workspace %s legacy ID = %q, want %q", table, testCase.workspaceID, got, testCase.wantLegacyID)
			}
		}
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO vcs_gitlab_migration_id_map (workspace_id,entity_kind,legacy_id,unified_id)
		VALUES ('10000000-0000-0000-0000-000000000001','pull_request',
		        '20000000-0000-0000-0000-000000000002','30000000-0000-0000-0000-000000000003');
		INSERT INTO gitlab_mr_discussion (workspace_id,merge_request_id,vcs_pull_request_id)
		VALUES ('10000000-0000-0000-0000-000000000001',
		        '20000000-0000-0000-0000-000000000099','30000000-0000-0000-0000-000000000003')
	`); err != nil {
		t.Fatalf("seed conflicting legacy detail identity: %v", err)
	}
	if err := NewRunner(pool, Environment{}).backfillLegacyDetailIDs(ctx); err == nil {
		t.Fatal("backfill accepted a mapped detail row with a conflicting legacy MR ID")
	}
	var conflictingID string
	if err := pool.QueryRow(ctx, `
		SELECT merge_request_id::text FROM gitlab_mr_discussion
		WHERE workspace_id='10000000-0000-0000-0000-000000000001'
		  AND vcs_pull_request_id='30000000-0000-0000-0000-000000000003'
	`).Scan(&conflictingID); err != nil {
		t.Fatalf("read conflicting legacy detail identity: %v", err)
	}
	if conflictingID != "20000000-0000-0000-0000-000000000099" {
		t.Fatalf("conflicting legacy identity was overwritten as %q", conflictingID)
	}
}

func TestReplayProcessesAllReceiptsForDeliveryID(t *testing.T) {
	ctx, pool := newIsolatedMigrationTestPool(t)
	if _, err := pool.Exec(ctx, `
		CREATE TABLE vcs_gitlab_migration_state (
			id SMALLINT PRIMARY KEY, phase TEXT NOT NULL, maintenance_mode BOOLEAN NOT NULL,
			generation BIGINT NOT NULL DEFAULT 1, began_at TIMESTAMPTZ,
			updated_at TIMESTAMPTZ NOT NULL DEFAULT now(), last_error TEXT
		);
		CREATE TABLE vcs_gitlab_migration_delivery (
			delivery_id TEXT NOT NULL, workspace_id UUID NOT NULL, connection_id UUID NOT NULL,
			scope_type TEXT NOT NULL, scope_id TEXT NOT NULL, scope_path TEXT NOT NULL DEFAULT '',
			hook_id BIGINT NOT NULL, event_type TEXT NOT NULL, received_at TIMESTAMPTZ NOT NULL DEFAULT now(),
			payload JSONB, reconciled_at TIMESTAMPTZ, reconciliation_note TEXT,
			PRIMARY KEY (delivery_id,workspace_id,connection_id,scope_type,scope_id,hook_id)
		);
		INSERT INTO vcs_gitlab_migration_state (id,phase,maintenance_mode) VALUES (1,'ready',TRUE);
		INSERT INTO vcs_gitlab_migration_delivery
			(delivery_id,workspace_id,connection_id,scope_type,scope_id,hook_id,event_type,payload)
		VALUES
			('delivery-1','10000000-0000-0000-0000-000000000001','20000000-0000-0000-0000-000000000001','project','17',71,'Merge Request Hook','{"object_kind":"merge_request"}'),
			('delivery-1','10000000-0000-0000-0000-000000000002','20000000-0000-0000-0000-000000000002','group','team',72,'Pipeline Hook','{"object_kind":"pipeline"}')
	`); err != nil {
		t.Fatalf("create replay receipt fixture: %v", err)
	}
	since := time.Now().Add(-time.Minute)
	var callbacks int
	runner := NewRunner(pool, Environment{})
	if err := runner.Replay(ctx, since, func(ctx context.Context, deliveryID string) error {
		callbacks++
		if deliveryID != "delivery-1" {
			return fmt.Errorf("unexpected delivery ID %q", deliveryID)
		}
		tag, err := pool.Exec(ctx, `
			UPDATE vcs_gitlab_migration_delivery
			SET reconciled_at=now()
			WHERE delivery_id=$1 AND received_at >= $2
		`, deliveryID, since)
		if err != nil {
			return err
		}
		if tag.RowsAffected() != 2 {
			return fmt.Errorf("reconciled %d receipt rows, want 2", tag.RowsAffected())
		}
		return nil
	}); err != nil {
		t.Fatalf("replay maintenance delivery: %v", err)
	}
	if callbacks != 1 {
		t.Fatalf("replay handler calls = %d, want one per delivery ID", callbacks)
	}
	state, err := runner.State(ctx)
	if err != nil {
		t.Fatalf("read migration status with retained IDs: %v", err)
	}
	if len(state.RecoveryDeliveryIDs) != 1 || state.RecoveryDeliveryIDs[0] != "delivery-1" {
		t.Fatalf("recovery delivery IDs = %#v, want [delivery-1]", state.RecoveryDeliveryIDs)
	}
	var pending int64
	if err := pool.QueryRow(ctx, `
		SELECT count(*) FROM vcs_gitlab_migration_delivery
		WHERE delivery_id='delivery-1' AND (reconciled_at IS NULL OR payload IS NULL)
	`).Scan(&pending); err != nil {
		t.Fatalf("check replayed receipts: %v", err)
	}
	if pending != 0 {
		t.Fatalf("replay left %d pending receipt rows", pending)
	}
}

func TestFinalizeSerializesWithMaintenanceReceiptIngress(t *testing.T) {
	t.Run("receipt commits before finalization check", func(t *testing.T) {
		ctx, pool := newIsolatedMigrationTestPool(t)
		ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		defer cancel()
		createFinalizeRaceFixture(t, ctx, pool, false)
		ingressTx, err := pool.Begin(ctx)
		if err != nil {
			t.Fatalf("begin maintenance receipt transaction: %v", err)
		}
		defer ingressTx.Rollback(context.Background())
		var phase string
		var maintenance bool
		if err := ingressTx.QueryRow(ctx, `SELECT phase,maintenance_mode FROM vcs_gitlab_migration_state WHERE id=1 FOR SHARE`).Scan(&phase, &maintenance); err != nil {
			t.Fatalf("lock migration gate for ingress: %v", err)
		}
		if phase != "ready" || !maintenance {
			t.Fatalf("ingress observed (%s,%v), want ready maintenance", phase, maintenance)
		}
		if _, err := ingressTx.Exec(ctx, `
			INSERT INTO vcs_gitlab_migration_delivery
				(delivery_id,workspace_id,connection_id,scope_type,scope_id,hook_id,event_type,payload)
			VALUES ('delivery-before-finalize','10000000-0000-0000-0000-000000000001',
			        '20000000-0000-0000-0000-000000000001','project','17',71,
			        'Merge Request Hook','{"object_kind":"merge_request"}')
		`); err != nil {
			t.Fatalf("persist maintenance receipt: %v", err)
		}
		runner := NewRunner(pool, Environment{VCSIntegrationEnabled: true})
		finalized := make(chan struct{})
		var finalizeErr error
		go func() {
			finalizeErr = runner.Finalize(ctx)
			close(finalized)
		}()
		waitForPgActivity(t, ctx, pool, "Lock", "", "FOR UPDATE", finalized)
		if err := ingressTx.Commit(ctx); err != nil {
			t.Fatalf("commit persisted maintenance receipt: %v", err)
		}
		select {
		case <-finalized:
		case <-ctx.Done():
			t.Fatal("finalization did not resume after receipt commit")
		}
		if finalizeErr == nil {
			t.Fatal("finalization ignored a receipt committed before its verification gate")
		}
		state, err := runner.State(ctx)
		if err != nil {
			t.Fatalf("read blocked finalization state: %v", err)
		}
		if state.Phase != "ready" || !state.MaintenanceMode || len(state.RecoveryDeliveryIDs) != 1 || state.RecoveryDeliveryIDs[0] != "delivery-before-finalize" {
			t.Fatalf("blocked finalization state = %#v", state)
		}
	})

	t.Run("ingress waits until complete then uses normal route", func(t *testing.T) {
		ctx, pool := newIsolatedMigrationTestPool(t)
		ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		defer cancel()
		createFinalizeRaceFixture(t, ctx, pool, true)
		runner := NewRunner(pool, Environment{VCSIntegrationEnabled: true})
		finalized := make(chan struct{})
		var finalizeErr error
		go func() {
			finalizeErr = runner.Finalize(ctx)
			close(finalized)
		}()
		waitForPgActivity(t, ctx, pool, "Timeout", "PgSleep", "UPDATE vcs_gitlab_migration_state", finalized)
		ingressDone := make(chan struct{})
		var ingressErr error
		var ingressPhase string
		var ingressMaintenance, routedNormally, persistedReceipt bool
		go func() {
			defer close(ingressDone)
			tx, err := pool.Begin(ctx)
			if err != nil {
				ingressErr = err
				return
			}
			defer tx.Rollback(context.Background())
			if err := tx.QueryRow(ctx, `SELECT phase,maintenance_mode FROM vcs_gitlab_migration_state WHERE id=1 FOR SHARE`).Scan(&ingressPhase, &ingressMaintenance); err != nil {
				ingressErr = err
				return
			}
			if ingressMaintenance {
				if _, err := tx.Exec(ctx, `
					INSERT INTO vcs_gitlab_migration_delivery
						(delivery_id,workspace_id,connection_id,scope_type,scope_id,hook_id,event_type,payload)
					VALUES ('delivery-after-finalize','10000000-0000-0000-0000-000000000003',
					        '20000000-0000-0000-0000-000000000003','project','18',73,
					        'Merge Request Hook','{"object_kind":"merge_request"}')
				`); err != nil {
					ingressErr = err
					return
				}
				persistedReceipt = true
			} else if ingressPhase == "complete" {
				routedNormally = true
			}
			ingressErr = tx.Commit(ctx)
		}()
		waitForPgActivity(t, ctx, pool, "Lock", "", "FOR SHARE", ingressDone)
		select {
		case <-finalized:
		case <-ctx.Done():
			t.Fatal("finalization did not complete while ingress waited")
		}
		if finalizeErr != nil {
			t.Fatalf("finalize before ingress: %v", finalizeErr)
		}
		noCredentialRunner := NewRunner(pool, Environment{})
		report, err := noCredentialRunner.Preflight(ctx)
		if err != nil || !report.NoOp {
			t.Fatalf("preflight after finalized schema = (%#v, %v), want no-op without credentials", report, err)
		}
		if err := noCredentialRunner.Migrate(ctx); err != nil {
			t.Fatalf("repeat migrate after finalization: %v", err)
		}
		var keptLegacyIDColumns, notNullLegacyIDColumns int
		if err := pool.QueryRow(ctx, `
			SELECT count(*),count(*) FILTER (WHERE attnotnull)
			FROM pg_attribute
			WHERE attname='merge_request_id' AND NOT attisdropped
			  AND attrelid IN (
			    'gitlab_mr_approval_state'::regclass,
			    'gitlab_mr_discussion'::regclass,
			    'gitlab_mr_note'::regclass,
			    'gitlab_pipeline_job'::regclass
			  )
		`).Scan(&keptLegacyIDColumns, &notNullLegacyIDColumns); err != nil {
			t.Fatalf("inspect post-finalize detail columns: %v", err)
		}
		if keptLegacyIDColumns != 4 || notNullLegacyIDColumns != 0 {
			t.Fatalf("post-finalize merge_request_id columns: count=%d not-null=%d, want 4 nullable columns", keptLegacyIDColumns, notNullLegacyIDColumns)
		}
		var legacyCorePresent bool
		if err := pool.QueryRow(ctx, `SELECT to_regclass(format('%I.%I',current_schema(),'gitlab_merge_request')) IS NOT NULL`).Scan(&legacyCorePresent); err != nil {
			t.Fatalf("inspect dropped legacy MR table: %v", err)
		}
		if legacyCorePresent {
			t.Fatal("finalization retained the legacy core MR table")
		}
		tag, err := pool.Exec(ctx, `
			INSERT INTO gitlab_mr_approval_state (merge_request_id,vcs_pull_request_id)
			VALUES (NULL,'30000000-0000-0000-0000-000000000001')
			ON CONFLICT (merge_request_id) DO NOTHING
		`)
		if err != nil || tag.RowsAffected() != 1 {
			t.Fatalf("post-finalize canonical detail insert: tag=%v error=%v", tag, err)
		}
		select {
		case <-ingressDone:
		case <-ctx.Done():
			t.Fatal("ingress did not resume after finalization")
		}
		if ingressErr != nil {
			t.Fatalf("finish ingress after finalization: %v", ingressErr)
		}
		if ingressPhase != "complete" || ingressMaintenance || !routedNormally || persistedReceipt {
			t.Fatalf("post-finalize ingress state=(%s,%v), normal=%v persisted=%v", ingressPhase, ingressMaintenance, routedNormally, persistedReceipt)
		}
		var receiptCount int64
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM vcs_gitlab_migration_delivery`).Scan(&receiptCount); err != nil {
			t.Fatalf("check post-finalize receipts: %v", err)
		}
		if receiptCount != 0 {
			t.Fatalf("post-finalize request was recorded as maintenance receipt")
		}
	})
}

func createFinalizeRaceFixture(t *testing.T, ctx context.Context, pool *pgxpool.Pool, pauseFinalization bool) {
	t.Helper()
	if _, err := pool.Exec(ctx, `
		CREATE TABLE vcs_gitlab_migration_state (
			id SMALLINT PRIMARY KEY, phase TEXT NOT NULL, maintenance_mode BOOLEAN NOT NULL,
			generation BIGINT NOT NULL DEFAULT 1, began_at TIMESTAMPTZ,
			updated_at TIMESTAMPTZ NOT NULL DEFAULT now(), last_error TEXT
		);
		CREATE TABLE vcs_gitlab_migration_snapshot (generation BIGINT NOT NULL);
		CREATE TABLE vcs_gitlab_migration_hook (state TEXT NOT NULL, previous_secret_encrypted TEXT NOT NULL DEFAULT '');
		CREATE TABLE vcs_gitlab_migration_delivery (
			delivery_id TEXT NOT NULL, workspace_id UUID NOT NULL, connection_id UUID NOT NULL,
			scope_type TEXT NOT NULL, scope_id TEXT NOT NULL, scope_path TEXT NOT NULL DEFAULT '',
			hook_id BIGINT NOT NULL, event_type TEXT NOT NULL, received_at TIMESTAMPTZ NOT NULL DEFAULT now(),
			payload JSONB, reconciled_at TIMESTAMPTZ, reconciliation_note TEXT,
			PRIMARY KEY (delivery_id,workspace_id,connection_id,scope_type,scope_id,hook_id)
		);
		CREATE TABLE vcs_gitlab_project (legacy_link_policy BOOLEAN NOT NULL);
		CREATE TABLE vcs_gitlab_migration_id_map (
			workspace_id UUID NOT NULL, entity_kind TEXT NOT NULL, legacy_id UUID NOT NULL, unified_id UUID NOT NULL
		);
		CREATE TABLE vcs_gitlab_mr_detail (vcs_pull_request_id UUID PRIMARY KEY);
		CREATE TABLE issue_vcs_pull_request (
			issue_id UUID NOT NULL, pull_request_id UUID NOT NULL, gitlab_close_intent BOOLEAN NOT NULL,
			gitlab_linked_by_type TEXT, gitlab_linked_by_id UUID, gitlab_linked_at TIMESTAMPTZ,
			vcs_linked BOOLEAN NOT NULL
		);
		CREATE TABLE issue_gitlab_merge_request (issue_id UUID NOT NULL);
		CREATE TABLE gitlab_mr_pipeline (id UUID PRIMARY KEY);
		CREATE TABLE gitlab_mr_approval_state (merge_request_id UUID, vcs_pull_request_id UUID NOT NULL);
		CREATE UNIQUE INDEX gitlab_mr_approval_legacy_id_uidx ON gitlab_mr_approval_state (merge_request_id);
		CREATE UNIQUE INDEX gitlab_mr_approval_vcs_pr_uidx ON gitlab_mr_approval_state (vcs_pull_request_id);
		CREATE TABLE gitlab_mr_discussion (merge_request_id UUID, vcs_pull_request_id UUID NOT NULL);
		CREATE TABLE gitlab_mr_note (merge_request_id UUID, vcs_pull_request_id UUID NOT NULL);
		CREATE TABLE gitlab_pipeline_job (merge_request_id UUID, vcs_pull_request_id UUID NOT NULL);
		CREATE TABLE gitlab_merge_request (id UUID PRIMARY KEY);
		CREATE TABLE gitlab_project_binding (id UUID PRIMARY KEY);
		CREATE TABLE gitlab_connection (id UUID PRIMARY KEY);
		CREATE TABLE issue (
			id UUID NOT NULL, workspace_id UUID NOT NULL,
			CONSTRAINT issue_workspace_id_id_key UNIQUE (workspace_id,id)
		);
		INSERT INTO vcs_gitlab_migration_state (id,phase,maintenance_mode) VALUES (1,'ready',TRUE);
	`); err != nil {
		t.Fatalf("create finalization race fixture: %v", err)
	}
	if pauseFinalization {
		if _, err := pool.Exec(ctx, `
			CREATE FUNCTION pause_gitlab_finalization() RETURNS trigger LANGUAGE plpgsql AS $$
			BEGIN
				IF NEW.phase='complete' THEN
					PERFORM pg_sleep(1.5);
				END IF;
				RETURN NEW;
			END $$;
			CREATE TRIGGER pause_gitlab_finalization
			BEFORE UPDATE ON vcs_gitlab_migration_state
			FOR EACH ROW EXECUTE FUNCTION pause_gitlab_finalization();
		`); err != nil {
			t.Fatalf("create finalization pause trigger: %v", err)
		}
	}
}

func waitForPgActivity(t *testing.T, ctx context.Context, pool *pgxpool.Pool, waitType, waitEvent, queryFragment string, done <-chan struct{}) {
	t.Helper()
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	timeout := time.NewTimer(8 * time.Second)
	defer timeout.Stop()
	for {
		var waiting bool
		if err := pool.QueryRow(ctx, `
			SELECT EXISTS (
				SELECT 1 FROM pg_stat_activity
				WHERE datname=current_database()
				  AND wait_event_type=$1
				  AND ($2='' OR wait_event=$2)
				  AND query ILIKE $3
			)
		`, waitType, waitEvent, "%"+queryFragment+"%").Scan(&waiting); err != nil {
			t.Fatalf("inspect PostgreSQL lock wait: %v", err)
		}
		if waiting {
			return
		}
		select {
		case <-done:
			t.Fatalf("operation finished before PostgreSQL entered the expected wait for %q", queryFragment)
		case <-ticker.C:
		case <-timeout.C:
			t.Fatalf("timed out waiting for PostgreSQL wait %q/%q on %q", waitType, waitEvent, queryFragment)
		case <-ctx.Done():
			t.Fatalf("context expired waiting for PostgreSQL wait %q/%q on %q: %v", waitType, waitEvent, queryFragment, ctx.Err())
		}
	}
}

func newIsolatedMigrationTestPool(t *testing.T) (context.Context, *pgxpool.Pool) {
	t.Helper()
	dbURL := os.Getenv("DATABASE_URL")
	if dbURL == "" {
		t.Skip("integration test requires Postgres at DATABASE_URL")
	}
	ctx := context.Background()
	admin, err := pgxpool.New(ctx, dbURL)
	if err != nil {
		t.Fatalf("connect admin pool: %v", err)
	}
	schema := fmt.Sprintf("gitlab_cutover_aux_%d", time.Now().UnixNano())
	if _, err := admin.Exec(ctx, "CREATE SCHEMA "+schema); err != nil {
		admin.Close()
		t.Fatalf("create isolated schema: %v", err)
	}
	cfg, err := pgxpool.ParseConfig(dbURL)
	if err != nil {
		_, _ = admin.Exec(ctx, "DROP SCHEMA "+schema+" CASCADE")
		admin.Close()
		t.Fatalf("parse database URL: %v", err)
	}
	cfg.ConnConfig.RuntimeParams["search_path"] = schema
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		_, _ = admin.Exec(ctx, "DROP SCHEMA "+schema+" CASCADE")
		admin.Close()
		t.Fatalf("connect isolated pool: %v", err)
	}
	t.Cleanup(func() {
		pool.Close()
		_, _ = admin.Exec(context.Background(), "DROP SCHEMA "+schema+" CASCADE")
		admin.Close()
	})
	return ctx, pool
}
