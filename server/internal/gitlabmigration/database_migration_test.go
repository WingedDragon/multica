package gitlabmigration

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/multica-ai/multica/server/internal/testutil"
)

type databaseMigrationFixture struct {
	pool       *pgxpool.Pool
	fx         *testutil.Fixture
	runner     *Runner
	env        Environment
	server     *httptest.Server
	workspace  string
	user       string
	issue      string
	connection string
	binding    string
	mergeReq   string
	at         time.Time
}

func newDatabaseMigrationFixture(t *testing.T) *databaseMigrationFixture {
	t.Helper()
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		t.Skip("integration test requires a disposable Postgres DATABASE_URL")
	}
	pool := testutil.IsolatedDB(t, dsn)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/v4/user":
			_, _ = w.Write([]byte(`{"username":"migration-user"}`))
		case "/api/v4/projects/77":
			_, _ = w.Write([]byte(`{"path_with_namespace":"group/sub/project"}`))
		case "/api/v4/groups":
			_, _ = w.Write([]byte(`[{"id":7,"full_path":"group"},{"id":8,"full_path":"group/sub"}]`))
		case "/api/v4/projects/77/hooks", "/api/v4/groups/7/hooks", "/api/v4/groups/8/hooks", "/api/v4/hooks":
			_, _ = w.Write([]byte(`[]`))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)

	seed := testutil.New(pool, "", "")
	workspace := seed.Workspace(t, "GitLab migration", "gitlab-migration-"+uuid.NewString())
	user := seed.User(t, "Migration user", "migration-"+uuid.NewString()+"@example.test")
	fx := testutil.New(pool, workspace, user)
	issue := fx.Issue(t, "Migration issue")
	key := make([]byte, 32)
	for i := range key {
		key[i] = 0x42
	}
	env := Environment{
		GitLabBaseURL: server.URL, GitLabToken: "migration-token", GitLabWebhookSecret: "migration-hook-secret",
		VCSSecretKey: base64.StdEncoding.EncodeToString(key), PublicURL: "https://multica.example.test",
		VCSIntegrationEnabled: true,
	}
	at := time.Date(2026, 11, 23, 10, 0, 0, 0, time.UTC)
	fixture := &databaseMigrationFixture{
		pool: pool, fx: fx, runner: NewRunner(pool, env), env: env, server: server,
		workspace: workspace, user: user, issue: issue, connection: uuid.NewString(),
		binding: uuid.NewString(), mergeReq: uuid.NewString(), at: at,
	}
	if _, err := pool.Exec(context.Background(), `
		UPDATE vcs_gitlab_migration_state
		SET phase='maintenance', maintenance_mode=true, generation=1
		WHERE id=1
	`); err != nil {
		t.Fatalf("prepare migration maintenance state: %v", err)
	}
	return fixture
}

func (f *databaseMigrationFixture) seedLegacyProjectAndMR(t *testing.T) {
	t.Helper()
	ctx := context.Background()
	box, err := f.env.box()
	if err != nil {
		t.Fatal(err)
	}
	token, err := sealSecret(box, f.env.GitLabToken)
	if err != nil {
		t.Fatal(err)
	}
	hookSecret, err := sealSecret(box, f.env.GitLabWebhookSecret)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.pool.Exec(ctx, `
		INSERT INTO gitlab_connection
			(id,workspace_id,base_url,host,connected_by_id,access_token_encrypted,webhook_secret_encrypted,created_at,updated_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$8)
	`, f.connection, f.workspace, f.server.URL, "gitlab.example.test", f.user, token, hookSecret, f.at); err != nil {
		t.Fatalf("insert legacy GitLab connection: %v", err)
	}
	if _, err := f.pool.Exec(ctx, `
		INSERT INTO gitlab_project_binding (
			id,workspace_id,connection_id,gitlab_project_id,path_with_namespace,web_url,last_sync_error,
			last_refresh_at,last_refresh_error,last_event_at,last_event_type,refresh_in_progress_at,created_at,updated_at
		)
		VALUES ($1,$2,$3,77,'group/sub/project','https://gitlab.example.test/group/sub/project','legacy sync error',
			$4,'legacy refresh error',$5,'legacy event',$6,$7,$8)
	`, f.binding, f.workspace, f.connection, f.at.Add(-20*time.Minute), f.at.Add(-10*time.Minute),
		f.at.Add(-2*time.Minute), f.at.Add(-time.Hour), f.at); err != nil {
		t.Fatalf("insert legacy GitLab project binding: %v", err)
	}
	if _, err := f.pool.Exec(ctx, `
		INSERT INTO gitlab_merge_request (
			id,workspace_id,project_binding_id,gitlab_project_id,mr_iid,title,description,state,web_url,
			source_branch,target_branch,author_username,sha,merge_commit_sha,detailed_merge_status,has_conflicts,
			additions,deletions,changed_files,mr_created_at,mr_updated_at,created_at,updated_at,
			reviewers,assignees,labels,last_refreshed_at,wakeup_sync_generation
		) VALUES ($1,$2,$3,77,81,'legacy title','legacy description','merged',
			'https://gitlab.example.test/group/sub/project/-/merge_requests/81','feature','main','legacy-author','legacy-head',
			'legacy-merge-sha','mergeable',false,5,2,1,$4,$5,$4,$5,
			'["reviewer"]'::jsonb,'["assignee"]'::jsonb,'["bug"]'::jsonb,$5,3)
	`, f.mergeReq, f.workspace, f.binding, f.at.Add(-time.Hour), f.at); err != nil {
		t.Fatalf("insert legacy GitLab MR: %v", err)
	}
}

func (f *databaseMigrationFixture) seedCanonicalConnection(t *testing.T, id string) (string, string) {
	t.Helper()
	ctx := context.Background()
	box, err := f.env.box()
	if err != nil {
		t.Fatal(err)
	}
	token, err := sealSecret(box, f.env.GitLabToken)
	if err != nil {
		t.Fatal(err)
	}
	hookSecret, err := sealSecret(box, f.env.GitLabWebhookSecret)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.pool.Exec(ctx, `
		INSERT INTO vcs_connection
			(id,workspace_id,provider,instance_url,account_login,access_token_encrypted,webhook_secret_encrypted,connected_by_id,created_at,updated_at)
		VALUES ($1,$2,'gitlab',$3,'migration-user',$4,$5,$6,$7,$7)
	`, id, f.workspace, f.server.URL, token, hookSecret, f.user, f.at); err != nil {
		t.Fatalf("insert existing VCS connection: %v", err)
	}
	return token, hookSecret
}
func (f *databaseMigrationFixture) seedCanonicalProject(t *testing.T, connectionID, id string) {
	t.Helper()
	if _, err := f.pool.Exec(context.Background(), `
		INSERT INTO vcs_gitlab_project (
			id,workspace_id,connection_id,gitlab_project_id,path_with_namespace,web_url,hook_id,hook_enabled,
			legacy_link_policy,last_sync_error,last_refresh_at,last_refresh_error,last_event_at,last_event_type,
			refresh_in_progress_at,created_at,updated_at
		) VALUES ($1,$2,$3,77,'group/sub/project','https://canonical.example.test/group/sub/project',991,true,
			false,'canonical sync error',$4,'canonical refresh error',$5,'canonical event',$6,$7,$8)
	`, id, f.workspace, connectionID, f.at.Add(-30*time.Minute), f.at.Add(-25*time.Minute),
		f.at.Add(-time.Hour), f.at.Add(-2*time.Hour), f.at.Add(-2*time.Hour)); err != nil {
		t.Fatalf("insert existing VCS GitLab project: %v", err)
	}
}

func (f *databaseMigrationFixture) seedCanonicalMR(t *testing.T, connectionID, id, title, state, head string, updatedAt time.Time) {
	t.Helper()
	if _, err := f.pool.Exec(context.Background(), `
		INSERT INTO vcs_pull_request (
			id,workspace_id,connection_id,provider,repo_owner,repo_name,pr_number,title,state,html_url,
			branch,head_sha,author_login,merged_at,closed_at,pr_created_at,pr_updated_at,
			additions,deletions,changed_files,has_conflicts,conflict_head_sha,created_at,updated_at
		) VALUES ($1,$2,$3,'gitlab','group/sub','project',81,$4,$5,
			'https://gitlab.example.test/group/sub/project/-/merge_requests/81','canonical-branch',$6,
			'canonical-author',NULL,NULL,$7,$8,11,4,2,false,'',$7,$8)
	`, id, f.workspace, connectionID, title, state, head, f.at.Add(-time.Hour), updatedAt); err != nil {
		t.Fatalf("insert canonical VCS MR: %v", err)
	}
}

func TestDatabaseMigrationImportsAndRestoresCanonicalRows(t *testing.T) {
	f := newDatabaseMigrationFixture(t)
	f.seedLegacyProjectAndMR(t)
	canonicalConnectionID, canonicalMRID := uuid.NewString(), uuid.NewString()
	tokenCipher, hookCipher := f.seedCanonicalConnection(t, canonicalConnectionID)
	canonicalUpdatedAt := f.at.Add(time.Minute)
	f.seedCanonicalMR(t, canonicalConnectionID, canonicalMRID, "canonical newer title", "open", "canonical-head", canonicalUpdatedAt)
	canonicalProjectID := uuid.NewString()
	f.seedCanonicalProject(t, canonicalConnectionID, canonicalProjectID)
	otherWorkspaceID := testutil.New(f.pool, "", "").Workspace(t, "Unrelated GitLab workspace", "gitlab-migration-unrelated-"+uuid.NewString())
	otherConnectionID, otherPullRequestID, otherDiscussionID := uuid.NewString(), uuid.NewString(), uuid.NewString()
	if _, err := f.pool.Exec(context.Background(), `
		INSERT INTO vcs_connection (
			id,workspace_id,provider,instance_url,account_login,access_token_encrypted,webhook_secret_encrypted,created_at,updated_at
		) VALUES ($1,$2,'gitlab',$3,'unrelated-user',$4,$5,$6,$6)
	`, otherConnectionID, otherWorkspaceID, f.server.URL, tokenCipher, hookCipher, f.at); err != nil {
		t.Fatalf("insert unrelated workspace VCS connection: %v", err)
	}
	if _, err := f.pool.Exec(context.Background(), `
		INSERT INTO vcs_pull_request (
			id,workspace_id,connection_id,provider,repo_owner,repo_name,pr_number,title,state,html_url,pr_created_at,pr_updated_at
		) VALUES ($1,$2,$3,'gitlab','unrelated-group','other-project',81,'unrelated workspace MR','open',
			'https://gitlab.example.test/unrelated-group/other-project/-/merge_requests/81',$4,$4)
	`, otherPullRequestID, otherWorkspaceID, otherConnectionID, f.at); err != nil {
		t.Fatalf("insert unrelated workspace VCS MR: %v", err)
	}
	if _, err := f.pool.Exec(context.Background(), `
		INSERT INTO gitlab_mr_discussion (
			id,workspace_id,vcs_pull_request_id,gitlab_discussion_id,resolved,discussion_updated_at,updated_at
		) VALUES ($1,$2,$3,'unrelated-workspace-discussion',true,$4,$4)
	`, otherDiscussionID, otherWorkspaceID, otherPullRequestID, f.at); err != nil {
		t.Fatalf("insert unrelated workspace discussion: %v", err)
	}
	if _, err := f.pool.Exec(context.Background(), `
		INSERT INTO vcs_gitlab_mr_detail (
			vcs_pull_request_id,description,merge_commit_sha,detailed_merge_status,target_branch,reviewers,assignees,labels,
			last_refreshed_at,last_refresh_error,conflict_sha,pipeline_sync_at,pipeline_sync_error,wakeup_sync_generation,
			created_at,updated_at
		) VALUES ($1,'canonical old description','canonical-merge','canonical-status','canonical-target',
			'["canonical-reviewer"]'::jsonb,'[]'::jsonb,'["canonical-label"]'::jsonb,NULL,'canonical refresh error',
			'canonical-conflict',$2,'canonical pipeline error',1,$3,$4)
	`, canonicalMRID, f.at.Add(5*time.Minute), f.at.Add(-time.Hour), f.at.Add(-time.Minute)); err != nil {
		t.Fatalf("insert existing VCS GitLab MR details: %v", err)
	}
	if _, err := f.pool.Exec(context.Background(), `
		UPDATE gitlab_merge_request
		SET last_refresh_error=NULL,pipeline_sync_at=$2,pipeline_sync_error=NULL
		WHERE id=$1
	`, f.mergeReq, f.at); err != nil {
		t.Fatalf("set successful legacy detail syncs: %v", err)
	}
	if _, err := f.pool.Exec(context.Background(), `
		INSERT INTO vcs_gitlab_pipeline (
			workspace_id,connection_id,pull_request_id,pipeline_id,pipeline_project_id,pipeline_project_path,
			sha,ref,status,web_url,pipeline_updated_at,created_at,updated_at
		) VALUES ($1,$2,$3,901,77,'group/sub/project','canonical-pipeline-head','canonical-ref','passed',
			'https://canonical.example.test/pipeline/901',$4,$5,$6)
	`, f.workspace, canonicalConnectionID, canonicalMRID, f.at.Add(time.Minute), f.at.Add(-time.Hour), f.at.Add(-time.Minute)); err != nil {
		t.Fatalf("insert existing VCS GitLab pipeline: %v", err)
	}

	if _, err := f.pool.Exec(context.Background(), `
		INSERT INTO issue_vcs_pull_request (
			issue_id,pull_request_id,close_intent,linked_by_type,linked_by_id,linked_at,vcs_linked
		) VALUES ($1,$2,false,'member',$3,$4,true)
	`, f.issue, canonicalMRID, f.user, f.at.Add(5*time.Minute)); err != nil {
		t.Fatalf("insert existing canonical issue link: %v", err)
	}
	if _, err := f.pool.Exec(context.Background(), `
		INSERT INTO issue_gitlab_merge_request (workspace_id,issue_id,merge_request_id,close_intent,linked_by_type,linked_by_id,linked_at)
		VALUES ($1,$2,$3,true,NULL,NULL,$4)
	`, f.workspace, f.issue, f.mergeReq, f.at.Add(3*time.Minute)); err != nil {
		t.Fatalf("insert legacy GitLab issue link: %v", err)
	}
	if _, err := f.pool.Exec(context.Background(), `
		INSERT INTO gitlab_mr_pipeline (merge_request_id,pipeline_id,pipeline_project_id,sha,ref,status,web_url,pipeline_updated_at,created_at,updated_at)
		VALUES ($1,901,77,'legacy-head','refs/merge-requests/81/head','failed',
			'https://gitlab.example.test/group/sub/project/-/pipelines/901',$2,$2,$2)
	`, f.mergeReq, f.at.Add(2*time.Minute)); err != nil {
		t.Fatalf("insert legacy GitLab pipeline: %v", err)
	}
	if _, err := f.pool.Exec(context.Background(), `
		INSERT INTO gitlab_mr_approval_state (merge_request_id,workspace_id,approved,approved_by,rules,updated_at)
		VALUES ($1,$2,true,'[{"name":"reviewer"}]'::jsonb,'[]'::jsonb,$3)
	`, f.mergeReq, f.workspace, f.at); err != nil {
		t.Fatalf("insert legacy approval state: %v", err)
	}
	discussionID, noteID, jobID := uuid.NewString(), uuid.NewString(), uuid.NewString()
	if _, err := f.pool.Exec(context.Background(), `
		INSERT INTO gitlab_mr_discussion (id,workspace_id,merge_request_id,gitlab_discussion_id,resolved)
		VALUES ($1,$2,$3,'discussion-1',false)
	`, discussionID, f.workspace, f.mergeReq); err != nil {
		t.Fatalf("insert legacy MR discussion: %v", err)
	}
	if _, err := f.pool.Exec(context.Background(), `
		INSERT INTO gitlab_mr_note (id,workspace_id,discussion_id,merge_request_id,gitlab_note_id,body)
		VALUES ($1,$2,$3,$4,902,'legacy note')
	`, noteID, f.workspace, discussionID, f.mergeReq); err != nil {
		t.Fatalf("insert legacy MR note: %v", err)
	}
	if _, err := f.pool.Exec(context.Background(), `
		INSERT INTO gitlab_pipeline_job (id,workspace_id,merge_request_id,pipeline_id,job_id,name,status)
		VALUES ($1,$2,$3,901,903,'test','failed')
	`, jobID, f.workspace, f.mergeReq); err != nil {
		t.Fatalf("insert legacy pipeline job: %v", err)
	}
	canonicalDiscussionID, canonicalNoteID, canonicalJobID := uuid.NewString(), uuid.NewString(), uuid.NewString()
	if _, err := f.pool.Exec(context.Background(), `
		INSERT INTO gitlab_mr_discussion (
			id,workspace_id,vcs_pull_request_id,gitlab_discussion_id,resolved,discussion_updated_at,updated_at
		) VALUES ($1,$2,$3,'discussion-1',true,$4,$4)
	`, canonicalDiscussionID, f.workspace, canonicalMRID, f.at); err != nil {
		t.Fatalf("insert canonical MR discussion: %v", err)
	}
	if _, err := f.pool.Exec(context.Background(), `
		INSERT INTO gitlab_mr_note (
			id,workspace_id,discussion_id,vcs_pull_request_id,gitlab_note_id,body,note_updated_at,updated_at
		) VALUES ($1,$2,$3,$4,902,'canonical older note',$5,$5)
	`, canonicalNoteID, f.workspace, canonicalDiscussionID, canonicalMRID, f.at); err != nil {
		t.Fatalf("insert canonical MR note: %v", err)
	}
	if _, err := f.pool.Exec(context.Background(), `
		INSERT INTO gitlab_pipeline_job (
			id,workspace_id,vcs_pull_request_id,pipeline_id,job_id,name,status,fetched_at,updated_at
		) VALUES ($1,$2,$3,901,903,'canonical older job','failed',$4,$4)
	`, canonicalJobID, f.workspace, canonicalMRID, f.at); err != nil {
		t.Fatalf("insert canonical pipeline job: %v", err)
	}
	if _, err := f.pool.Exec(context.Background(), `
		UPDATE gitlab_mr_discussion SET resolved=false,discussion_updated_at=$2,updated_at=$2 WHERE id=$1
	`, discussionID, f.at.Add(10*time.Minute)); err != nil {
		t.Fatalf("make legacy discussion newer: %v", err)
	}
	if _, err := f.pool.Exec(context.Background(), `
		UPDATE gitlab_mr_note SET body='legacy newer note',note_updated_at=$2,updated_at=$2 WHERE id=$1
	`, noteID, f.at.Add(10*time.Minute)); err != nil {
		t.Fatalf("make legacy note newer: %v", err)
	}
	if _, err := f.pool.Exec(context.Background(), `
		UPDATE gitlab_pipeline_job SET name='legacy newer job',status='success',fetched_at=$2,updated_at=$2 WHERE id=$1
	`, jobID, f.at.Add(10*time.Minute)); err != nil {
		t.Fatalf("make legacy pipeline job newer: %v", err)
	}

	chatWakeupID := uuid.NewString()
	chatCondition := json.RawMessage(`{"type":"pull_request","repo":"group/sub/project","number":81,"event":"checks_finished","provider":"gitlab","connection_id":"` + f.connection + `","gitlab":{"source":"gitlab_connection","merge_request_id":"` + f.mergeReq + `","activated_at":"2026-11-23T10:00:00Z","baseline_available":true,"baseline_head_sha":"legacy-head","baseline_pipelines":[{"pipeline_id":"901","pipeline_project_id":"77","pipeline_project_path":"group/sub/project","ref":"refs/merge-requests/81/head","sha":"legacy-head","status":"failed","updated_at":"2026-11-23T10:02:00Z"}]}}`)
	chatStateSuffix := "77:group/sub/project:refs/merge-requests/81/head:901:legacy-head:failed:" + f.at.Add(2*time.Minute).Format(time.RFC3339Nano)
	legacyChatState := "gitlab:gitlab_connection:" + f.connection + ":" + f.mergeReq + ":" + chatStateSuffix
	canonicalChatState := "gitlab:vcs_connection:" + canonicalConnectionID + ":" + canonicalMRID + ":" + chatStateSuffix
	if _, err := f.pool.Exec(context.Background(), `
		INSERT INTO chat_wakeup (id,workspace_id,chat_session_id,agent_id,requester_id,title,instruction,kind,condition,condition_state,next_eval_at,expires_at)
		VALUES ($1,$2,$3,$4,$5,'Wait for MR','resume','condition',$6::jsonb,$7,$8,$9)
	`, chatWakeupID, f.workspace, uuid.NewString(), uuid.NewString(), f.user, string(chatCondition), legacyChatState, f.at, f.at.Add(24*time.Hour)); err != nil {
		t.Fatalf("insert legacy GitLab chat wakeup: %v", err)
	}
	issueWakeupID := uuid.NewString()
	issueCondition := `{"type":"pull_request","event":"checks_finished"}`
	if _, err := f.pool.Exec(context.Background(), `
		INSERT INTO issue_wakeup (id,workspace_id,issue_id,agent_id,created_by,instruction,kind,mode,condition,condition_state)
		VALUES ($1,$2,$3,$4,$5,'wake on linked MR','event','once',$6::jsonb,'legacy-ci-baseline')
	`, issueWakeupID, f.workspace, f.issue, uuid.NewString(), f.user, issueCondition); err != nil {
		t.Fatalf("insert issue wakeup: %v", err)
	}

	if err := f.runner.Migrate(context.Background()); err != nil {
		t.Fatalf("migrate legacy GitLab data: %v", err)
	}
	var projectPolicy, projectHookEnabled bool
	var projectHookID int64
	var projectPath, projectURL, projectSyncError, projectRefreshError, projectEventType string
	var projectRefreshAt, projectEventAt, projectInProgress, projectCreatedAt, projectUpdatedAt time.Time
	if err := f.pool.QueryRow(context.Background(), `
		SELECT legacy_link_policy,hook_id,hook_enabled,path_with_namespace,web_url,last_sync_error,
		       last_refresh_at,last_refresh_error,last_event_at,last_event_type,refresh_in_progress_at,created_at,updated_at
		FROM vcs_gitlab_project WHERE id=$1
	`, canonicalProjectID).Scan(&projectPolicy, &projectHookID, &projectHookEnabled, &projectPath, &projectURL, &projectSyncError,
		&projectRefreshAt, &projectRefreshError, &projectEventAt, &projectEventType, &projectInProgress, &projectCreatedAt, &projectUpdatedAt); err != nil {
		t.Fatal(err)
	}
	if !projectPolicy || projectHookID != 991 || !projectHookEnabled || projectPath != "group/sub/project" ||
		projectURL != "https://canonical.example.test/group/sub/project" || projectSyncError != "legacy sync error" ||
		projectRefreshError != "legacy refresh error" || projectEventType != "legacy event" ||
		!projectRefreshAt.Equal(f.at.Add(-20*time.Minute)) || !projectEventAt.Equal(f.at.Add(-10*time.Minute)) ||
		!projectInProgress.Equal(f.at.Add(-2*time.Minute)) || !projectCreatedAt.Equal(f.at.Add(-2*time.Hour)) ||
		!projectUpdatedAt.Equal(f.at) {
		t.Fatalf("project merge lost newest state or canonical Hook settings: policy=%v hook=%d/%v URL=%q sync=%q refresh=%q/%s event=%q/%s progress=%s created=%s updated=%s",
			projectPolicy, projectHookID, projectHookEnabled, projectURL, projectSyncError, projectRefreshError, projectRefreshAt,
			projectEventType, projectEventAt, projectInProgress, projectCreatedAt, projectUpdatedAt)
	}
	var mappedMRID string
	if err := f.pool.QueryRow(context.Background(), `
		SELECT unified_id::text FROM vcs_gitlab_migration_id_map
		WHERE workspace_id=$1 AND entity_kind='pull_request' AND legacy_id=$2
	`, f.workspace, f.mergeReq).Scan(&mappedMRID); err != nil || mappedMRID != canonicalMRID {
		t.Fatalf("legacy MR mapping = %q, err=%v; want %q", mappedMRID, err, canonicalMRID)
	}
	var title, state, headSHA string
	if err := f.pool.QueryRow(context.Background(), `SELECT title,state,head_sha FROM vcs_pull_request WHERE id=$1`, canonicalMRID).Scan(&title, &state, &headSHA); err != nil {
		t.Fatal(err)
	}
	if title != "canonical newer title" || state != "open" || headSHA != "canonical-head" {
		t.Fatalf("older legacy MR regressed canonical state: %q %q %q", title, state, headSHA)
	}
	var closeIntent, vcsLinked, gitlabCloseIntent bool
	var linkedByType, linkedByID, gitlabLinkedByType, gitlabLinkedByID *string
	var linkedAt, gitlabLinkedAt time.Time
	if err := f.pool.QueryRow(context.Background(), `
		SELECT close_intent,vcs_linked,linked_by_type,linked_by_id::text,linked_at,
		       gitlab_close_intent,gitlab_linked_by_type,gitlab_linked_by_id::text,gitlab_linked_at
		FROM issue_vcs_pull_request WHERE issue_id=$1 AND pull_request_id=$2
	`, f.issue, canonicalMRID).Scan(&closeIntent, &vcsLinked, &linkedByType, &linkedByID, &linkedAt,
		&gitlabCloseIntent, &gitlabLinkedByType, &gitlabLinkedByID, &gitlabLinkedAt); err != nil {
		t.Fatal(err)
	}
	if closeIntent || !vcsLinked || !gitlabCloseIntent || linkedByType == nil || *linkedByType != "member" || linkedByID == nil || *linkedByID != f.user || gitlabLinkedByType != nil || gitlabLinkedByID != nil || !linkedAt.Equal(f.at.Add(5*time.Minute)) || !gitlabLinkedAt.Equal(f.at.Add(3*time.Minute)) {
		t.Fatalf("merged issue link lost independent provenance/nulls: generic=(%v,%v,%v,%v,%v), gitlab=(%v,%v,%v,%v)", closeIntent, vcsLinked, linkedByType, linkedByID, linkedAt, gitlabCloseIntent, gitlabLinkedByType, gitlabLinkedByID, gitlabLinkedAt)
	}
	var pipelineCount, detailCount int
	if err := f.pool.QueryRow(context.Background(), `SELECT count(*) FROM vcs_gitlab_pipeline WHERE pull_request_id=$1 AND pipeline_id=901`, canonicalMRID).Scan(&pipelineCount); err != nil {
		t.Fatal(err)
	}
	if err := f.pool.QueryRow(context.Background(), `SELECT count(*) FROM vcs_gitlab_mr_detail WHERE vcs_pull_request_id=$1`, canonicalMRID).Scan(&detailCount); err != nil {
		t.Fatal(err)
	}
	if pipelineCount != 1 || detailCount != 1 {
		t.Fatalf("imported pipeline/detail counts = %d/%d, want 1/1", pipelineCount, detailCount)
	}

	var pipelineSHA, pipelineRef, pipelineStatus, pipelineURL string
	var pipelineVersion time.Time
	if err := f.pool.QueryRow(context.Background(), `
		SELECT sha,ref,status,web_url,pipeline_updated_at FROM vcs_gitlab_pipeline
		WHERE pull_request_id=$1 AND pipeline_id=901
	`, canonicalMRID).Scan(&pipelineSHA, &pipelineRef, &pipelineStatus, &pipelineURL, &pipelineVersion); err != nil {
		t.Fatal(err)
	}
	if pipelineSHA != "legacy-head" || pipelineRef != "refs/merge-requests/81/head" || pipelineStatus != "failed" ||
		!pipelineVersion.Equal(f.at.Add(2*time.Minute)) {
		t.Fatalf("newer legacy pipeline did not win: sha=%q ref=%q status=%q version=%s", pipelineSHA, pipelineRef, pipelineStatus, pipelineVersion)
	}
	var detailDescription, detailMergeCommit, detailTarget string
	var detailReviewers []byte
	var detailVersion time.Time
	var migratedRefreshAt, migratedPipelineSyncAt pgtype.Timestamptz
	var migratedRefreshError, migratedPipelineSyncError pgtype.Text
	if err := f.pool.QueryRow(context.Background(), `
		SELECT description,merge_commit_sha,target_branch,reviewers,updated_at,
		       last_refreshed_at,last_refresh_error,pipeline_sync_at,pipeline_sync_error
		FROM vcs_gitlab_mr_detail WHERE vcs_pull_request_id=$1
	`, canonicalMRID).Scan(&detailDescription, &detailMergeCommit, &detailTarget, &detailReviewers, &detailVersion,
		&migratedRefreshAt, &migratedRefreshError, &migratedPipelineSyncAt, &migratedPipelineSyncError); err != nil {
		t.Fatal(err)
	}
	if detailDescription != "legacy description" || detailMergeCommit != "legacy-merge-sha" || detailTarget != "main" ||
		!jsonEqual(json.RawMessage(detailReviewers), json.RawMessage(`["reviewer"]`)) || !detailVersion.Equal(f.at) {
		t.Fatalf("newer legacy MR detail did not win: description=%q merge=%q target=%q reviewers=%s updated=%s",
			detailDescription, detailMergeCommit, detailTarget, detailReviewers, detailVersion)
	}
	if !migratedRefreshAt.Valid || !migratedRefreshAt.Time.Equal(f.at) || migratedRefreshError.Valid ||
		!migratedPipelineSyncAt.Valid || !migratedPipelineSyncAt.Time.Equal(f.at.Add(5*time.Minute)) ||
		!migratedPipelineSyncError.Valid || migratedPipelineSyncError.String != "canonical pipeline error" {
		t.Fatalf("sync timestamps/errors were not merged as pairs: refresh=(%v,%v) pipeline=(%v,%v)",
			migratedRefreshAt, migratedRefreshError, migratedPipelineSyncAt, migratedPipelineSyncError)
	}
	for _, table := range []string{"gitlab_mr_approval_state", "gitlab_mr_discussion", "gitlab_mr_note", "gitlab_pipeline_job"} {
		var got int
		if err := f.pool.QueryRow(context.Background(), "SELECT count(*) FROM "+table+" WHERE workspace_id=$1 AND vcs_pull_request_id=$2", f.workspace, canonicalMRID).Scan(&got); err != nil {
			t.Fatal(err)
		}
		if got != 1 {
			t.Fatalf("%s was not uniquely attached to canonical MR: count=%d", table, got)
		}
	}
	var migratedDiscussionID string
	var migratedResolved bool
	if err := f.pool.QueryRow(context.Background(), `SELECT id::text,resolved FROM gitlab_mr_discussion WHERE workspace_id=$1 AND vcs_pull_request_id=$2 AND gitlab_discussion_id='discussion-1'`, f.workspace, canonicalMRID).Scan(&migratedDiscussionID, &migratedResolved); err != nil {
		t.Fatal(err)
	}
	var migratedNoteBody, migratedNoteDiscussionID string
	if err := f.pool.QueryRow(context.Background(), `SELECT body,discussion_id::text FROM gitlab_mr_note WHERE workspace_id=$1 AND vcs_pull_request_id=$2 AND gitlab_note_id=902`, f.workspace, canonicalMRID).Scan(&migratedNoteBody, &migratedNoteDiscussionID); err != nil {
		t.Fatal(err)
	}
	var migratedJobName, migratedJobStatus string
	if err := f.pool.QueryRow(context.Background(), `SELECT name,status FROM gitlab_pipeline_job WHERE workspace_id=$1 AND vcs_pull_request_id=$2 AND job_id=903`, f.workspace, canonicalMRID).Scan(&migratedJobName, &migratedJobStatus); err != nil {
		t.Fatal(err)
	}
	if migratedDiscussionID != canonicalDiscussionID || migratedResolved || migratedNoteBody != "legacy newer note" ||
		migratedNoteDiscussionID != canonicalDiscussionID || migratedJobName != "legacy newer job" || migratedJobStatus != "success" {
		t.Fatalf("duplicate GitLab details were not merged to latest content: discussion=%s/%v note=%q/%s job=%q/%q",
			migratedDiscussionID, migratedResolved, migratedNoteBody, migratedNoteDiscussionID, migratedJobName, migratedJobStatus)
	}
	var migratedCondition, migratedChatState string
	if err := f.pool.QueryRow(context.Background(), `SELECT condition::text,condition_state FROM chat_wakeup WHERE id=$1`, chatWakeupID).Scan(&migratedCondition, &migratedChatState); err != nil {
		t.Fatal(err)
	}
	if migratedChatState != canonicalChatState {
		t.Fatalf("chat wakeup fingerprint identity was not migrated: got %q want %q", migratedChatState, canonicalChatState)
	}
	var wantChat any
	if err := json.Unmarshal(chatCondition, &wantChat); err != nil {
		t.Fatal(err)
	}
	var migratedChat map[string]any
	if err := json.Unmarshal([]byte(migratedCondition), &migratedChat); err != nil {
		t.Fatal(err)
	}
	migratedGitLab := migratedChat["gitlab"].(map[string]any)
	wantGitLab := wantChat.(map[string]any)["gitlab"].(map[string]any)
	if migratedChat["connection_id"] == f.connection || migratedGitLab["source"] != "vcs_connection" ||
		migratedGitLab["merge_request_id"] != canonicalMRID || !jsonEqual(migratedChat["type"], "pull_request") {
		t.Fatalf("chat wakeup migration lost target: %s", migratedCondition)
	}
	for _, key := range []string{"activated_at", "baseline_available", "baseline_head_sha", "baseline_pipelines"} {
		if !jsonEqual(migratedGitLab[key], wantGitLab[key]) {
			t.Fatalf("chat wakeup migration changed baseline field %q: got %#v want %#v", key, migratedGitLab[key], wantGitLab[key])
		}
	}
	var issueWakeupCondition, conditionState string
	if err := f.pool.QueryRow(context.Background(), `SELECT condition::text,condition_state FROM issue_wakeup WHERE id=$1`, issueWakeupID).Scan(&issueWakeupCondition, &conditionState); err != nil {
		t.Fatal(err)
	}
	if conditionState != "legacy-ci-baseline" || issueWakeupCondition == "" {
		t.Fatalf("issue wakeup baseline was not preserved: condition=%s state=%q", issueWakeupCondition, conditionState)
	}

	var snapshots int
	if err := f.pool.QueryRow(context.Background(), `SELECT count(*) FROM vcs_gitlab_migration_snapshot WHERE generation=1`).Scan(&snapshots); err != nil {
		t.Fatal(err)
	}
	if snapshots == 0 {
		t.Fatal("migration committed without durable before-images")
	}
	var beforeTitle, beforeState, beforeHead string
	var beforeUpdatedAt time.Time
	if err := f.pool.QueryRow(context.Background(), `SELECT title,state,head_sha,pr_updated_at FROM vcs_pull_request WHERE id=$1`, canonicalMRID).Scan(&beforeTitle, &beforeState, &beforeHead, &beforeUpdatedAt); err != nil {
		t.Fatal(err)
	}
	if _, err := f.pool.Exec(context.Background(), `UPDATE vcs_gitlab_migration_state SET phase='restoring',maintenance_mode=true WHERE id=1`); err != nil {
		t.Fatal(err)
	}
	if err := f.runner.backfillLegacyDetailIDs(context.Background()); err != nil {
		t.Fatalf("backfill legacy detail identities before restore: %v", err)
	}
	if err := f.runner.RestoreDatabase(context.Background()); err != nil {
		t.Fatalf("restore canonical database rows: %v", err)
	}
	if err := f.runner.RestoreDatabase(context.Background()); err != nil {
		t.Fatalf("retry database restore: %v", err)
	}
	var unrelatedDiscussionCount int
	if err := f.pool.QueryRow(context.Background(), `
		SELECT count(*) FROM gitlab_mr_discussion
		WHERE id=$1 AND workspace_id=$2 AND merge_request_id IS NULL AND vcs_pull_request_id=$3
		  AND gitlab_discussion_id='unrelated-workspace-discussion' AND resolved=true
		  AND discussion_updated_at=$4 AND updated_at=$4
	`, otherDiscussionID, otherWorkspaceID, otherPullRequestID, f.at).Scan(&unrelatedDiscussionCount); err != nil {
		t.Fatal(err)
	}
	if unrelatedDiscussionCount != 1 {
		t.Fatalf("database restore changed unrelated workspace discussion: count=%d", unrelatedDiscussionCount)
	}

	var restoredTitle, restoredState, restoredHead string
	var restoredUpdatedAt time.Time
	if err := f.pool.QueryRow(context.Background(), `SELECT title,state,head_sha,pr_updated_at FROM vcs_pull_request WHERE id=$1`, canonicalMRID).Scan(&restoredTitle, &restoredState, &restoredHead, &restoredUpdatedAt); err != nil {
		t.Fatal(err)
	}
	if restoredTitle != beforeTitle || restoredState != beforeState || restoredHead != beforeHead || !restoredUpdatedAt.Equal(beforeUpdatedAt) {
		t.Fatalf("database restore did not return canonical MR to before-image: got %q/%q/%q/%s want %q/%q/%q/%s", restoredTitle, restoredState, restoredHead, restoredUpdatedAt, beforeTitle, beforeState, beforeHead, beforeUpdatedAt)
	}
	var afterLinkCount, afterPipelineCount, afterDetailCount, afterProjectCount, afterMapCount int
	if err := f.pool.QueryRow(context.Background(), `
		SELECT count(*) FROM issue_vcs_pull_request
		WHERE issue_id=$1 AND pull_request_id=$2 AND close_intent=false AND vcs_linked=true
		  AND gitlab_linked_at IS NULL AND gitlab_close_intent=false
		  AND linked_by_type='member' AND linked_by_id=$3::uuid AND linked_at=$4
	`, f.issue, canonicalMRID, f.user, f.at.Add(5*time.Minute)).Scan(&afterLinkCount); err != nil {
		t.Fatal(err)
	}
	if err := f.pool.QueryRow(context.Background(), `SELECT count(*) FROM vcs_gitlab_pipeline WHERE pull_request_id=$1`, canonicalMRID).Scan(&afterPipelineCount); err != nil {
		t.Fatal(err)
	}
	if err := f.pool.QueryRow(context.Background(), `SELECT count(*) FROM vcs_gitlab_mr_detail WHERE vcs_pull_request_id=$1`, canonicalMRID).Scan(&afterDetailCount); err != nil {
		t.Fatal(err)
	}
	if err := f.pool.QueryRow(context.Background(), `SELECT count(*) FROM vcs_gitlab_project WHERE workspace_id=$1`, f.workspace).Scan(&afterProjectCount); err != nil {
		t.Fatal(err)
	}
	if err := f.pool.QueryRow(context.Background(), `SELECT count(*) FROM vcs_gitlab_migration_id_map WHERE workspace_id=$1`, f.workspace).Scan(&afterMapCount); err != nil {
		t.Fatal(err)
	}
	if afterLinkCount != 1 || afterPipelineCount != 1 || afterDetailCount != 1 || afterProjectCount != 1 || afterMapCount != 0 {
		t.Fatalf("restore changed pre-existing rows or retained mappings: link=%d pipeline=%d detail=%d project=%d mapping=%d", afterLinkCount, afterPipelineCount, afterDetailCount, afterProjectCount, afterMapCount)
	}
	var restoredProjectPolicy, restoredProjectHookEnabled bool
	var restoredProjectHookID int64
	var restoredProjectSyncError, restoredProjectURL string
	if err := f.pool.QueryRow(context.Background(), `
		SELECT legacy_link_policy,hook_id,hook_enabled,last_sync_error,web_url FROM vcs_gitlab_project WHERE id=$1
	`, canonicalProjectID).Scan(&restoredProjectPolicy, &restoredProjectHookID, &restoredProjectHookEnabled, &restoredProjectSyncError, &restoredProjectURL); err != nil {
		t.Fatal(err)
	}
	if restoredProjectPolicy || restoredProjectHookID != 991 || !restoredProjectHookEnabled ||
		restoredProjectSyncError != "canonical sync error" || restoredProjectURL != "https://canonical.example.test/group/sub/project" {
		t.Fatalf("project restore lost canonical before-image: policy=%v hook=%d/%v sync=%q URL=%q",
			restoredProjectPolicy, restoredProjectHookID, restoredProjectHookEnabled, restoredProjectSyncError, restoredProjectURL)
	}
	var restoredPipelineSHA, restoredPipelineRef, restoredPipelineStatus string
	var restoredPipelineVersion time.Time
	if err := f.pool.QueryRow(context.Background(), `
		SELECT sha,ref,status,pipeline_updated_at FROM vcs_gitlab_pipeline WHERE pull_request_id=$1 AND pipeline_id=901
	`, canonicalMRID).Scan(&restoredPipelineSHA, &restoredPipelineRef, &restoredPipelineStatus, &restoredPipelineVersion); err != nil {
		t.Fatal(err)
	}
	if restoredPipelineSHA != "canonical-pipeline-head" || restoredPipelineRef != "canonical-ref" ||
		restoredPipelineStatus != "passed" || !restoredPipelineVersion.Equal(f.at.Add(time.Minute)) {
		t.Fatalf("pipeline restore lost canonical before-image: %q %q %q %s", restoredPipelineSHA, restoredPipelineRef, restoredPipelineStatus, restoredPipelineVersion)
	}
	var restoredDetailDescription, restoredDetailTarget string
	if err := f.pool.QueryRow(context.Background(), `SELECT description,target_branch FROM vcs_gitlab_mr_detail WHERE vcs_pull_request_id=$1`, canonicalMRID).Scan(&restoredDetailDescription, &restoredDetailTarget); err != nil {
		t.Fatal(err)
	}
	if restoredDetailDescription != "canonical old description" || restoredDetailTarget != "canonical-target" {
		t.Fatalf("detail restore lost canonical before-image: description=%q target=%q", restoredDetailDescription, restoredDetailTarget)
	}
	var restoredSyncRefreshAt, restoredSyncPipelineAt pgtype.Timestamptz
	var restoredSyncRefreshError, restoredSyncPipelineError pgtype.Text
	if err := f.pool.QueryRow(context.Background(), `
		SELECT last_refreshed_at,last_refresh_error,pipeline_sync_at,pipeline_sync_error
		FROM vcs_gitlab_mr_detail WHERE vcs_pull_request_id=$1
	`, canonicalMRID).Scan(&restoredSyncRefreshAt, &restoredSyncRefreshError, &restoredSyncPipelineAt, &restoredSyncPipelineError); err != nil {
		t.Fatal(err)
	}
	if restoredSyncRefreshAt.Valid || !restoredSyncRefreshError.Valid || restoredSyncRefreshError.String != "canonical refresh error" ||
		!restoredSyncPipelineAt.Valid || !restoredSyncPipelineAt.Time.Equal(f.at.Add(5*time.Minute)) ||
		!restoredSyncPipelineError.Valid || restoredSyncPipelineError.String != "canonical pipeline error" {
		t.Fatalf("sync restore lost canonical timestamp/error pair: refresh=(%v,%v) pipeline=(%v,%v)",
			restoredSyncRefreshAt, restoredSyncRefreshError, restoredSyncPipelineAt, restoredSyncPipelineError)
	}
	for _, table := range []string{"gitlab_mr_approval_state", "gitlab_mr_discussion", "gitlab_mr_note", "gitlab_pipeline_job"} {
		var got int
		if err := f.pool.QueryRow(context.Background(), "SELECT count(*) FROM "+table+" WHERE merge_request_id=$1 AND vcs_pull_request_id IS NULL", f.mergeReq).Scan(&got); err != nil {
			t.Fatal(err)
		}
		if got != 1 {
			t.Fatalf("restore did not return %s row to legacy identity: count=%d", table, got)
		}
	}
	var restoredDiscussionResolved, restoredLegacyDiscussionResolved bool
	if err := f.pool.QueryRow(context.Background(), `SELECT resolved FROM gitlab_mr_discussion WHERE id=$1`, canonicalDiscussionID).Scan(&restoredDiscussionResolved); err != nil {
		t.Fatal(err)
	}
	if err := f.pool.QueryRow(context.Background(), `SELECT resolved FROM gitlab_mr_discussion WHERE id=$1`, discussionID).Scan(&restoredLegacyDiscussionResolved); err != nil {
		t.Fatal(err)
	}
	var restoredCanonicalNoteBody, restoredLegacyNoteBody, restoredCanonicalNoteDiscussion, restoredLegacyNoteDiscussion string
	if err := f.pool.QueryRow(context.Background(), `SELECT body,discussion_id::text FROM gitlab_mr_note WHERE id=$1`, canonicalNoteID).Scan(&restoredCanonicalNoteBody, &restoredCanonicalNoteDiscussion); err != nil {
		t.Fatal(err)
	}
	if err := f.pool.QueryRow(context.Background(), `SELECT body,discussion_id::text FROM gitlab_mr_note WHERE id=$1`, noteID).Scan(&restoredLegacyNoteBody, &restoredLegacyNoteDiscussion); err != nil {
		t.Fatal(err)
	}
	var restoredCanonicalJobName, restoredCanonicalJobStatus, restoredLegacyJobName, restoredLegacyJobStatus string
	if err := f.pool.QueryRow(context.Background(), `SELECT name,status FROM gitlab_pipeline_job WHERE id=$1`, canonicalJobID).Scan(&restoredCanonicalJobName, &restoredCanonicalJobStatus); err != nil {
		t.Fatal(err)
	}
	if err := f.pool.QueryRow(context.Background(), `SELECT name,status FROM gitlab_pipeline_job WHERE id=$1`, jobID).Scan(&restoredLegacyJobName, &restoredLegacyJobStatus); err != nil {
		t.Fatal(err)
	}
	if !restoredDiscussionResolved || restoredLegacyDiscussionResolved ||
		restoredCanonicalNoteBody != "canonical older note" || restoredCanonicalNoteDiscussion != canonicalDiscussionID ||
		restoredLegacyNoteBody != "legacy newer note" || restoredLegacyNoteDiscussion != discussionID ||
		restoredCanonicalJobName != "canonical older job" || restoredCanonicalJobStatus != "failed" ||
		restoredLegacyJobName != "legacy newer job" || restoredLegacyJobStatus != "success" {
		t.Fatalf("rollback did not restore both sides of duplicate details: discussions=%v/%v notes=%q/%q jobs=%q/%q/%q/%q",
			restoredDiscussionResolved, restoredLegacyDiscussionResolved, restoredCanonicalNoteBody, restoredLegacyNoteBody,
			restoredCanonicalJobName, restoredCanonicalJobStatus, restoredLegacyJobName, restoredLegacyJobStatus)
	}
	var restoredCondition, restoredChatState string
	if err := f.pool.QueryRow(context.Background(), `SELECT condition::text,condition_state FROM chat_wakeup WHERE id=$1`, chatWakeupID).Scan(&restoredCondition, &restoredChatState); err != nil {
		t.Fatal(err)
	}
	if restoredChatState != legacyChatState {
		t.Fatalf("restore changed legacy chat wakeup fingerprint: got %q want %q", restoredChatState, legacyChatState)
	}
	var restoredJSON, originalJSON any
	if err := json.Unmarshal([]byte(restoredCondition), &restoredJSON); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(chatCondition, &originalJSON); err != nil {
		t.Fatal(err)
	}
	if !jsonEqual(restoredJSON, originalJSON) {
		t.Fatalf("restore changed original chat wakeup baseline: got %s want %s", restoredCondition, chatCondition)
	}
	var restoredIssueCondition, restoredIssueState string
	if err := f.pool.QueryRow(context.Background(), `SELECT condition::text,condition_state FROM issue_wakeup WHERE id=$1`, issueWakeupID).Scan(&restoredIssueCondition, &restoredIssueState); err != nil {
		t.Fatal(err)
	}
	var restoredIssueJSON, originalIssueJSON any
	if err := json.Unmarshal([]byte(restoredIssueCondition), &restoredIssueJSON); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal([]byte(issueCondition), &originalIssueJSON); err != nil {
		t.Fatal(err)
	}
	if !jsonEqual(restoredIssueJSON, originalIssueJSON) || restoredIssueState != "legacy-ci-baseline" {
		t.Fatalf("restore changed issue wakeup target/baseline: condition=%s state=%q", restoredIssueCondition, restoredIssueState)
	}
	var restoredTokenCipher, restoredHookCipher string
	if err := f.pool.QueryRow(context.Background(), `SELECT access_token_encrypted,webhook_secret_encrypted FROM vcs_connection WHERE id=$1`, canonicalConnectionID).Scan(&restoredTokenCipher, &restoredHookCipher); err != nil {
		t.Fatal(err)
	}
	if restoredTokenCipher != tokenCipher || restoredHookCipher != hookCipher {
		t.Fatal("restore changed pre-existing encrypted VCS connection credentials")
	}
}

func TestDatabaseMigrationConflictRollsBackTransaction(t *testing.T) {
	f := newDatabaseMigrationFixture(t)
	f.seedLegacyProjectAndMR(t)
	canonicalConnectionID, canonicalMRID := uuid.NewString(), uuid.NewString()
	f.seedCanonicalConnection(t, canonicalConnectionID)
	f.seedCanonicalMR(t, canonicalConnectionID, canonicalMRID, "different same-version title", "open", "different-head", f.at)

	if err := f.runner.Migrate(context.Background()); err == nil {
		t.Fatal("same-version conflicting MR snapshots unexpectedly migrated")
	}
	for _, query := range []string{
		`SELECT count(*) FROM vcs_gitlab_project WHERE workspace_id=$1`,
		`SELECT count(*) FROM vcs_gitlab_migration_id_map WHERE workspace_id=$1`,
		`SELECT count(*) FROM vcs_gitlab_migration_snapshot WHERE generation=1 AND workspace_id=$1`,
	} {
		var count int
		if err := f.pool.QueryRow(context.Background(), query, f.workspace).Scan(&count); err != nil {
			t.Fatal(err)
		}
		if count != 0 {
			t.Fatalf("failed transaction left %d rows for query %q", count, query)
		}
	}
	var title string
	if err := f.pool.QueryRow(context.Background(), `SELECT title FROM vcs_pull_request WHERE id=$1`, canonicalMRID).Scan(&title); err != nil {
		t.Fatal(err)
	}
	if title != "different same-version title" {
		t.Fatalf("failed migration changed pre-existing canonical MR: %q", title)
	}
	var phase string
	if err := f.pool.QueryRow(context.Background(), `SELECT phase FROM vcs_gitlab_migration_state WHERE id=1`).Scan(&phase); err != nil {
		t.Fatal(err)
	}
	if phase != "maintenance" {
		t.Fatalf("failed migration changed maintenance state to %q", phase)
	}
}
func TestDatabaseMigrationScopesSameMRIdentityToWorkspace(t *testing.T) {
	f := newDatabaseMigrationFixture(t)
	f.seedLegacyProjectAndMR(t)
	seed := testutil.New(f.pool, "", "")
	workspace2 := seed.Workspace(t, "Another GitLab workspace", "gitlab-migration-"+uuid.NewString())
	user2 := seed.User(t, "Another migration user", "migration-"+uuid.NewString()+"@example.test")
	second := &databaseMigrationFixture{
		pool: f.pool, fx: testutil.New(f.pool, workspace2, user2), runner: f.runner, env: f.env, server: f.server,
		workspace: workspace2, user: user2, connection: uuid.NewString(), binding: uuid.NewString(),
		mergeReq: uuid.NewString(), at: f.at,
	}
	second.seedLegacyProjectAndMR(t)

	if err := f.runner.Migrate(context.Background()); err != nil {
		t.Fatalf("migrate same GitLab MR identity in two workspaces: %v", err)
	}
	var count int
	if err := f.pool.QueryRow(context.Background(), `
		SELECT count(*) FROM vcs_pull_request
		WHERE provider='gitlab' AND repo_owner='group/sub' AND repo_name='project' AND pr_number=81
	`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 2 {
		t.Fatalf("canonical MR count for same path/number in different workspaces = %d, want 2", count)
	}
	for _, source := range []struct {
		workspace, connection, mr string
	}{{f.workspace, f.connection, f.mergeReq}, {second.workspace, second.connection, second.mergeReq}} {
		var mappedConnection, mappedMR string
		if err := f.pool.QueryRow(context.Background(), `
			SELECT unified_id::text FROM vcs_gitlab_migration_id_map
			WHERE workspace_id=$1 AND entity_kind='connection' AND legacy_id=$2
		`, source.workspace, source.connection).Scan(&mappedConnection); err != nil {
			t.Fatal(err)
		}
		if err := f.pool.QueryRow(context.Background(), `
			SELECT unified_id::text FROM vcs_gitlab_migration_id_map
			WHERE workspace_id=$1 AND entity_kind='pull_request' AND legacy_id=$2
		`, source.workspace, source.mr).Scan(&mappedMR); err != nil {
			t.Fatal(err)
		}
		var workspaceID, connectionID string
		if err := f.pool.QueryRow(context.Background(), `SELECT workspace_id::text,connection_id::text FROM vcs_pull_request WHERE id=$1`, mappedMR).Scan(&workspaceID, &connectionID); err != nil {
			t.Fatal(err)
		}
		if workspaceID != source.workspace || connectionID != mappedConnection {
			t.Fatalf("workspace %s legacy MR mapped into another scope: row workspace=%s connection=%s want connection=%s", source.workspace, workspaceID, connectionID, mappedConnection)
		}
	}
	box, err := f.env.box()
	if err != nil {
		t.Fatal(err)
	}
	for _, source := range []struct{ workspace, connection string }{{f.workspace, f.connection}, {second.workspace, second.connection}} {
		var ciphertext, hookCiphertext string
		if err := f.pool.QueryRow(context.Background(), `
			SELECT access_token_encrypted,webhook_secret_encrypted FROM vcs_connection WHERE workspace_id=$1 AND id=$2
		`, source.workspace, source.connection).Scan(&ciphertext, &hookCiphertext); err != nil {
			t.Fatal(err)
		}
		token, err := openSecret(box, ciphertext)
		if err != nil {
			t.Fatal(err)
		}
		secret, err := openSecret(box, hookCiphertext)
		if err != nil {
			t.Fatal(err)
		}
		if token != f.env.GitLabToken || secret != f.env.GitLabWebhookSecret {
			t.Fatalf("workspace %s migrated credentials did not preserve original secret values", source.workspace)
		}
		var legacyTokenCipher, legacyHookCipher string
		if err := f.pool.QueryRow(context.Background(), `
			SELECT access_token_encrypted,webhook_secret_encrypted FROM gitlab_connection
			WHERE workspace_id=$1 AND id=$2
		`, source.workspace, source.connection).Scan(&legacyTokenCipher, &legacyHookCipher); err != nil {
			t.Fatal(err)
		}
		if ciphertext != legacyTokenCipher || hookCiphertext != legacyHookCipher {
			t.Fatalf("workspace %s canonical connection did not preserve original credential ciphertext", source.workspace)
		}
	}
}

func TestDatabaseMigrationAmbiguousGitLabLinkRollsBackTransaction(t *testing.T) {
	f := newDatabaseMigrationFixture(t)
	f.seedLegacyProjectAndMR(t)
	canonicalConnectionID, canonicalMRID := uuid.NewString(), uuid.NewString()
	f.seedCanonicalConnection(t, canonicalConnectionID)
	f.seedCanonicalMR(t, canonicalConnectionID, canonicalMRID, "canonical newer title", "open", "canonical-head", f.at.Add(time.Minute))
	existingGitLabOwner := uuid.NewString()
	if _, err := f.pool.Exec(context.Background(), `
		INSERT INTO issue_vcs_pull_request (
			issue_id,pull_request_id,close_intent,linked_by_type,linked_by_id,linked_at,
			vcs_linked,gitlab_linked_by_type,gitlab_linked_by_id,gitlab_linked_at
		) VALUES ($1,$2,false,NULL,NULL,$3,true,'member',$4,$3)
	`, f.issue, canonicalMRID, f.at, existingGitLabOwner); err != nil {
		t.Fatalf("insert existing overlapping GitLab link: %v", err)
	}
	if _, err := f.pool.Exec(context.Background(), `
		INSERT INTO issue_gitlab_merge_request (workspace_id,issue_id,merge_request_id,close_intent,linked_by_type,linked_by_id,linked_at)
		VALUES ($1,$2,$3,true,'member',$4,$5)
	`, f.workspace, f.issue, f.mergeReq, uuid.NewString(), f.at); err != nil {
		t.Fatalf("insert conflicting legacy GitLab link: %v", err)
	}

	if err := f.runner.Migrate(context.Background()); err == nil {
		t.Fatal("different same-source GitLab manual link owners unexpectedly migrated")
	}
	for _, query := range []string{
		`SELECT count(*) FROM vcs_gitlab_project WHERE workspace_id=$1`,
		`SELECT count(*) FROM vcs_gitlab_migration_id_map WHERE workspace_id=$1`,
		`SELECT count(*) FROM vcs_gitlab_migration_snapshot WHERE generation=1 AND workspace_id=$1`,
	} {
		var count int
		if err := f.pool.QueryRow(context.Background(), query, f.workspace).Scan(&count); err != nil {
			t.Fatal(err)
		}
		if count != 0 {
			t.Fatalf("ambiguous link failure left %d rows for query %q", count, query)
		}
	}
	var owner string
	var gitlabLinkedAt time.Time
	if err := f.pool.QueryRow(context.Background(), `
		SELECT gitlab_linked_by_id::text,gitlab_linked_at FROM issue_vcs_pull_request
		WHERE issue_id=$1 AND pull_request_id=$2
	`, f.issue, canonicalMRID).Scan(&owner, &gitlabLinkedAt); err != nil {
		t.Fatal(err)
	}
	if owner != existingGitLabOwner || !gitlabLinkedAt.Equal(f.at) {
		t.Fatalf("ambiguous migration altered canonical GitLab provenance: owner=%q time=%s", owner, gitlabLinkedAt)
	}
}
func TestDatabaseMigrationNoOpsAfterFinalizeWithoutLegacyTables(t *testing.T) {
	f := newDatabaseMigrationFixture(t)
	ctx := context.Background()
	if _, err := f.pool.Exec(ctx, `UPDATE vcs_gitlab_migration_state SET phase='complete',maintenance_mode=false WHERE id=1`); err != nil {
		t.Fatal(err)
	}
	if _, err := f.pool.Exec(ctx, `
		DROP TABLE IF EXISTS gitlab_mr_pipeline,issue_gitlab_merge_request,gitlab_merge_request,gitlab_project_binding,gitlab_connection CASCADE
	`); err != nil {
		t.Fatalf("simulate finalized legacy-table cleanup: %v", err)
	}
	var beforeGeneration int64
	if err := f.pool.QueryRow(ctx, `SELECT generation FROM vcs_gitlab_migration_state WHERE id=1`).Scan(&beforeGeneration); err != nil {
		t.Fatal(err)
	}
	if err := f.runner.Migrate(ctx); err != nil {
		t.Fatalf("repeat migration after finalization: %v", err)
	}
	var phase string
	var afterGeneration int64
	if err := f.pool.QueryRow(ctx, `SELECT phase,generation FROM vcs_gitlab_migration_state WHERE id=1`).Scan(&phase, &afterGeneration); err != nil {
		t.Fatal(err)
	}
	if phase != "complete" || afterGeneration != beforeGeneration {
		t.Fatalf("finalized no-op changed state: phase=%q generation=%d want %d", phase, afterGeneration, beforeGeneration)
	}
}

func TestDatabaseMigrationResolvesPipelineProjectIDFromCompleteURL(t *testing.T) {
	f := newDatabaseMigrationFixture(t)
	f.seedLegacyProjectAndMR(t)
	if _, err := f.pool.Exec(context.Background(), `
		INSERT INTO gitlab_mr_pipeline (
			merge_request_id,pipeline_id,pipeline_project_id,sha,ref,status,web_url,pipeline_updated_at
		) VALUES ($1,1901,0,'legacy-ci-head','refs/merge-requests/81/head','failed',
			$2,$3)
	`, f.mergeReq, f.server.URL+"/group/sub/project/-/pipelines/1901", f.at); err != nil {
		t.Fatalf("insert legacy pipeline without project ID: %v", err)
	}
	if err := f.runner.Migrate(context.Background()); err != nil {
		t.Fatalf("migrate legacy pipeline with a complete same-instance project URL: %v", err)
	}
	var pipelineProjectID int64
	var pipelineProjectPath string
	if err := f.pool.QueryRow(context.Background(), `
		SELECT pipeline_project_id,pipeline_project_path
		FROM vcs_gitlab_pipeline
		WHERE pipeline_id=1901 AND pipeline_project_path='group/sub/project'
	`).Scan(&pipelineProjectID, &pipelineProjectPath); err != nil {
		t.Fatalf("read migrated legacy pipeline identity: %v", err)
	}
	if pipelineProjectID != 77 || pipelineProjectPath != "group/sub/project" {
		t.Fatalf("migrated pipeline project=(%d,%q), want (77,%q)", pipelineProjectID, pipelineProjectPath, "group/sub/project")
	}
}

func TestDatabaseMigrationFailsClosedOnUnknownPipelineProject(t *testing.T) {
	f := newDatabaseMigrationFixture(t)
	f.seedLegacyProjectAndMR(t)
	if _, err := f.pool.Exec(context.Background(), `
		INSERT INTO gitlab_mr_pipeline (
			merge_request_id,pipeline_id,pipeline_project_id,sha,ref,status,web_url,pipeline_updated_at
		) VALUES ($1,1902,0,'unknown-ci-head','feature/not-the-authoritative-branch','failed',$2,$3)
	`, f.mergeReq, f.server.URL+"/group/unrelated/project/-/pipelines/1902", f.at); err != nil {
		t.Fatalf("insert pipeline with unknown project identity: %v", err)
	}
	if err := f.runner.Migrate(context.Background()); err == nil {
		t.Fatal("migration accepted an unprovable legacy pipeline project identity")
	}
	for _, query := range []string{
		`SELECT count(*) FROM vcs_connection WHERE workspace_id=$1`,
		`SELECT count(*) FROM vcs_gitlab_project WHERE workspace_id=$1`,
		`SELECT count(*) FROM vcs_pull_request WHERE workspace_id=$1`,
		`SELECT count(*) FROM vcs_gitlab_pipeline WHERE workspace_id=$1`,
		`SELECT count(*) FROM vcs_gitlab_migration_snapshot WHERE generation=1 AND workspace_id=$1`,
	} {
		var count int
		if err := f.pool.QueryRow(context.Background(), query, f.workspace).Scan(&count); err != nil {
			t.Fatal(err)
		}
		if count != 0 {
			t.Fatalf("failed pipeline identity left partial migration state: %s returned %d", query, count)
		}
	}
	var phase string
	var maintenance bool
	if err := f.pool.QueryRow(context.Background(), `SELECT phase,maintenance_mode FROM vcs_gitlab_migration_state WHERE id=1`).Scan(&phase, &maintenance); err != nil {
		t.Fatal(err)
	}
	if phase != "maintenance" || !maintenance {
		t.Fatalf("failed pipeline migration changed state: phase=%q maintenance=%v", phase, maintenance)
	}
}
