package handler

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

type workspaceGitLabDeleteFixture struct {
	workspaceID            string
	legacyConnectionID     string
	legacyProjectID        string
	legacyMergeRequestID   string
	legacyPipelineID       int64
	legacyIssueID          string
	legacyDiscussionID     string
	legacyNoteID           string
	legacyJobID            string
	canonicalConnectionID  string
	canonicalProjectID     string
	canonicalPullRequestID string
}

type workspaceDeleteTempSearchPathTxStarter struct{ delegate txStarter }

func (s workspaceDeleteTempSearchPathTxStarter) Begin(ctx context.Context) (pgx.Tx, error) {
	tx, err := s.delegate.Begin(ctx)
	if err != nil {
		return nil, err
	}
	if _, err := tx.Exec(ctx, `CREATE TEMP TABLE workspace_delete_search_path_probe(id INTEGER) ON COMMIT DROP`); err != nil {
		_ = tx.Rollback(ctx)
		return nil, err
	}
	if _, err := tx.Exec(ctx, `SET LOCAL search_path = pg_temp, public`); err != nil {
		_ = tx.Rollback(ctx)
		return nil, err
	}
	return tx, nil
}

func newWorkspaceGitLabDeleteFixture(t *testing.T, suffix string, includeLegacy bool) workspaceGitLabDeleteFixture {
	t.Helper()
	ctx := context.Background()
	f := workspaceGitLabDeleteFixture{
		legacyConnectionID:    uuid.NewString(),
		legacyMergeRequestID:  uuid.NewString(),
		legacyDiscussionID:    uuid.NewString(),
		legacyNoteID:          uuid.NewString(),
		legacyJobID:           uuid.NewString(),
		legacyPipelineID:      7001,
		canonicalConnectionID: uuid.NewString(),
	}
	if err := testPool.QueryRow(ctx, `
		INSERT INTO workspace (name,slug) VALUES ($1,$2) RETURNING id
	`, "GitLab delete "+suffix, "handler-tests-gitlab-delete-"+suffix).Scan(&f.workspaceID); err != nil {
		t.Fatalf("create GitLab delete workspace: %v", err)
	}
	t.Cleanup(func() {
		cleanupWorkspaceGitLabDeleteFixture(f.workspaceID)
	})
	if _, err := testPool.Exec(ctx, `
		INSERT INTO member (workspace_id,user_id,role) VALUES ($1,$2,'owner')
	`, f.workspaceID, testUserID); err != nil {
		t.Fatalf("create GitLab delete workspace owner: %v", err)
	}
	if includeLegacy {
		if _, err := testPool.Exec(ctx, `
			INSERT INTO gitlab_connection (id,workspace_id,base_url,host)
			VALUES ($1,$2,$3,$4)
		`, f.legacyConnectionID, f.workspaceID, "https://legacy-"+suffix+".gitlab.example", "legacy-"+suffix+".gitlab.example"); err != nil {
			t.Fatalf("create legacy GitLab connection: %v", err)
		}
		if err := testPool.QueryRow(ctx, `
			INSERT INTO gitlab_project_binding (
				workspace_id,connection_id,gitlab_project_id,path_with_namespace,web_url
			) VALUES ($1,$2,7001,$3,$4) RETURNING id
		`, f.workspaceID, f.legacyConnectionID, "group/"+suffix, "https://legacy-"+suffix+".gitlab.example/group/"+suffix).Scan(&f.legacyProjectID); err != nil {
			t.Fatalf("create legacy GitLab project binding: %v", err)
		}
		if _, err := testPool.Exec(ctx, `
			INSERT INTO gitlab_merge_request (
				id,workspace_id,project_binding_id,gitlab_project_id,mr_iid,title,state,web_url,mr_created_at,mr_updated_at
			) VALUES ($1,$2,$3,7001,7,'legacy','open',$4,now(),now())
		`, f.legacyMergeRequestID, f.workspaceID, f.legacyProjectID, "https://legacy-"+suffix+".gitlab.example/group/"+suffix+"/-/merge_requests/7"); err != nil {
			t.Fatalf("create legacy GitLab merge request: %v", err)
		}
		if err := testPool.QueryRow(ctx, `
			INSERT INTO issue (workspace_id,title,creator_type,creator_id)
			VALUES ($1,$2,'member',$3) RETURNING id
		`, f.workspaceID, "legacy GitLab issue "+suffix, testUserID).Scan(&f.legacyIssueID); err != nil {
			t.Fatalf("create legacy GitLab issue: %v", err)
		}
		for _, statement := range []struct {
			name  string
			query string
			args  []any
		}{
			{"legacy pipeline", `INSERT INTO gitlab_mr_pipeline (merge_request_id,pipeline_id,sha,status,pipeline_updated_at) VALUES ($1,$2,'head','passed',now())`, []any{f.legacyMergeRequestID, f.legacyPipelineID}},
			{"legacy issue link", `INSERT INTO issue_gitlab_merge_request (workspace_id,issue_id,merge_request_id) VALUES ($1,$2,$3)`, []any{f.workspaceID, f.legacyIssueID, f.legacyMergeRequestID}},
			{"legacy approval state", `INSERT INTO gitlab_mr_approval_state (workspace_id,merge_request_id) VALUES ($1,$2)`, []any{f.workspaceID, f.legacyMergeRequestID}},
			{"legacy discussion", `INSERT INTO gitlab_mr_discussion (id,workspace_id,merge_request_id,gitlab_discussion_id) VALUES ($1,$2,$3,$4)`, []any{f.legacyDiscussionID, f.workspaceID, f.legacyMergeRequestID, "discussion-" + suffix}},
			{"legacy note", `INSERT INTO gitlab_mr_note (id,workspace_id,discussion_id,merge_request_id,gitlab_note_id,body) VALUES ($1,$2,$3,$4,7001,'note')`, []any{f.legacyNoteID, f.workspaceID, f.legacyDiscussionID, f.legacyMergeRequestID}},
			{"legacy pipeline job", `INSERT INTO gitlab_pipeline_job (id,workspace_id,merge_request_id,pipeline_id,job_id,name,status) VALUES ($1,$2,$3,7001,7002,'job','passed')`, []any{f.legacyJobID, f.workspaceID, f.legacyMergeRequestID}},
		} {
			if _, err := testPool.Exec(ctx, statement.query, statement.args...); err != nil {
				t.Fatalf("create %s: %v", statement.name, err)
			}
		}
	}
	if _, err := testPool.Exec(ctx, `
		INSERT INTO vcs_connection (
			id,workspace_id,provider,instance_url,account_login,access_token_encrypted,webhook_secret_encrypted
		) VALUES ($1,$2,'gitlab',$3,'workspace-owner','test-token-ciphertext','test-hook-ciphertext')
	`, f.canonicalConnectionID, f.workspaceID, "https://canonical-"+suffix+".gitlab.example"); err != nil {
		t.Fatalf("create canonical GitLab connection: %v", err)
	}
	if err := testPool.QueryRow(ctx, `
		INSERT INTO vcs_gitlab_project (workspace_id,connection_id,gitlab_project_id,path_with_namespace,web_url)
		VALUES ($1,$2,8001,$3,$4) RETURNING id
	`, f.workspaceID, f.canonicalConnectionID, "group/"+suffix, "https://canonical-"+suffix+".gitlab.example/group/"+suffix).Scan(&f.canonicalProjectID); err != nil {
		t.Fatalf("create canonical GitLab project: %v", err)
	}
	if err := testPool.QueryRow(ctx, `
		INSERT INTO vcs_pull_request (
			workspace_id,connection_id,provider,repo_owner,repo_name,pr_number,title,state,html_url,
			pr_created_at,pr_updated_at,additions,deletions,changed_files
		) VALUES ($1,$2,'gitlab','group',$3,8,'canonical','open',$4,now(),now(),0,0,0) RETURNING id
	`, f.workspaceID, f.canonicalConnectionID, suffix, "https://canonical-"+suffix+".gitlab.example/group/"+suffix+"/-/merge_requests/8").Scan(&f.canonicalPullRequestID); err != nil {
		t.Fatalf("create canonical GitLab pull request: %v", err)
	}
	for _, statement := range []struct {
		name  string
		query string
		args  []any
	}{
		{"canonical MR detail", `INSERT INTO vcs_gitlab_mr_detail (vcs_pull_request_id,description) VALUES ($1,'detail')`, []any{f.canonicalPullRequestID}},
		{"canonical pipeline", `INSERT INTO vcs_gitlab_pipeline (workspace_id,connection_id,pull_request_id,pipeline_id,pipeline_project_id,pipeline_project_path,sha,ref,status,pipeline_updated_at) VALUES ($1,$2,$3,8001,8002,$4,'head','refs/heads/main','passed',now())`, []any{f.workspaceID, f.canonicalConnectionID, f.canonicalPullRequestID, "group/" + suffix}},
		{"migration connection map", `INSERT INTO vcs_gitlab_migration_id_map (workspace_id,entity_kind,legacy_id,unified_id) VALUES ($1,'connection',$2,$3)`, []any{f.workspaceID, f.legacyConnectionID, f.canonicalConnectionID}},
		{"migration snapshot", `INSERT INTO vcs_gitlab_migration_snapshot (generation,entity_kind,workspace_id,record_key,existed,before_image) VALUES (1,'connection',$1,$2,false,NULL)`, []any{f.workspaceID, f.legacyConnectionID}},
		{"migration Hook ledger", `INSERT INTO vcs_gitlab_migration_hook (connection_id,workspace_id,scope_type,scope_id,managed,previous_config,state) VALUES ($1,$2,'project','8001',true,'{}'::jsonb,'verified')`, []any{f.canonicalConnectionID, f.workspaceID}},
		{"migration delivery receipt", `INSERT INTO vcs_gitlab_migration_delivery (delivery_id,workspace_id,connection_id,scope_type,scope_id,hook_id,event_type) VALUES ($1,$2,$3,'project','8001',1,'Merge Request Hook')`, []any{"delivery-" + suffix, f.workspaceID, f.canonicalConnectionID}},
	} {
		if _, err := testPool.Exec(ctx, statement.query, statement.args...); err != nil {
			t.Fatalf("create %s: %v", statement.name, err)
		}
	}
	return f
}

func cleanupWorkspaceGitLabDeleteFixture(workspaceID string) {
	ctx := context.Background()
	tx, err := testPool.Begin(ctx)
	if err == nil {
		if err := deleteWorkspaceGitLabData(ctx, tx, parseUUID(workspaceID)); err == nil {
			_, _ = tx.Exec(ctx, `DELETE FROM vcs_gitlab_pipeline WHERE workspace_id=$1`, workspaceID)
			_, _ = tx.Exec(ctx, `DELETE FROM vcs_pull_request WHERE workspace_id=$1`, workspaceID)
			_, _ = tx.Exec(ctx, `DELETE FROM vcs_connection WHERE workspace_id=$1`, workspaceID)
			_, _ = tx.Exec(ctx, `DELETE FROM workspace WHERE id=$1`, workspaceID)
			_ = tx.Commit(ctx)
		} else {
			_ = tx.Rollback(ctx)
		}
	}
	_, _ = testPool.Exec(ctx, `DELETE FROM workspace WHERE id=$1`, workspaceID)
}

func workspaceGitLabDeleteRowsPresent(t *testing.T, f workspaceGitLabDeleteFixture, includeLegacy bool, expected int64) {
	t.Helper()
	queries := []struct {
		name  string
		query string
		args  []any
	}{
		{"canonical project", `SELECT count(*) FROM vcs_gitlab_project WHERE id=$1`, []any{f.canonicalProjectID}},
		{"canonical pull request", `SELECT count(*) FROM vcs_pull_request WHERE id=$1`, []any{f.canonicalPullRequestID}},
		{"canonical MR detail", `SELECT count(*) FROM vcs_gitlab_mr_detail WHERE vcs_pull_request_id=$1`, []any{f.canonicalPullRequestID}},
		{"canonical pipeline", `SELECT count(*) FROM vcs_gitlab_pipeline WHERE pull_request_id=$1`, []any{f.canonicalPullRequestID}},
		{"migration connection map", `SELECT count(*) FROM vcs_gitlab_migration_id_map WHERE workspace_id=$1`, []any{f.workspaceID}},
		{"migration snapshot", `SELECT count(*) FROM vcs_gitlab_migration_snapshot WHERE workspace_id=$1`, []any{f.workspaceID}},
		{"migration Hook ledger", `SELECT count(*) FROM vcs_gitlab_migration_hook WHERE workspace_id=$1`, []any{f.workspaceID}},
		{"migration delivery receipt", `SELECT count(*) FROM vcs_gitlab_migration_delivery WHERE workspace_id=$1`, []any{f.workspaceID}},
	}
	if includeLegacy {
		queries = append(queries, []struct {
			name  string
			query string
			args  []any
		}{
			{"legacy connection", `SELECT count(*) FROM gitlab_connection WHERE id=$1`, []any{f.legacyConnectionID}},
			{"legacy project binding", `SELECT count(*) FROM gitlab_project_binding WHERE id=$1`, []any{f.legacyProjectID}},
			{"legacy merge request", `SELECT count(*) FROM gitlab_merge_request WHERE id=$1`, []any{f.legacyMergeRequestID}},
			{"legacy pipeline", `SELECT count(*) FROM gitlab_mr_pipeline WHERE merge_request_id=$1`, []any{f.legacyMergeRequestID}},
			{"legacy issue link", `SELECT count(*) FROM issue_gitlab_merge_request WHERE issue_id=$1`, []any{f.legacyIssueID}},
			{"legacy approval state", `SELECT count(*) FROM gitlab_mr_approval_state WHERE merge_request_id=$1`, []any{f.legacyMergeRequestID}},
			{"legacy discussion", `SELECT count(*) FROM gitlab_mr_discussion WHERE id=$1`, []any{f.legacyDiscussionID}},
			{"legacy note", `SELECT count(*) FROM gitlab_mr_note WHERE id=$1`, []any{f.legacyNoteID}},
			{"legacy pipeline job", `SELECT count(*) FROM gitlab_pipeline_job WHERE id=$1`, []any{f.legacyJobID}},
		}...)
	}
	for _, item := range queries {
		var count int64
		if err := testPool.QueryRow(context.Background(), item.query, item.args...).Scan(&count); err != nil {
			t.Fatalf("count %s: %v", item.name, err)
		}
		if count != expected {
			t.Errorf("%s rows = %d, want %d", item.name, count, expected)
		}
	}
}

func TestDeleteWorkspaceCleansGitLabDataAcrossCutoverSchemas(t *testing.T) {
	if testPool == nil || testHandler == nil {
		t.Skip("handler test fixture not initialized (no DB?)")
	}
	for _, tc := range []struct {
		name           string
		includeLegacy  bool
		hideLegacyCore bool
	}{
		{name: "legacy schema", includeLegacy: true},
		{name: "after legacy finalization", hideLegacyCore: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.hideLegacyCore {
				previous := testHandler.TxStarter
				testHandler.TxStarter = workspaceDeleteTempSearchPathTxStarter{delegate: previous}
				t.Cleanup(func() { testHandler.TxStarter = previous })
			}
			victim := newWorkspaceGitLabDeleteFixture(t, "victim-"+uuid.NewString(), tc.includeLegacy)
			neighbour := newWorkspaceGitLabDeleteFixture(t, "neighbour-"+uuid.NewString(), tc.includeLegacy)
			workspaceGitLabDeleteRowsPresent(t, victim, tc.includeLegacy, 1)
			workspaceGitLabDeleteRowsPresent(t, neighbour, tc.includeLegacy, 1)
			var migrationPhaseBefore string
			if err := testPool.QueryRow(context.Background(), `SELECT phase FROM vcs_gitlab_migration_state WHERE id=1`).Scan(&migrationPhaseBefore); err != nil {
				t.Fatalf("read global GitLab migration state before deletion: %v", err)
			}

			rec := httptest.NewRecorder()
			req := newRequest(http.MethodDelete, "/api/workspaces/"+victim.workspaceID, nil)
			testHandler.DeleteWorkspace(rec, withURLParam(req, "id", victim.workspaceID))
			if rec.Code != http.StatusNoContent {
				t.Fatalf("DeleteWorkspace = %d %s", rec.Code, rec.Body.String())
			}
			workspaceGitLabDeleteRowsPresent(t, victim, tc.includeLegacy, 0)
			workspaceGitLabDeleteRowsPresent(t, neighbour, tc.includeLegacy, 1)
			var migrationPhaseAfter string
			if err := testPool.QueryRow(context.Background(), `SELECT phase FROM vcs_gitlab_migration_state WHERE id=1`).Scan(&migrationPhaseAfter); err != nil {
				t.Fatalf("read global GitLab migration state after deletion: %v", err)
			}
			if migrationPhaseAfter != migrationPhaseBefore {
				t.Fatalf("workspace deletion changed global GitLab migration state from %q to %q", migrationPhaseBefore, migrationPhaseAfter)
			}
		})
	}
}
