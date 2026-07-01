package handler

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/events"
	"github.com/multica-ai/multica/server/internal/integrations/gitlab"
	"github.com/multica-ai/multica/server/internal/middleware"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/multica-ai/multica/server/pkg/protocol"
)

func TestGitLabWebhookRejectsBadToken(t *testing.T) {
	t.Setenv("GITLAB_BASE_URL", "https://code.mlamp.cn")
	t.Setenv("GITLAB_TOKEN", "token")
	t.Setenv("GITLAB_WEBHOOK_SECRET", "secret")

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/webhooks/gitlab", bytes.NewReader([]byte(`{}`)))
	req.Header.Set("X-Gitlab-Token", "wrong")
	req.Header.Set("X-Gitlab-Event", "Merge Request Hook")

	testHandler.HandleGitLabWebhook(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d (%s)", rec.Code, rec.Body.String())
	}
}

func TestGitLabWebhookIgnoresUnsupportedEvent(t *testing.T) {
	t.Setenv("GITLAB_BASE_URL", "https://code.mlamp.cn")
	t.Setenv("GITLAB_TOKEN", "token")
	t.Setenv("GITLAB_WEBHOOK_SECRET", "secret")

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/webhooks/gitlab", bytes.NewReader([]byte(`{}`)))
	req.Header.Set("X-Gitlab-Token", "secret")
	req.Header.Set("X-Gitlab-Event", "Job Hook")

	testHandler.HandleGitLabWebhook(rec, req)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("expected 204, got %d (%s)", rec.Code, rec.Body.String())
	}
}

func TestGitLabWebhookRequiresSecret(t *testing.T) {
	t.Setenv("GITLAB_BASE_URL", "https://code.mlamp.cn")
	t.Setenv("GITLAB_TOKEN", "token")
	t.Setenv("GITLAB_WEBHOOK_SECRET", "")

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/webhooks/gitlab", bytes.NewReader([]byte(`{}`)))
	req.Header.Set("X-Gitlab-Token", "secret")
	req.Header.Set("X-Gitlab-Event", "Merge Request Hook")

	testHandler.HandleGitLabWebhook(rec, req)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503, got %d (%s)", rec.Code, rec.Body.String())
	}
}

func TestGitLabWebhookRejectsInvalidJSON(t *testing.T) {
	t.Setenv("GITLAB_BASE_URL", "https://code.mlamp.cn")
	t.Setenv("GITLAB_TOKEN", "token")
	t.Setenv("GITLAB_WEBHOOK_SECRET", "secret")

	for _, event := range []string{"Merge Request Hook", "Pipeline Hook"} {
		t.Run(event, func(t *testing.T) {
			rec := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodPost, "/api/webhooks/gitlab", bytes.NewReader([]byte(`{`)))
			req.Header.Set("X-Gitlab-Token", "secret")
			req.Header.Set("X-Gitlab-Event", event)

			testHandler.HandleGitLabWebhook(rec, req)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("expected 400, got %d (%s)", rec.Code, rec.Body.String())
			}
		})
	}
}

type fakeGitLabAPI struct {
	project         gitlab.Project
	createHookID    int64
	createHookCalls int
	deleteHookCalls int
	deleteErr       error
}

func (f *fakeGitLabAPI) GetProject(context.Context, string) (gitlab.Project, error) {
	return f.project, nil
}

func (f *fakeGitLabAPI) CreateProjectHook(context.Context, int64, string, string) (gitlab.ProjectHook, error) {
	f.createHookCalls++
	return gitlab.ProjectHook{ID: f.createHookID}, nil
}

func (f *fakeGitLabAPI) DeleteProjectHook(context.Context, int64, int64) error {
	f.deleteHookCalls++
	return f.deleteErr
}

func installFakeGitLabClient(t *testing.T, fake *fakeGitLabAPI) {
	t.Helper()
	prev := newGitLabClient
	newGitLabClient = func(gitlab.Config) (gitLabAPI, error) {
		return fake, nil
	}
	t.Cleanup(func() { newGitLabClient = prev })
}

func withPublicURLForGitLabTest(t *testing.T, publicURL string) {
	t.Helper()
	prev := testHandler.cfg.PublicURL
	testHandler.cfg.PublicURL = publicURL
	t.Cleanup(func() { testHandler.cfg.PublicURL = prev })
}

func withWorkspaceReposForGitLabTest(t *testing.T, workspaceID string, repos []workspaceRepoRef) {
	t.Helper()
	ctx := context.Background()
	var previous []byte
	if err := testPool.QueryRow(ctx, `SELECT repos FROM workspace WHERE id = $1`, workspaceID).Scan(&previous); err != nil {
		t.Fatalf("read workspace repos: %v", err)
	}
	next, err := json.Marshal(repos)
	if err != nil {
		t.Fatalf("marshal workspace repos: %v", err)
	}
	if _, err := testPool.Exec(ctx, `UPDATE workspace SET repos = $1 WHERE id = $2`, next, workspaceID); err != nil {
		t.Fatalf("set workspace repos: %v", err)
	}
	t.Cleanup(func() {
		_, _ = testPool.Exec(context.Background(), `UPDATE workspace SET repos = $1 WHERE id = $2`, previous, workspaceID)
	})
}

func readWorkspaceReposForGitLabTest(t *testing.T, workspaceID string) []workspaceRepoRef {
	t.Helper()
	var raw []byte
	if err := testPool.QueryRow(context.Background(), `SELECT repos FROM workspace WHERE id = $1`, workspaceID).Scan(&raw); err != nil {
		t.Fatalf("read workspace repos: %v", err)
	}
	var repos []workspaceRepoRef
	if err := json.Unmarshal(raw, &repos); err != nil {
		t.Fatalf("decode workspace repos: %v", err)
	}
	return repos
}

func seedGitLabProjectBinding(t *testing.T, workspaceID string, projectID int64) db.GitlabProjectBinding {
	t.Helper()
	ctx := context.Background()
	conn, err := testHandler.Queries.UpsertGitLabConnection(ctx, db.UpsertGitLabConnectionParams{
		WorkspaceID:   parseUUID(workspaceID),
		BaseUrl:       "https://code.mlamp.cn",
		Host:          "code.mlamp.cn",
		ConnectedByID: parseUUID(testUserID),
	})
	if err != nil {
		t.Fatalf("UpsertGitLabConnection: %v", err)
	}
	binding, err := testHandler.Queries.UpsertGitLabProjectBinding(ctx, db.UpsertGitLabProjectBindingParams{
		WorkspaceID:       parseUUID(workspaceID),
		ConnectionID:      conn.ID,
		GitlabProjectID:   projectID,
		PathWithNamespace: fmt.Sprintf("group/project-%d", projectID),
		WebUrl:            fmt.Sprintf("https://code.mlamp.cn/group/project-%d", projectID),
		HookEnabled:       false,
	})
	if err != nil {
		t.Fatalf("UpsertGitLabProjectBinding: %v", err)
	}
	t.Cleanup(func() {
		cleanupGitLabProjectRows(context.Background(), workspaceID, projectID)
	})
	return binding
}

func cleanupGitLabProjectRows(ctx context.Context, workspaceID string, projectID int64) {
	_, _ = testPool.Exec(ctx, `
DELETE FROM gitlab_mr_pipeline
WHERE merge_request_id IN (
	SELECT id FROM gitlab_merge_request WHERE workspace_id = $1 AND gitlab_project_id = $2
)`, workspaceID, projectID)
	_, _ = testPool.Exec(ctx, `
DELETE FROM issue_gitlab_merge_request
WHERE workspace_id = $1
  AND merge_request_id IN (
	SELECT id FROM gitlab_merge_request WHERE workspace_id = $1 AND gitlab_project_id = $2
  )`, workspaceID, projectID)
	_, _ = testPool.Exec(ctx, `DELETE FROM gitlab_merge_request WHERE workspace_id = $1 AND gitlab_project_id = $2`, workspaceID, projectID)
	_, _ = testPool.Exec(ctx, `DELETE FROM gitlab_project_binding WHERE workspace_id = $1 AND gitlab_project_id = $2`, workspaceID, projectID)
	_, _ = testPool.Exec(ctx, `DELETE FROM gitlab_connection WHERE workspace_id = $1 AND host = 'code.mlamp.cn'`, workspaceID)
}

func createGitLabTestIssue(t *testing.T, title, status string) IssueResponse {
	t.Helper()
	return createGitLabIssueInWorkspace(t, testWorkspaceID, title, status)
}

func createGitLabIssueInWorkspace(t *testing.T, workspaceID, title, status string) IssueResponse {
	t.Helper()
	rec := httptest.NewRecorder()
	var buf bytes.Buffer
	if err := json.NewEncoder(&buf).Encode(map[string]any{
		"title":  title,
		"status": status,
	}); err != nil {
		t.Fatalf("encode issue body: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, "/api/issues?workspace_id="+workspaceID, &buf)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-User-ID", testUserID)
	req.Header.Set("X-Workspace-ID", workspaceID)
	testHandler.CreateIssue(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("CreateIssue: %d %s", rec.Code, rec.Body.String())
	}
	var out IssueResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode issue: %v", err)
	}
	t.Cleanup(func() {
		ctx := context.Background()
		_, _ = testPool.Exec(ctx, `DELETE FROM issue_gitlab_merge_request WHERE issue_id = $1`, out.ID)
		_, _ = testPool.Exec(ctx, `DELETE FROM activity_log WHERE issue_id = $1`, out.ID)
		_, _ = testPool.Exec(ctx, `DELETE FROM issue WHERE id = $1`, out.ID)
	})
	return out
}

func fireGitLabMRWebhook(t *testing.T, projectID int64, iid int32, title, description, branch, state, action, sha string) {
	t.Helper()
	t.Setenv("GITLAB_BASE_URL", "https://code.mlamp.cn")
	t.Setenv("GITLAB_TOKEN", "token")
	t.Setenv("GITLAB_WEBHOOK_SECRET", "secret")
	attrs := map[string]any{
		"iid":                   iid,
		"title":                 title,
		"description":           description,
		"state":                 state,
		"draft":                 state == "draft",
		"url":                   fmt.Sprintf("https://code.mlamp.cn/group/project-%d/-/merge_requests/%d", projectID, iid),
		"source_branch":         branch,
		"target_branch":         "main",
		"last_commit":           map[string]any{"id": sha},
		"merge_commit_sha":      "",
		"detailed_merge_status": "mergeable",
		"has_conflicts":         false,
		"created_at":            "2026-04-28T00:00:00Z",
		"updated_at":            "2026-04-29T00:00:00Z",
		"merged_at":             "",
		"closed_at":             "",
		"action":                action,
	}
	if state == "merged" {
		attrs["merged_at"] = "2026-04-29T00:00:00Z"
	} else if state == "closed" {
		attrs["closed_at"] = "2026-04-29T00:00:00Z"
	}
	raw, _ := json.Marshal(map[string]any{
		"object_kind": "merge_request",
		"user": map[string]any{
			"username":   "gitlab-user",
			"avatar_url": "https://code.mlamp.cn/avatar.png",
		},
		"project": map[string]any{
			"id":                  projectID,
			"path_with_namespace": fmt.Sprintf("group/project-%d", projectID),
			"web_url":             fmt.Sprintf("https://code.mlamp.cn/group/project-%d", projectID),
		},
		"object_attributes": attrs,
	})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/webhooks/gitlab", bytes.NewReader(raw))
	req.Header.Set("X-Gitlab-Token", "secret")
	req.Header.Set("X-Gitlab-Event", "Merge Request Hook")
	testHandler.HandleGitLabWebhook(rec, req)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("MR webhook: expected 202, got %d (%s)", rec.Code, rec.Body.String())
	}
}

func fireGitLabPipelineWebhook(t *testing.T, projectID, pipelineID int64, sha, status string) {
	t.Helper()
	t.Setenv("GITLAB_BASE_URL", "https://code.mlamp.cn")
	t.Setenv("GITLAB_TOKEN", "token")
	t.Setenv("GITLAB_WEBHOOK_SECRET", "secret")
	raw, _ := json.Marshal(map[string]any{
		"object_kind": "pipeline",
		"object_attributes": map[string]any{
			"id":         pipelineID,
			"sha":        sha,
			"ref":        "main",
			"status":     status,
			"url":        fmt.Sprintf("https://code.mlamp.cn/group/project-%d/-/pipelines/%d", projectID, pipelineID),
			"updated_at": "2026-04-29T00:00:00Z",
		},
		"project": map[string]any{"id": projectID},
	})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/webhooks/gitlab", bytes.NewReader(raw))
	req.Header.Set("X-Gitlab-Token", "secret")
	req.Header.Set("X-Gitlab-Event", "Pipeline Hook")
	testHandler.HandleGitLabWebhook(rec, req)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("Pipeline webhook: expected 202, got %d (%s)", rec.Code, rec.Body.String())
	}
}

func createGitLabProjectBindingRequest(t *testing.T, workspaceID, project string) *http.Request {
	t.Helper()
	req := newRequest(http.MethodPost, "/api/workspaces/"+workspaceID+"/gitlab/projects", map[string]any{
		"project": project,
	})
	req = withURLParam(req, "id", workspaceID)
	req = req.WithContext(middleware.SetMemberContext(req.Context(), workspaceID, db.Member{
		Role:   "owner",
		UserID: parseUUID(testUserID),
	}))
	return req
}

func deleteGitLabProjectBindingRequest(t *testing.T, workspaceID, bindingID string) *http.Request {
	t.Helper()
	req := newRequest(http.MethodDelete, "/api/workspaces/"+workspaceID+"/gitlab/projects/"+bindingID, nil)
	req = withURLParams(req, "id", workspaceID, "bindingId", bindingID)
	req = req.WithContext(middleware.SetMemberContext(req.Context(), workspaceID, db.Member{
		Role:   "owner",
		UserID: parseUUID(testUserID),
	}))
	return req
}

func TestCreateGitLabProjectBindingReusesExistingHook(t *testing.T) {
	if testHandler == nil {
		t.Skip("handler test fixture not initialized (no DB?)")
	}
	t.Setenv("GITLAB_BASE_URL", "https://code.mlamp.cn")
	t.Setenv("GITLAB_TOKEN", "token")
	t.Setenv("GITLAB_WEBHOOK_SECRET", "secret")
	withPublicURLForGitLabTest(t, "https://multica.example")
	const projectID int64 = 98001
	cleanupGitLabProjectRows(context.Background(), testWorkspaceID, projectID)
	t.Cleanup(func() { cleanupGitLabProjectRows(context.Background(), testWorkspaceID, projectID) })

	fake := &fakeGitLabAPI{
		project: gitlab.Project{
			ID:                projectID,
			PathWithNamespace: "group/idempotent",
			WebURL:            "https://code.mlamp.cn/group/idempotent",
		},
		createHookID: 88001,
	}
	installFakeGitLabClient(t, fake)

	for i := 0; i < 2; i++ {
		rec := httptest.NewRecorder()
		testHandler.CreateGitLabProjectBinding(rec, createGitLabProjectBindingRequest(t, testWorkspaceID, "group/idempotent"))
		if rec.Code != http.StatusOK {
			t.Fatalf("CreateGitLabProjectBinding call %d: expected 200, got %d (%s)", i+1, rec.Code, rec.Body.String())
		}
	}
	if fake.createHookCalls != 1 {
		t.Fatalf("CreateProjectHook calls = %d, want 1", fake.createHookCalls)
	}
	rows, err := testHandler.Queries.ListGitLabProjectBindingsByWorkspace(context.Background(), parseUUID(testWorkspaceID))
	if err != nil {
		t.Fatalf("ListGitLabProjectBindingsByWorkspace: %v", err)
	}
	var matches []db.GitlabProjectBinding
	for _, row := range rows {
		if row.GitlabProjectID == projectID {
			matches = append(matches, row)
		}
	}
	if len(matches) != 1 {
		t.Fatalf("expected 1 persisted binding for project, got %d", len(matches))
	}
	if !matches[0].HookID.Valid || matches[0].HookID.Int64 != 88001 || !matches[0].HookEnabled {
		t.Fatalf("binding hook state = id %+v enabled %v, want id 88001 enabled", matches[0].HookID, matches[0].HookEnabled)
	}
}

func TestCreateGitLabProjectBindingAddsProjectToWorkspaceRepos(t *testing.T) {
	if testHandler == nil {
		t.Skip("handler test fixture not initialized (no DB?)")
	}
	t.Setenv("GITLAB_BASE_URL", "https://code.mlamp.cn")
	t.Setenv("GITLAB_TOKEN", "token")
	t.Setenv("GITLAB_WEBHOOK_SECRET", "secret")
	withPublicURLForGitLabTest(t, "https://multica.example")
	withWorkspaceReposForGitLabTest(t, testWorkspaceID, []workspaceRepoRef{
		{URL: "https://github.com/acme/existing.git", Description: "existing"},
	})
	const projectID int64 = 98002
	cleanupGitLabProjectRows(context.Background(), testWorkspaceID, projectID)
	t.Cleanup(func() { cleanupGitLabProjectRows(context.Background(), testWorkspaceID, projectID) })

	fake := &fakeGitLabAPI{
		project: gitlab.Project{
			ID:                projectID,
			PathWithNamespace: "group/shared",
			WebURL:            "https://code.mlamp.cn/group/shared",
			HTTPURLToRepo:     "https://code.mlamp.cn/group/shared.git",
			SSHURLToRepo:      "git@code.mlamp.cn:group/shared.git",
		},
		createHookID: 88002,
	}
	installFakeGitLabClient(t, fake)

	rec := httptest.NewRecorder()
	testHandler.CreateGitLabProjectBinding(rec, createGitLabProjectBindingRequest(t, testWorkspaceID, "group/shared"))
	if rec.Code != http.StatusOK {
		t.Fatalf("CreateGitLabProjectBinding: expected 200, got %d (%s)", rec.Code, rec.Body.String())
	}

	repos := readWorkspaceReposForGitLabTest(t, testWorkspaceID)
	if len(repos) != 2 {
		t.Fatalf("expected GitLab project to be appended to workspace repos, got %+v", repos)
	}
	if repos[1].URL != "https://code.mlamp.cn/group/shared.git" || repos[1].Description != "GitLab: group/shared" {
		t.Fatalf("gitlab repo entry = %+v", repos[1])
	}
}

func seedGitLabProjectBindingWithHook(t *testing.T, workspaceID string, projectID, hookID int64) db.GitlabProjectBinding {
	t.Helper()
	ctx := context.Background()
	conn, err := testHandler.Queries.UpsertGitLabConnection(ctx, db.UpsertGitLabConnectionParams{
		WorkspaceID:   parseUUID(workspaceID),
		BaseUrl:       "https://code.mlamp.cn",
		Host:          "code.mlamp.cn",
		ConnectedByID: parseUUID(testUserID),
	})
	if err != nil {
		t.Fatalf("UpsertGitLabConnection: %v", err)
	}
	binding, err := testHandler.Queries.UpsertGitLabProjectBinding(ctx, db.UpsertGitLabProjectBindingParams{
		WorkspaceID:       parseUUID(workspaceID),
		ConnectionID:      conn.ID,
		GitlabProjectID:   projectID,
		PathWithNamespace: fmt.Sprintf("group/hooked-%d", projectID),
		WebUrl:            fmt.Sprintf("https://code.mlamp.cn/group/hooked-%d", projectID),
		HookEnabled:       true,
		HookID:            pgtype.Int8{Int64: hookID, Valid: true},
	})
	if err != nil {
		t.Fatalf("UpsertGitLabProjectBinding: %v", err)
	}
	t.Cleanup(func() {
		cleanupGitLabProjectRows(context.Background(), workspaceID, projectID)
	})
	return binding
}

func TestDeleteGitLabProjectBindingRemoteFailureKeepsBinding(t *testing.T) {
	if testHandler == nil {
		t.Skip("handler test fixture not initialized (no DB?)")
	}
	t.Setenv("GITLAB_BASE_URL", "https://code.mlamp.cn")
	t.Setenv("GITLAB_TOKEN", "token")
	t.Setenv("GITLAB_WEBHOOK_SECRET", "secret")
	const projectID int64 = 98002
	binding := seedGitLabProjectBindingWithHook(t, testWorkspaceID, projectID, 88002)
	fake := &fakeGitLabAPI{deleteErr: errors.New("gitlab api error 500: boom")}
	installFakeGitLabClient(t, fake)

	rec := httptest.NewRecorder()
	testHandler.DeleteGitLabProjectBinding(rec, deleteGitLabProjectBindingRequest(t, testWorkspaceID, uuidToString(binding.ID)))
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("expected 502, got %d (%s)", rec.Code, rec.Body.String())
	}
	if fake.deleteHookCalls != 1 {
		t.Fatalf("DeleteProjectHook calls = %d, want 1", fake.deleteHookCalls)
	}
	if _, err := testHandler.Queries.GetGitLabProjectBinding(context.Background(), db.GetGitLabProjectBindingParams{
		ID:          binding.ID,
		WorkspaceID: parseUUID(testWorkspaceID),
	}); err != nil {
		t.Fatalf("binding should remain after remote delete failure: %v", err)
	}
}

func TestDeleteGitLabProjectBindingRemoteSuccessDeletesBinding(t *testing.T) {
	if testHandler == nil {
		t.Skip("handler test fixture not initialized (no DB?)")
	}
	t.Setenv("GITLAB_BASE_URL", "https://code.mlamp.cn")
	t.Setenv("GITLAB_TOKEN", "token")
	t.Setenv("GITLAB_WEBHOOK_SECRET", "secret")
	const projectID int64 = 98003
	binding := seedGitLabProjectBindingWithHook(t, testWorkspaceID, projectID, 88003)
	fake := &fakeGitLabAPI{}
	installFakeGitLabClient(t, fake)

	rec := httptest.NewRecorder()
	testHandler.DeleteGitLabProjectBinding(rec, deleteGitLabProjectBindingRequest(t, testWorkspaceID, uuidToString(binding.ID)))
	if rec.Code != http.StatusNoContent {
		t.Fatalf("expected 204, got %d (%s)", rec.Code, rec.Body.String())
	}
	if fake.deleteHookCalls != 1 {
		t.Fatalf("DeleteProjectHook calls = %d, want 1", fake.deleteHookCalls)
	}
	if _, err := testHandler.Queries.GetGitLabProjectBinding(context.Background(), db.GetGitLabProjectBindingParams{
		ID:          binding.ID,
		WorkspaceID: parseUUID(testWorkspaceID),
	}); err == nil {
		t.Fatal("binding should be deleted after remote delete success")
	}
}

func TestDeleteGitLabProjectBindingDoesNotRequireWebhookSecret(t *testing.T) {
	if testHandler == nil {
		t.Skip("handler test fixture not initialized (no DB?)")
	}
	t.Setenv("GITLAB_BASE_URL", "https://code.mlamp.cn")
	t.Setenv("GITLAB_TOKEN", "token")
	t.Setenv("GITLAB_WEBHOOK_SECRET", "")
	const projectID int64 = 98004
	binding := seedGitLabProjectBindingWithHook(t, testWorkspaceID, projectID, 88004)
	fake := &fakeGitLabAPI{}
	installFakeGitLabClient(t, fake)

	rec := httptest.NewRecorder()
	testHandler.DeleteGitLabProjectBinding(rec, deleteGitLabProjectBindingRequest(t, testWorkspaceID, uuidToString(binding.ID)))
	if rec.Code != http.StatusNoContent {
		t.Fatalf("expected 204, got %d (%s)", rec.Code, rec.Body.String())
	}
	if fake.deleteHookCalls != 1 {
		t.Fatalf("DeleteProjectHook calls = %d, want 1", fake.deleteHookCalls)
	}
	if _, err := testHandler.Queries.GetGitLabProjectBinding(context.Background(), db.GetGitLabProjectBindingParams{
		ID:          binding.ID,
		WorkspaceID: parseUUID(testWorkspaceID),
	}); err == nil {
		t.Fatal("binding should be deleted when API credentials are present without webhook secret")
	}
}

func TestDeleteGitLabProjectBindingMissingAPICredentialsKeepsBinding(t *testing.T) {
	if testHandler == nil {
		t.Skip("handler test fixture not initialized (no DB?)")
	}
	t.Setenv("GITLAB_BASE_URL", "https://code.mlamp.cn")
	t.Setenv("GITLAB_TOKEN", "")
	t.Setenv("GITLAB_WEBHOOK_SECRET", "")
	const projectID int64 = 98005
	binding := seedGitLabProjectBindingWithHook(t, testWorkspaceID, projectID, 88005)
	fake := &fakeGitLabAPI{}
	installFakeGitLabClient(t, fake)

	rec := httptest.NewRecorder()
	testHandler.DeleteGitLabProjectBinding(rec, deleteGitLabProjectBindingRequest(t, testWorkspaceID, uuidToString(binding.ID)))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503, got %d (%s)", rec.Code, rec.Body.String())
	}
	if fake.deleteHookCalls != 0 {
		t.Fatalf("DeleteProjectHook calls = %d, want 0", fake.deleteHookCalls)
	}
	if _, err := testHandler.Queries.GetGitLabProjectBinding(context.Background(), db.GetGitLabProjectBindingParams{
		ID:          binding.ID,
		WorkspaceID: parseUUID(testWorkspaceID),
	}); err != nil {
		t.Fatalf("binding should remain when GitLab API credentials are incomplete: %v", err)
	}
}

func TestGitLabRoutesRoleGating(t *testing.T) {
	if testHandler == nil {
		t.Skip("handler test fixture not initialized (no DB?)")
	}
	ctx := context.Background()
	const slug = "gitlab-routes-role-gating"
	_, _ = testPool.Exec(ctx, `DELETE FROM workspace WHERE slug = $1`, slug)

	var wsID string
	if err := testPool.QueryRow(ctx, `
INSERT INTO workspace (name, slug, description, issue_prefix)
VALUES ($1, $2, $3, $4)
RETURNING id
`, "GitLab Routes Role Gating", slug, "gitlab routes role gating", "GLR").Scan(&wsID); err != nil {
		t.Fatalf("create workspace: %v", err)
	}
	mkUser := func(t *testing.T, label string) string {
		t.Helper()
		var id string
		email := fmt.Sprintf("gitlab-routes-%s-%s@multica.ai", slug, label)
		if err := testPool.QueryRow(ctx, `INSERT INTO "user" (name, email) VALUES ($1, $2) RETURNING id`, "GLR "+label, email).Scan(&id); err != nil {
			t.Fatalf("create user %s: %v", label, err)
		}
		return id
	}
	adminUserID := mkUser(t, "admin")
	memberUserID := mkUser(t, "member")
	outsiderUserID := mkUser(t, "outsider")
	for _, m := range []struct {
		userID, role string
	}{{adminUserID, "admin"}, {memberUserID, "member"}} {
		if _, err := testPool.Exec(ctx, `INSERT INTO member (workspace_id, user_id, role) VALUES ($1, $2, $3)`, wsID, m.userID, m.role); err != nil {
			t.Fatalf("insert member %s: %v", m.role, err)
		}
	}
	binding := seedGitLabProjectBinding(t, wsID, 96001)
	t.Cleanup(func() {
		_, _ = testPool.Exec(context.Background(), `DELETE FROM workspace WHERE id = $1`, wsID)
		for _, uid := range []string{adminUserID, memberUserID, outsiderUserID} {
			_, _ = testPool.Exec(context.Background(), `DELETE FROM "user" WHERE id = $1`, uid)
		}
	})

	router := chi.NewRouter()
	router.Route("/api/workspaces/{id}", func(r chi.Router) {
		r.Group(func(r chi.Router) {
			r.Use(middleware.RequireWorkspaceMemberFromURL(testHandler.Queries, "id"))
			r.Get("/gitlab/config", testHandler.GetGitLabConfig)
		})
		r.Group(func(r chi.Router) {
			r.Use(middleware.RequireWorkspaceRoleFromURL(testHandler.Queries, "id", "owner", "admin"))
			r.Post("/gitlab/projects", testHandler.CreateGitLabProjectBinding)
			r.Delete("/gitlab/projects/{bindingId}", testHandler.DeleteGitLabProjectBinding)
		})
	})
	exercise := func(method, path, userID string, body []byte) int {
		req := httptest.NewRequest(method, path, bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-User-ID", userID)
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)
		return rec.Code
	}
	if got := exercise(http.MethodGet, "/api/workspaces/"+wsID+"/gitlab/config", memberUserID, nil); got != http.StatusOK {
		t.Fatalf("member GET config: want 200, got %d", got)
	}
	if got := exercise(http.MethodPost, "/api/workspaces/"+wsID+"/gitlab/projects", memberUserID, []byte(`{"project":"group/repo"}`)); got != http.StatusForbidden {
		t.Fatalf("member POST project: want 403, got %d", got)
	}
	if got := exercise(http.MethodGet, "/api/workspaces/"+wsID+"/gitlab/config", outsiderUserID, nil); got != http.StatusNotFound {
		t.Fatalf("outsider GET config: want 404, got %d", got)
	}
	if got := exercise(http.MethodDelete, "/api/workspaces/"+wsID+"/gitlab/projects/"+uuidToString(binding.ID), memberUserID, nil); got != http.StatusForbidden {
		t.Fatalf("member DELETE project: want 403, got %d", got)
	}
	if got := exercise(http.MethodDelete, "/api/workspaces/"+wsID+"/gitlab/projects/"+uuidToString(binding.ID), adminUserID, nil); got != http.StatusNoContent {
		t.Fatalf("admin DELETE project: want 204, got %d", got)
	}
}

func TestGitLabProjectBindingResponseRedactsLastSyncError(t *testing.T) {
	rawErr := `hook failed: GITLAB_WEBHOOK_SECRET=super-secret {"token":"json-secret"} secret-token: raw-secret glpat-AbCdEfGhIjKlMnOpQrStUvWx`
	resp := gitLabProjectBindingToResponse(db.GitlabProjectBinding{
		LastSyncError: pgtype.Text{String: rawErr, Valid: true},
	})
	if resp.LastSyncError == nil {
		t.Fatal("expected last_sync_error to be present")
	}
	got := *resp.LastSyncError
	if !strings.Contains(got, "GITLAB_WEBHOOK_SECRET") || !strings.Contains(got, "[REDACTED") {
		t.Fatalf("expected redacted marker, got: %s", got)
	}
	for _, leak := range []string{
		"=super-secret",
		"json-secret",
		"raw-secret",
		"glpat-AbCdEfGhIjKlMnOpQrStUvWx",
	} {
		if strings.Contains(got, leak) {
			t.Fatalf("response mapper leaked %q in last_sync_error: %s", leak, got)
		}
	}
}

func TestGitLabConfigRedactsStoredLastSyncError(t *testing.T) {
	if testHandler == nil {
		t.Skip("handler test fixture not initialized (no DB?)")
	}
	const projectID int64 = 96002
	binding := seedGitLabProjectBinding(t, testWorkspaceID, projectID)
	rawErr := `hook failed: GITLAB_WEBHOOK_SECRET=super-secret {"token":"json-secret"} secret-token: raw-secret glpat-AbCdEfGhIjKlMnOpQrStUvWx`
	if _, err := testPool.Exec(
		context.Background(),
		`UPDATE gitlab_project_binding SET last_sync_error = $1 WHERE id = $2`,
		rawErr,
		binding.ID,
	); err != nil {
		t.Fatalf("seed raw last_sync_error: %v", err)
	}

	router := chi.NewRouter()
	router.Route("/api/workspaces/{id}", func(r chi.Router) {
		r.Use(middleware.RequireWorkspaceMemberFromURL(testHandler.Queries, "id"))
		r.Get("/gitlab/config", testHandler.GetGitLabConfig)
	})
	req := httptest.NewRequest(http.MethodGet, "/api/workspaces/"+testWorkspaceID+"/gitlab/config", nil)
	req.Header.Set("X-User-ID", testUserID)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET config: want 200, got %d (%s)", rec.Code, rec.Body.String())
	}

	body := rec.Body.String()
	if !strings.Contains(body, "GITLAB_WEBHOOK_SECRET") || !strings.Contains(body, "[REDACTED") {
		t.Fatalf("expected redacted marker in response body, got: %s", body)
	}
	for _, leak := range []string{
		"=super-secret",
		"json-secret",
		"raw-secret",
		"glpat-AbCdEfGhIjKlMnOpQrStUvWx",
	} {
		if strings.Contains(body, leak) {
			t.Fatalf("GET config leaked %q in response body: %s", leak, body)
		}
	}
}

func TestGitLabWebhookLinksMRFromTitleBodyAndBranch(t *testing.T) {
	if testHandler == nil {
		t.Skip("handler test fixture not initialized (no DB?)")
	}
	const projectID int64 = 97001
	seedGitLabProjectBinding(t, testWorkspaceID, projectID)
	issue := createGitLabTestIssue(t, "gitlab link", "in_progress")

	fireGitLabMRWebhook(t, projectID, 1, "Refs "+issue.Identifier, "See "+issue.Identifier, strings.ToLower(issue.Identifier)+"/branch", "opened", "open", "abc123")

	rows, err := testHandler.Queries.ListGitLabMergeRequestsByIssue(context.Background(), db.ListGitLabMergeRequestsByIssueParams{
		WorkspaceID: parseUUID(testWorkspaceID),
		IssueID:     parseUUID(issue.ID),
	})
	if err != nil {
		t.Fatalf("ListGitLabMergeRequestsByIssue: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("expected 1 linked MR, got %d", len(rows))
	}

	rec := httptest.NewRecorder()
	req := newRequest(http.MethodGet, "/api/issues/"+issue.ID+"/gitlab/merge-requests", nil)
	req = withURLParam(req, "id", issue.ID)
	testHandler.ListGitLabMergeRequestsForIssue(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("list endpoint: %d %s", rec.Code, rec.Body.String())
	}
	var body struct {
		MergeRequests []GitLabMergeRequestResponse `json:"merge_requests"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if len(body.MergeRequests) != 1 || body.MergeRequests[0].IID != 1 {
		t.Fatalf("unexpected list response: %+v", body.MergeRequests)
	}
}

func TestGitLabClosingIntentTitleBodyOnly(t *testing.T) {
	if testHandler == nil {
		t.Skip("handler test fixture not initialized (no DB?)")
	}
	ctx := context.Background()
	const projectID int64 = 97002
	seedGitLabProjectBinding(t, testWorkspaceID, projectID)
	branchOnly := createGitLabTestIssue(t, "gitlab branch only", "in_progress")
	titleBody := createGitLabTestIssue(t, "gitlab title body", "in_progress")

	fireGitLabMRWebhook(t, projectID, 1, "Implement branch only", "", strings.ToLower(branchOnly.Identifier)+"/fix", "merged", "merge", "sha-branch")
	got, err := testHandler.Queries.GetIssue(ctx, parseUUID(branchOnly.ID))
	if err != nil {
		t.Fatalf("GetIssue branchOnly: %v", err)
	}
	if got.Status != "in_progress" {
		t.Fatalf("branch-only identifier should not auto-complete, got %q", got.Status)
	}
	counts, err := testHandler.Queries.GetIssueGitLabMRCloseAggregate(ctx, db.GetIssueGitLabMRCloseAggregateParams{
		WorkspaceID: parseUUID(testWorkspaceID),
		IssueID:     parseUUID(branchOnly.ID),
	})
	if err != nil {
		t.Fatalf("GetIssueGitLabMRCloseAggregate branchOnly: %v", err)
	}
	if counts.MergedWithCloseIntentCount != 0 {
		t.Fatalf("branch-only close intent count = %d, want 0", counts.MergedWithCloseIntentCount)
	}

	fireGitLabMRWebhook(t, projectID, 2, "Fixes "+titleBody.Identifier, "", "feature/title-body", "merged", "merge", "sha-title")
	got, err = testHandler.Queries.GetIssue(ctx, parseUUID(titleBody.ID))
	if err != nil {
		t.Fatalf("GetIssue titleBody: %v", err)
	}
	if got.Status != "done" {
		t.Fatalf("title/body closing keyword should auto-complete, got %q", got.Status)
	}
}

func TestGitLabMergedCloseIntentAdvancesIssueToDone(t *testing.T) {
	if testHandler == nil {
		t.Skip("handler test fixture not initialized (no DB?)")
	}
	prevBus := testHandler.Bus
	testHandler.Bus = events.New()
	t.Cleanup(func() { testHandler.Bus = prevBus })
	var issueUpdatedSource any
	testHandler.Bus.Subscribe(protocol.EventIssueUpdated, func(e events.Event) {
		if payload, ok := e.Payload.(map[string]any); ok {
			issueUpdatedSource = payload["source"]
		}
	})
	const projectID int64 = 97003
	seedGitLabProjectBinding(t, testWorkspaceID, projectID)
	issue := createGitLabTestIssue(t, "gitlab merged close intent", "in_progress")

	fireGitLabMRWebhook(t, projectID, 1, "Implement", "Fixes "+issue.Identifier, "feature/close", "merged", "merge", "sha-close")
	got, err := testHandler.Queries.GetIssue(context.Background(), parseUUID(issue.ID))
	if err != nil {
		t.Fatalf("GetIssue: %v", err)
	}
	if got.Status != "done" {
		t.Fatalf("expected issue done, got %q", got.Status)
	}
	if issueUpdatedSource != "gitlab_mr_merged" {
		t.Fatalf("issue updated source = %v, want gitlab_mr_merged", issueUpdatedSource)
	}
}

func TestGitLabMultipleMRsKeepIssueOpenUntilAllResolved(t *testing.T) {
	if testHandler == nil {
		t.Skip("handler test fixture not initialized (no DB?)")
	}
	ctx := context.Background()
	const projectID int64 = 97004
	seedGitLabProjectBinding(t, testWorkspaceID, projectID)
	issue := createGitLabTestIssue(t, "gitlab multiple mrs", "in_progress")

	fireGitLabMRWebhook(t, projectID, 1, issue.Identifier+": follow-up", "", "feature/open", "opened", "open", "sha-open")
	fireGitLabMRWebhook(t, projectID, 2, "Implement primary", "Closes "+issue.Identifier, "feature/primary", "merged", "merge", "sha-primary")
	got, err := testHandler.Queries.GetIssue(ctx, parseUUID(issue.ID))
	if err != nil {
		t.Fatalf("GetIssue after primary merge: %v", err)
	}
	if got.Status != "in_progress" {
		t.Fatalf("issue should stay in_progress while linked MR is open, got %q", got.Status)
	}

	fireGitLabMRWebhook(t, projectID, 1, issue.Identifier+": follow-up", "", "feature/open", "merged", "merge", "sha-open")
	got, err = testHandler.Queries.GetIssue(ctx, parseUUID(issue.ID))
	if err != nil {
		t.Fatalf("GetIssue after follow-up merge: %v", err)
	}
	if got.Status != "done" {
		t.Fatalf("issue should advance after all MRs resolved, got %q", got.Status)
	}
}

func TestGitLabWebhookProcessesMultipleWorkspaceBindings(t *testing.T) {
	if testHandler == nil {
		t.Skip("handler test fixture not initialized (no DB?)")
	}
	ctx := context.Background()
	const projectID int64 = 97007
	const slug = "gitlab-multi-workspace-binding"
	_, _ = testPool.Exec(ctx, `DELETE FROM workspace WHERE slug = $1`, slug)

	var secondWorkspaceID string
	if err := testPool.QueryRow(ctx, `
INSERT INTO workspace (name, slug, description, issue_prefix)
VALUES ($1, $2, $3, $4)
RETURNING id
`, "GitLab Multi Workspace", slug, "gitlab multi workspace", "GLM").Scan(&secondWorkspaceID); err != nil {
		t.Fatalf("create second workspace: %v", err)
	}
	if _, err := testPool.Exec(ctx, `INSERT INTO member (workspace_id, user_id, role) VALUES ($1, $2, 'owner')`, secondWorkspaceID, testUserID); err != nil {
		t.Fatalf("insert second workspace member: %v", err)
	}
	t.Cleanup(func() {
		_, _ = testPool.Exec(context.Background(), `DELETE FROM workspace WHERE id = $1`, secondWorkspaceID)
	})

	seedGitLabProjectBinding(t, testWorkspaceID, projectID)
	seedGitLabProjectBinding(t, secondWorkspaceID, projectID)
	first := createGitLabTestIssue(t, "gitlab first workspace", "in_progress")
	second := createGitLabIssueInWorkspace(t, secondWorkspaceID, "gitlab second workspace", "in_progress")

	fireGitLabMRWebhook(t, projectID, 1, "Implement shared project work", "Closes "+first.Identifier+"\nCloses "+second.Identifier, "feature/multi-workspace", "merged", "merge", "sha-multi")

	for _, issue := range []IssueResponse{first, second} {
		got, err := testHandler.Queries.GetIssue(ctx, parseUUID(issue.ID))
		if err != nil {
			t.Fatalf("GetIssue %s: %v", issue.Identifier, err)
		}
		if got.Status != "done" {
			t.Fatalf("issue %s should be done after shared project webhook, got %q", issue.Identifier, got.Status)
		}
	}
}

func TestGitLabPipelineBeforeMRDoesNotCreatePipeline(t *testing.T) {
	if testHandler == nil {
		t.Skip("handler test fixture not initialized (no DB?)")
	}
	const projectID int64 = 97005
	seedGitLabProjectBinding(t, testWorkspaceID, projectID)

	fireGitLabPipelineWebhook(t, projectID, 7001, "sha-before", "success")
	var count int
	if err := testPool.QueryRow(context.Background(), `
SELECT COUNT(*) FROM gitlab_mr_pipeline p
JOIN gitlab_merge_request mr ON mr.id = p.merge_request_id
WHERE mr.workspace_id = $1 AND mr.gitlab_project_id = $2
`, testWorkspaceID, projectID).Scan(&count); err != nil {
		t.Fatalf("count pipelines: %v", err)
	}
	if count != 0 {
		t.Fatalf("expected no pipeline rows before MR exists, got %d", count)
	}
}

func TestGitLabPipelineAfterMRUpdatesIssueList(t *testing.T) {
	if testHandler == nil {
		t.Skip("handler test fixture not initialized (no DB?)")
	}
	const projectID int64 = 97006
	seedGitLabProjectBinding(t, testWorkspaceID, projectID)
	issue := createGitLabTestIssue(t, "gitlab pipeline", "in_progress")

	fireGitLabMRWebhook(t, projectID, 1, "Refs "+issue.Identifier, "", "feature/pipeline", "opened", "open", "sha-after")
	fireGitLabPipelineWebhook(t, projectID, 7002, "sha-after", "success")

	rec := httptest.NewRecorder()
	req := newRequest(http.MethodGet, "/api/issues/"+issue.ID+"/gitlab/merge-requests", nil)
	req = withURLParam(req, "id", issue.ID)
	testHandler.ListGitLabMergeRequestsForIssue(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("list endpoint: %d %s", rec.Code, rec.Body.String())
	}
	var body struct {
		MergeRequests []GitLabMergeRequestResponse `json:"merge_requests"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if len(body.MergeRequests) != 1 {
		t.Fatalf("expected 1 MR, got %d", len(body.MergeRequests))
	}
	if body.MergeRequests[0].PipelineStatus == nil || *body.MergeRequests[0].PipelineStatus != "passed" {
		t.Fatalf("pipeline status = %v, want passed", body.MergeRequests[0].PipelineStatus)
	}
	if body.MergeRequests[0].PipelineURL == nil || *body.MergeRequests[0].PipelineURL == "" {
		t.Fatalf("pipeline url missing: %+v", body.MergeRequests[0])
	}
}

func TestGitLabWebhookUnboundProjectDoesNotCreateRows(t *testing.T) {
	if testHandler == nil {
		t.Skip("handler test fixture not initialized (no DB?)")
	}
	const projectID int64 = 97999
	cleanupGitLabProjectRows(context.Background(), testWorkspaceID, projectID)

	fireGitLabMRWebhook(t, projectID, 1, "Refs HAN-1", "", "feature/unbound", "opened", "open", "sha-unbound")
	var count int
	if err := testPool.QueryRow(context.Background(), `SELECT COUNT(*) FROM gitlab_merge_request WHERE gitlab_project_id = $1`, projectID).Scan(&count); err != nil {
		t.Fatalf("count gitlab_merge_request: %v", err)
	}
	if count != 0 {
		t.Fatalf("expected no MR rows for unbound project, got %d", count)
	}
}
