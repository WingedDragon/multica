package gitlabmigration

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/util/secretbox"
)

func TestRewriteChatWakeupConditionPreservesGitLabBaseline(t *testing.T) {
	oldConnection := "10000000-0000-0000-0000-000000000001"
	newConnection := "20000000-0000-0000-0000-000000000001"
	oldMR := "10000000-0000-0000-0000-000000000002"
	newMR := "20000000-0000-0000-0000-000000000002"
	condition := json.RawMessage(`{"type":"pull_request","repo":"group/sub/project","number":81,"event":"checks_finished","provider":"gitlab","connection_id":"` + oldConnection + `","gitlab":{"source":"gitlab_connection","merge_request_id":"` + oldMR + `","activated_at":"2026-11-23T10:00:00Z","baseline_available":true,"baseline_head_sha":"head-a","baseline_pipelines":[{"pipeline_id":"9","pipeline_project_id":"17","pipeline_project_path":"group/sub/project","ref":"refs/merge-requests/81/head","sha":"head-a","status":"failed","updated_at":"2026-11-23T09:59:00Z"}]}}`)

	got, changed, err := rewriteChatWakeupCondition(condition, idMappings{
		Connections:  map[string]string{oldConnection: newConnection},
		PullRequests: map[string]string{oldMR: newMR},
	})
	if err != nil {
		t.Fatalf("rewriteChatWakeupCondition() error = %v", err)
	}
	if !changed {
		t.Fatal("rewriteChatWakeupCondition() changed = false, want true")
	}
	var want, actual map[string]any
	if err := json.Unmarshal(condition, &want); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(got, &actual); err != nil {
		t.Fatal(err)
	}
	actualGitLab := actual["gitlab"].(map[string]any)
	wantGitLab := want["gitlab"].(map[string]any)
	if actual["connection_id"] != newConnection || actualGitLab["source"] != "vcs_connection" || actualGitLab["merge_request_id"] != newMR {
		t.Fatalf("mapped condition = %#v", actual)
	}
	for _, key := range []string{"activated_at", "baseline_available", "baseline_head_sha", "baseline_pipelines"} {
		if !jsonEqual(actualGitLab[key], wantGitLab[key]) {
			t.Errorf("GitLab baseline field %q changed: got %#v, want %#v", key, actualGitLab[key], wantGitLab[key])
		}
	}
	for _, key := range []string{"type", "repo", "number", "event", "provider"} {
		if actual[key] != want[key] {
			t.Errorf("condition field %q changed: got %#v, want %#v", key, actual[key], want[key])
		}
	}
}

func TestRewriteChatWakeupConditionRejectsMissingLegacyMapping(t *testing.T) {
	condition := json.RawMessage(`{"type":"pull_request","repo":"group/project","number":1,"event":"merged","provider":"gitlab","connection_id":"10000000-0000-0000-0000-000000000001","gitlab":{"source":"gitlab_connection","merge_request_id":"10000000-0000-0000-0000-000000000002"}}`)
	if _, _, err := rewriteChatWakeupCondition(condition, idMappings{}); err == nil {
		t.Fatal("rewriteChatWakeupCondition() error = nil, want unresolved legacy IDs to fail closed")
	}
}

func TestMergeIssueLinkRetainsIndependentGitLabAndGenericProvenance(t *testing.T) {
	at := time.Date(2026, 11, 23, 10, 0, 0, 0, time.UTC)
	got, err := mergeIssueLink(issueLinkState{
		LinkedByType: "member", LinkedByID: "member-a", LinkedAt: at.Add(time.Minute),
		CloseIntent: true, ReferenceOnly: true,
	}, issueLinkState{
		GitLabLinkedByType: "system", GitLabLinkedByID: "gitlab-user", GitLabLinkedAt: pgtype.Timestamptz{Time: at, Valid: true},
		GitLabCloseIntent: true,
	})
	if err != nil {
		t.Fatalf("mergeIssueLink() error = %v", err)
	}
	if !got.CloseIntent || !got.GitLabCloseIntent || !got.ReferenceOnly ||
		got.LinkedByType != "member" || got.LinkedByID != "member-a" || !got.LinkedAt.Equal(at.Add(time.Minute)) ||
		got.GitLabLinkedByType != "system" || got.GitLabLinkedByID != "gitlab-user" || !got.GitLabLinkedAt.Valid || !got.GitLabLinkedAt.Time.Equal(at) {
		t.Fatalf("merged issue link = %#v", got)
	}

}
func TestMergeIssueLinkRejectsConflictingManualOwners(t *testing.T) {
	_, err := mergeIssueLink(issueLinkState{GitLabLinkedByType: "member", GitLabLinkedByID: "member-a"}, issueLinkState{GitLabLinkedByType: "member", GitLabLinkedByID: "member-b"})
	if !errors.Is(err, errAmbiguousLink) {
		t.Fatalf("mergeIssueLink() error = %v, want %v", err, errAmbiguousLink)
	}
}

func TestMergeMRSnapshotUsesNewestStateAndRejectsEqualTimestampConflict(t *testing.T) {
	at := time.Date(2026, 11, 23, 10, 0, 0, 0, time.UTC)
	older := mrSnapshot{UpdatedAt: at, State: "open", HeadSHA: "old-head", Title: "old"}
	newer := mrSnapshot{UpdatedAt: at.Add(time.Second), State: "merged", HeadSHA: "new-head", Title: "new", TargetBranch: "main"}
	got, err := mergeMRSnapshot(older, newer)
	if err != nil {
		t.Fatalf("mergeMRSnapshot() error = %v", err)
	}
	if got.State != "merged" || got.HeadSHA != "new-head" || got.Title != "new" || got.TargetBranch != "main" {
		t.Fatalf("merged MR snapshot = %#v", got)
	}
	_, err = mergeMRSnapshot(older, mrSnapshot{UpdatedAt: at, State: "closed", HeadSHA: "other-head"})
	if !errors.Is(err, errAmbiguousMR) {
		t.Fatalf("equal-timestamp merge error = %v, want %v", err, errAmbiguousMR)
	}
}

func TestReverseChatWakeupRewritePreservesBaseline(t *testing.T) {
	newConnection := "20000000-0000-0000-0000-000000000001"
	oldConnection := "10000000-0000-0000-0000-000000000001"
	newMR := "20000000-0000-0000-0000-000000000002"
	oldMR := "10000000-0000-0000-0000-000000000002"
	condition := json.RawMessage(`{"type":"pull_request","provider":"gitlab","connection_id":"` + newConnection + `","gitlab":{"source":"vcs_connection","merge_request_id":"` + newMR + `","baseline_head_sha":"head-a","baseline_pipelines":[{"pipeline_id":"9","status":"failed"}]}}`)
	got, changed, err := rewriteChatWakeupConditionToLegacy(condition, idMappings{
		Connections:  map[string]string{newConnection: oldConnection},
		PullRequests: map[string]string{newMR: oldMR},
	})
	if err != nil {
		t.Fatalf("rewriteChatWakeupConditionToLegacy() error = %v", err)
	}
	if !changed {
		t.Fatal("rewriteChatWakeupConditionToLegacy() changed = false, want true")
	}
	var actual map[string]any
	if err := json.Unmarshal(got, &actual); err != nil {
		t.Fatal(err)
	}
	gitlabState := actual["gitlab"].(map[string]any)
	if actual["connection_id"] != oldConnection || gitlabState["source"] != "gitlab_connection" || gitlabState["merge_request_id"] != oldMR {
		t.Fatalf("restored condition = %#v", actual)
	}
	if gitlabState["baseline_head_sha"] != "head-a" || !jsonEqual(gitlabState["baseline_pipelines"], []any{map[string]any{"pipeline_id": "9", "status": "failed"}}) {
		t.Fatalf("restored baseline changed: %#v", gitlabState)
	}
}
func TestLegacyCredentialsPreferPerConnectionCiphertextAndSealFallback(t *testing.T) {
	key := make([]byte, secretbox.KeySize)
	key[0] = 19
	box, err := secretbox.New(key)
	if err != nil {
		t.Fatal(err)
	}
	encryptedToken, err := sealSecret(box, "legacy-api-token")
	if err != nil {
		t.Fatal(err)
	}
	encryptedSecret, err := sealSecret(box, "legacy-hook-secret")
	if err != nil {
		t.Fatal(err)
	}
	env := Environment{
		GitLabBaseURL:       "https://another.example",
		GitLabToken:         "wrong-token",
		GitLabWebhookSecret: "wrong-secret",
	}
	token, secret, saved, err := env.legacyCredentials(box, "https://gitlab.example", encryptedToken, encryptedSecret)
	if err != nil {
		t.Fatalf("read encrypted legacy credentials: %v", err)
	}
	if token != "legacy-api-token" || secret != "legacy-hook-secret" || saved != encryptedSecret {
		t.Fatalf("encrypted legacy credentials = (%q,%q,%q)", token, secret, saved)
	}
	fallback := Environment{
		GitLabBaseURL:       "https://gitlab.example/",
		GitLabToken:         "environment-token",
		GitLabWebhookSecret: "environment-secret",
	}
	token, secret, saved, err = fallback.legacyCredentials(box, "https://gitlab.example", "", "")
	if err != nil {
		t.Fatalf("use exact-instance environment fallback: %v", err)
	}
	if token != fallback.GitLabToken || secret != fallback.GitLabWebhookSecret {
		t.Fatalf("fallback credentials = (%q,%q)", token, secret)
	}
	decrypted, err := openSecret(box, saved)
	if err != nil || decrypted != fallback.GitLabWebhookSecret {
		t.Fatalf("encrypted fallback secret decrypts to %q, err=%v", decrypted, err)
	}
	fallback.GitLabBaseURL = "https://wrong.example"
	if _, _, _, err := fallback.legacyCredentials(box, "https://gitlab.example", "", ""); err == nil {
		t.Fatal("accepted environment credentials for a different GitLab instance")
	}
}
func TestHookRollbackDeletesOnlyToolCreatedHooks(t *testing.T) {
	cases := []struct {
		name string
		hook migrationHook
		want bool
	}{
		{name: "tool-created canonical Hook", hook: migrationHook{CreatedByTool: true}, want: true},
		{name: "pre-existing manual canonical Hook", hook: migrationHook{CreatedByTool: false}, want: false},
		{name: "modified legacy Hook", hook: migrationHook{LegacyID: 42, CreatedByTool: false}, want: false},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			if got := shouldDeleteHookOnRestore(test.hook); got != test.want {
				t.Fatalf("shouldDeleteHookOnRestore(%#v) = %v, want %v", test.hook, got, test.want)
			}
		})
	}
}

func TestManagedProjectHookCreationIntentClaimsRemoteHookAfterLedgerFailure(t *testing.T) {
	ctx, pool := newIsolatedMigrationTestPool(t)
	const connectionID = "30000000-0000-0000-0000-000000000001"
	const workspaceID = "40000000-0000-0000-0000-000000000001"
	if _, err := pool.Exec(ctx, `
		CREATE TABLE vcs_gitlab_migration_hook (
			connection_id UUID NOT NULL, workspace_id UUID NOT NULL, scope_type TEXT NOT NULL,
			scope_id TEXT NOT NULL, scope_path TEXT NOT NULL, legacy_hook_id BIGINT NOT NULL,
			new_hook_id BIGINT, managed BOOLEAN NOT NULL, created_by_tool BOOLEAN NOT NULL DEFAULT FALSE,
			previous_config JSONB NOT NULL DEFAULT '{}'::jsonb, target_url TEXT NOT NULL DEFAULT '',
			state TEXT NOT NULL, last_error TEXT, updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
			PRIMARY KEY (connection_id, scope_type, scope_id, legacy_hook_id)
		);
		CREATE TABLE vcs_gitlab_project (
			workspace_id UUID NOT NULL, connection_id UUID NOT NULL, gitlab_project_id BIGINT NOT NULL,
			hook_id BIGINT, hook_enabled BOOLEAN NOT NULL DEFAULT FALSE, last_sync_error TEXT,
			updated_at TIMESTAMPTZ NOT NULL DEFAULT now(), PRIMARY KEY (workspace_id,connection_id,gitlab_project_id)
		);
		CREATE FUNCTION fail_hook_id_claim() RETURNS trigger LANGUAGE plpgsql AS $$
		BEGIN
			IF OLD.new_hook_id IS NULL AND NEW.new_hook_id IS NOT NULL THEN
				RAISE EXCEPTION 'simulated migration-ledger write failure';
			END IF;
			RETURN NEW;
		END $$;
		CREATE TRIGGER fail_hook_id_claim BEFORE UPDATE ON vcs_gitlab_migration_hook
		FOR EACH ROW EXECUTE FUNCTION fail_hook_id_claim();
	`); err != nil {
		t.Fatalf("create Hook retry fixture: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO vcs_gitlab_migration_hook
			(connection_id,workspace_id,scope_type,scope_id,scope_path,legacy_hook_id,managed,state)
		VALUES ($1,$2,'project','123','group/project',0,TRUE,'pending')
	`, connectionID, workspaceID); err != nil {
		t.Fatalf("seed Hook creation intent row: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO vcs_gitlab_project (workspace_id,connection_id,gitlab_project_id)
		VALUES ($1,$2,123)
	`, workspaceID, connectionID); err != nil {
		t.Fatalf("seed canonical project: %v", err)
	}
	const publicURL = "https://hooks.example.test"
	placeholder, err := manualHookURL(publicURL, connectionID, "project", "123", 0)
	if err != nil {
		t.Fatal(err)
	}
	var remoteHooks []map[string]any
	var createCalls, updateCalls int
	var intentObservedBeforeCreate bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api/v4/projects/123/hooks":
			_ = json.NewEncoder(w).Encode(remoteHooks)
		case r.Method == http.MethodPost && r.URL.Path == "/api/v4/projects/123/hooks":
			createCalls++
			var body map[string]any
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				http.Error(w, "invalid JSON", http.StatusBadRequest)
				return
			}
			var target string
			var createdByTool, noID bool
			err := pool.QueryRow(r.Context(), `
				SELECT target_url,created_by_tool,new_hook_id IS NULL
				FROM vcs_gitlab_migration_hook
				WHERE connection_id=$1 AND scope_type='project' AND scope_id='123' AND legacy_hook_id=0
			`, connectionID).Scan(&target, &createdByTool, &noID)
			intentObservedBeforeCreate = err == nil && target == placeholder && createdByTool && noID
			hook := map[string]any{
				"id": float64(77), "url": body["url"], "merge_requests_events": true,
				"pipeline_events": true, "job_events": true, "note_events": true,
			}
			remoteHooks = append(remoteHooks, hook)
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(hook)
		case r.Method == http.MethodPut && r.URL.Path == "/api/v4/projects/123/hooks/77":
			updateCalls++
			var body map[string]any
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				http.Error(w, "invalid JSON", http.StatusBadRequest)
				return
			}
			for key, value := range body {
				remoteHooks[0][key] = value
			}
			_ = json.NewEncoder(w).Encode(remoteHooks[0])
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	api, err := newGitLabAPI(server.URL, "api-token", "")
	if err != nil {
		t.Fatal(err)
	}
	runner := NewRunner(pool, Environment{PublicURL: publicURL})
	connection := migrationConnection{ID: connectionID, WorkspaceID: workspaceID, WebhookSecret: "canonical-secret", API: api}
	hook := migrationHook{ConnectionID: connectionID, WorkspaceID: workspaceID, ScopeType: "project", ScopeID: "123", ScopePath: "group/project", Managed: true}
	if err := runner.createManagedProjectHook(ctx, connection, hook); err == nil {
		t.Fatal("expected the simulated ledger write to fail after remote creation")
	}
	if !intentObservedBeforeCreate || createCalls != 1 || len(remoteHooks) != 1 {
		t.Fatalf("creation intent/retry evidence = intent %v, POSTs %d, remote Hooks %d", intentObservedBeforeCreate, createCalls, len(remoteHooks))
	}
	var createdByTool, noID bool
	var savedTarget, savedState string
	if err := pool.QueryRow(ctx, `
		SELECT created_by_tool,new_hook_id IS NULL,target_url,state
		FROM vcs_gitlab_migration_hook
		WHERE connection_id=$1 AND scope_type='project' AND scope_id='123' AND legacy_hook_id=0
	`, connectionID).Scan(&createdByTool, &noID, &savedTarget, &savedState); err != nil {
		t.Fatal(err)
	}
	if !createdByTool || !noID || savedTarget != placeholder || savedState != "pending" {
		t.Fatalf("durable creation intent = (%v,%v,%q,%q)", createdByTool, noID, savedTarget, savedState)
	}
	if _, err := pool.Exec(ctx, `DROP TRIGGER fail_hook_id_claim ON vcs_gitlab_migration_hook; DROP FUNCTION fail_hook_id_claim()`); err != nil {
		t.Fatalf("remove simulated ledger failure: %v", err)
	}
	if err := runner.createManagedProjectHook(ctx, connection, hook); err != nil {
		t.Fatalf("retry and claim existing remote Hook: %v", err)
	}
	target, err := manualHookURL(publicURL, connectionID, "project", "123", 77)
	if err != nil {
		t.Fatal(err)
	}
	if createCalls != 1 || updateCalls != 1 || len(remoteHooks) != 1 || !hookURLMatches(hookURL(remoteHooks[0]), target) {
		t.Fatalf("retry duplicated or failed to finish Hook: POSTs %d, PUTs %d, remote %#v", createCalls, updateCalls, remoteHooks)
	}
	var id pgtype.Int8
	if err := pool.QueryRow(ctx, `
		SELECT new_hook_id,created_by_tool,target_url,state
		FROM vcs_gitlab_migration_hook
		WHERE connection_id=$1 AND scope_type='project' AND scope_id='123' AND legacy_hook_id=0
	`, connectionID).Scan(&id, &createdByTool, &savedTarget, &savedState); err != nil {
		t.Fatal(err)
	}
	if !id.Valid || id.Int64 != 77 || !createdByTool || savedTarget != target || savedState != "updated" {
		t.Fatalf("reconciled Hook ledger = (%v,%v,%q,%q)", id, createdByTool, savedTarget, savedState)
	}
	var projectHookID int64
	if err := pool.QueryRow(ctx, `SELECT hook_id FROM vcs_gitlab_project WHERE workspace_id=$1 AND connection_id=$2 AND gitlab_project_id=123`, workspaceID, connectionID).Scan(&projectHookID); err != nil {
		t.Fatal(err)
	}
	if projectHookID != 77 {
		t.Fatalf("canonical project Hook ID = %d, want 77", projectHookID)
	}
}

func TestGroupHooksUnsupportedByMetadataRequiresExplicitPlanEvidence(t *testing.T) {
	cases := []struct {
		name        string
		status      int
		body        string
		unsupported bool
	}{
		{name: "free plan", status: http.StatusOK, body: `{"plan":"free"}`, unsupported: true},
		{name: "community edition", status: http.StatusOK, body: `{"enterprise":false}`, unsupported: true},
		{name: "premium plan", status: http.StatusOK, body: `{"plan":"premium","enterprise":true}`},
		{name: "permission denied", status: http.StatusForbidden},
		{name: "metadata unavailable", status: http.StatusNotFound},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/api/v4/metadata" {
					http.NotFound(w, r)
					return
				}
				w.WriteHeader(tc.status)
				if tc.body != "" {
					_, _ = w.Write([]byte(tc.body))
				}
			}))
			defer server.Close()
			api, err := newGitLabAPI(server.URL, "token", "")
			if err != nil {
				t.Fatal(err)
			}
			if got := api.groupHooksUnsupportedByMetadata(context.Background()); got != tc.unsupported {
				t.Fatalf("groupHooksUnsupportedByMetadata() = %v, want %v", got, tc.unsupported)
			}
		})
	}
}

func TestGroupHookNotFoundRequiresConfirmedOwnerOrAdmin(t *testing.T) {
	cases := []struct {
		name            string
		hookStatus      int
		memberStatus    int
		accessLevel     int
		isAdmin         bool
		wantUnsupported bool
		wantAccessError bool
	}{
		{name: "confirmed owner", hookStatus: http.StatusNotFound, memberStatus: http.StatusOK, accessLevel: 50, wantUnsupported: true},
		{name: "confirmed non-owner", hookStatus: http.StatusNotFound, memberStatus: http.StatusOK, accessLevel: 40, wantAccessError: true},
		{name: "membership cannot be confirmed", hookStatus: http.StatusNotFound, memberStatus: http.StatusForbidden, wantAccessError: true},
		{name: "administrator", hookStatus: http.StatusNotFound, isAdmin: true, wantUnsupported: true},
		{name: "forbidden Hook endpoint", hookStatus: http.StatusForbidden, memberStatus: http.StatusOK, accessLevel: 50},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/api/v4/groups/5800/hooks":
					w.WriteHeader(tc.hookStatus)
				case "/api/v4/user":
					_, _ = w.Write([]byte(`{"id":42,"username":"migration-user","is_admin":` + strconv.FormatBool(tc.isAdmin) + `}`))
				case "/api/v4/groups/5800/members/all/42":
					w.WriteHeader(tc.memberStatus)
					if tc.memberStatus == http.StatusOK {
						_ = json.NewEncoder(w).Encode(map[string]int{"access_level": tc.accessLevel})
					}
				default:
					http.NotFound(w, r)
				}
			}))
			defer server.Close()
			api, err := newGitLabAPI(server.URL, "token", "")
			if err != nil {
				t.Fatal(err)
			}
			_, hookErr := api.listHooks(context.Background(), "group", 5800)
			unsupported, accessErr := api.groupHooksUnsupportedForGroup(context.Background(), 5800, hookErr)
			if unsupported != tc.wantUnsupported || (accessErr != nil) != tc.wantAccessError {
				t.Fatalf("unsupported=%v accessErr=%v, want unsupported=%v accessError=%v", unsupported, accessErr, tc.wantUnsupported, tc.wantAccessError)
			}
			if tc.hookStatus == http.StatusForbidden && unsupported {
				t.Fatal("HTTP 403 must not prove that group hooks are unsupported")
			}
		})
	}
}
