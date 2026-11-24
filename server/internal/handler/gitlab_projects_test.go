package handler

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/integrations/gitlab"
	"github.com/multica-ai/multica/server/internal/integrations/vcs"
	"github.com/multica-ai/multica/server/internal/util/secretbox"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

type canonicalGitLabProjectAPI struct {
	project          gitlab.Project
	projectPath      string
	hookID           int64
	hookURL          string
	hookSecret       string
	deletedProjectID int64
	deletedHookID    int64
	createHookErr    error
	deleteHookErr    error
	config           gitlab.Config
}

func (f *canonicalGitLabProjectAPI) ValidateCredentials(context.Context) error { return nil }
func (f *canonicalGitLabProjectAPI) GetProject(_ context.Context, path string) (gitlab.Project, error) {
	f.projectPath = path
	return f.project, nil
}
func (f *canonicalGitLabProjectAPI) CreateProjectHook(_ context.Context, projectID int64, url, secret string) (gitlab.ProjectHook, error) {
	f.hookURL, f.hookSecret = url, secret
	if f.createHookErr != nil {
		return gitlab.ProjectHook{}, f.createHookErr
	}
	return gitlab.ProjectHook{ID: f.hookID}, nil
}
func (f *canonicalGitLabProjectAPI) GetProjectHook(context.Context, int64, int64) (gitlab.ProjectHook, error) {
	return gitlab.ProjectHook{}, nil
}
func (f *canonicalGitLabProjectAPI) GetCurrentUserName(context.Context) (string, error) {
	return "GitLab user", nil
}
func (f *canonicalGitLabProjectAPI) UpdateProjectHookSecret(context.Context, int64, int64, string, string, string) error {
	return nil
}
func (f *canonicalGitLabProjectAPI) DeleteProjectHook(_ context.Context, projectID, hookID int64) error {
	f.deletedProjectID, f.deletedHookID = projectID, hookID
	return f.deleteHookErr
}

func installCanonicalGitLabProjectAPI(t *testing.T, fake gitLabAPI) {
	t.Helper()
	previous := newGitLabClient
	previousOverride := gitLabClientOverrideForTest
	gitLabClientOverrideForTest = true
	newGitLabClient = func(cfg gitlab.Config) (gitLabAPI, error) {
		if configurable, ok := fake.(*canonicalGitLabProjectAPI); ok {
			configurable.config = cfg
		}
		return fake, nil
	}
	t.Cleanup(func() {
		newGitLabClient = previous
		gitLabClientOverrideForTest = previousOverride
	})
}

func seedCanonicalGitLabProject(t *testing.T, workspaceID, connectionID string, gitlabProjectID int64, path string, hookID *int64, hookEnabled bool) string {
	t.Helper()
	var id string
	var hook any
	if hookID != nil {
		hook = *hookID
	}
	err := testPool.QueryRow(context.Background(), `
INSERT INTO vcs_gitlab_project (
    workspace_id, connection_id, gitlab_project_id, path_with_namespace, web_url,
    hook_id, hook_enabled
) VALUES ($1, $2, $3, $4, $5, $6, $7)
RETURNING id::text
`, workspaceID, connectionID, gitlabProjectID, path, "https://gitlab.example/"+path, hook, hookEnabled).Scan(&id)
	if err != nil {
		t.Fatalf("seed canonical GitLab project: %v", err)
	}
	t.Cleanup(func() {
		_, _ = testPool.Exec(context.Background(), `DELETE FROM vcs_gitlab_project WHERE id = $1`, id)
	})
	return id
}

func directGitLabProjectRequest(method, path, workspaceID string, body any, param string, value string) *http.Request {
	req := newRequest(method, path, body)
	routeContext := chi.NewRouteContext()
	routeContext.URLParams.Add("id", workspaceID)
	if param != "" {
		routeContext.URLParams.Add(param, value)
	}
	return req.WithContext(context.WithValue(req.Context(), chi.RouteCtxKey, routeContext))
}

func seedGitLabProjectTestConnection(t *testing.T, box *secretbox.Box, provider, baseURL string) (string, string) {
	t.Helper()
	instanceURL := isolatedGitLabProjectTestURL(baseURL)
	connectionID := seedVCSConnection(t, context.Background(), box, provider, instanceURL)
	t.Cleanup(func() {
		_, _ = testPool.Exec(context.Background(), `DELETE FROM vcs_connection WHERE id=$1`, parseUUID(connectionID))
	})
	return connectionID, instanceURL
}

func isolatedGitLabProjectTestURL(baseURL string) string {
	scheme, authority, ok := strings.Cut(baseURL, "://")
	if !ok {
		return baseURL
	}
	host, path, hasPath := strings.Cut(authority, "/")
	host = strings.ReplaceAll(randomID(), "-", "") + "." + host
	if hasPath {
		return scheme + "://" + host + "/" + path
	}
	return scheme + "://" + host
}

func TestVCSGitLabProjectCreateUsesCanonicalConnectionAndScopedWebhook(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("handler test fixture not initialized (no DB?)")
	}
	box := withVCSBox(t)
	withPublicURLForGitLabTest(t, "https://multica.example")
	withWorkspaceReposForGitLabTest(t, testWorkspaceID, []workspaceRepoRef{})
	t.Setenv("GITLAB_PROXY_URL", "socks5h://proxy.example:1080")
	connectionID, instanceURL := seedGitLabProjectTestConnection(t, box, "gitlab", "https://gitlab.example.com/gitlab")
	t.Cleanup(func() {
		_, _ = testPool.Exec(context.Background(), `DELETE FROM vcs_gitlab_project WHERE workspace_id=$1 AND connection_id=$2 AND gitlab_project_id=$3`, testWorkspaceID, parseUUID(connectionID), int64(90102))
	})
	var connectionUUID pgtype.UUID
	if err := connectionUUID.Scan(connectionID); err != nil {
		t.Fatalf("parse connection UUID: %v", err)
	}
	fake := &canonicalGitLabProjectAPI{
		project: gitlab.Project{
			ID:                90102,
			PathWithNamespace: "group/subgroup/project",
			WebURL:            instanceURL + "/group/subgroup/project",
			HTTPURLToRepo:     instanceURL + "/group/subgroup/project.git",
		},
		hookID: 90103,
	}
	installCanonicalGitLabProjectAPI(t, fake)

	req := directGitLabProjectRequest(http.MethodPost,
		"/api/workspaces/"+testWorkspaceID+"/vcs/gitlab/projects", testWorkspaceID,
		map[string]any{
			"connection_id": connectionID,
			"project":       instanceURL + "/group/subgroup/project",
		}, "", "")
	rec := httptest.NewRecorder()
	testHandler.CreateVCSGitLabProject(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("CreateVCSGitLabProject: %d %s", rec.Code, rec.Body.String())
	}
	if fake.projectPath != "group/subgroup/project" {
		t.Fatalf("project lookup path = %q, want full subgroup path without instance subpath", fake.projectPath)
	}
	if fake.config.BaseURL != instanceURL || fake.config.Token != "tok" || fake.config.ProxyURL != "socks5h://proxy.example:1080" {
		t.Fatalf("GitLab config = %+v", fake.config)
	}
	if fake.hookURL != "https://multica.example/api/webhooks/vcs/"+connectionID || fake.hookSecret != vcsTestSecret {
		t.Fatalf("registered hook target = %q, secret match = %t", fake.hookURL, fake.hookSecret == vcsTestSecret)
	}
	var response struct {
		Project          GitLabProjectBindingResponse `json:"project"`
		ManualWebhookURL string                       `json:"manual_webhook_url"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if response.Project.PathWithNamespace != "group/subgroup/project" || !response.Project.HookEnabled || response.Project.HookID == nil || *response.Project.HookID != fake.hookID || response.Project.LegacyLinkPolicy {
		t.Fatalf("canonical project response = %+v", response.Project)
	}
	if response.ManualWebhookURL != fake.hookURL {
		t.Fatalf("manual webhook URL = %q, want %q", response.ManualWebhookURL, fake.hookURL)
	}
	if strings.Contains(rec.Body.String(), vcsTestSecret) || strings.Contains(rec.Body.String(), "tok") {
		t.Fatal("project response leaked connection credentials")
	}
	var canonicalCount int
	if err := testPool.QueryRow(context.Background(), `SELECT count(*) FROM vcs_gitlab_project WHERE workspace_id=$1 AND connection_id=$2 AND gitlab_project_id=$3`, testWorkspaceID, connectionID, int64(90102)).Scan(&canonicalCount); err != nil {
		t.Fatalf("count canonical project: %v", err)
	}
	if canonicalCount != 1 {
		t.Fatalf("canonical project count = %d, want 1", canonicalCount)
	}
	if repos := readWorkspaceReposForGitLabTest(t, testWorkspaceID); len(repos) != 1 || repos[0].URL != fake.project.HTTPURLToRepo {
		t.Fatalf("workspace repos = %+v, want registered clone URL", repos)
	}
}

func TestVCSGitLabProjectReconnectPreservesLegacyLinkPolicy(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("handler test fixture not initialized (no DB?)")
	}
	box := withVCSBox(t)
	connectionID, instanceURL := seedGitLabProjectTestConnection(t, box, "gitlab", "https://gitlab-policy.example")
	var projectID string
	if err := testPool.QueryRow(context.Background(), `
INSERT INTO vcs_gitlab_project (
    workspace_id, connection_id, gitlab_project_id, path_with_namespace, web_url,
    hook_id, hook_enabled, legacy_link_policy
) VALUES ($1, $2, 90104, 'group/policy', 'https://gitlab-policy.example/group/policy', 90105, TRUE, TRUE)
RETURNING id::text
`, testWorkspaceID, connectionID).Scan(&projectID); err != nil {
		t.Fatalf("seed migrated GitLab project: %v", err)
	}
	t.Cleanup(func() {
		_, _ = testPool.Exec(context.Background(), `DELETE FROM vcs_gitlab_project WHERE id = $1`, projectID)
	})
	fake := &canonicalGitLabProjectAPI{project: gitlab.Project{
		ID:                90104,
		PathWithNamespace: "group/policy",
		WebURL:            instanceURL + "/group/policy",
	}}
	installCanonicalGitLabProjectAPI(t, fake)

	rec := httptest.NewRecorder()
	testHandler.CreateVCSGitLabProject(rec, directGitLabProjectRequest(http.MethodPost,
		"/api/workspaces/"+testWorkspaceID+"/vcs/gitlab/projects", testWorkspaceID,
		map[string]any{"connection_id": connectionID, "project": "group/policy"}, "", ""))
	if rec.Code != http.StatusOK {
		t.Fatalf("reconnect migrated project: %d %s", rec.Code, rec.Body.String())
	}
	var legacyLinkPolicy bool
	if err := testPool.QueryRow(context.Background(), `SELECT legacy_link_policy FROM vcs_gitlab_project WHERE id = $1`, projectID).Scan(&legacyLinkPolicy); err != nil {
		t.Fatalf("read legacy link policy after reconnect: %v", err)
	}
	if !legacyLinkPolicy {
		t.Fatal("reconnect reset migrated legacy link policy")
	}
}

func TestVCSGitLabProjectListIsWorkspaceAndProviderScoped(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("handler test fixture not initialized (no DB?)")
	}
	box := withVCSBox(t)
	gitlabConnectionID, _ := seedGitLabProjectTestConnection(t, box, "gitlab", "https://gitlab-list.example")
	forgejoConnectionID, _ := seedGitLabProjectTestConnection(t, box, "forgejo", "https://forgejo-list.example")
	gitlabProjectID := seedCanonicalGitLabProject(t, testWorkspaceID, gitlabConnectionID, 90201, "group/allowed", nil, false)
	wrongProviderProjectID := seedCanonicalGitLabProject(t, testWorkspaceID, forgejoConnectionID, 90202, "group/wrong-provider", nil, false)
	foreignWorkspaceID := createGitLabCleanupWorkspace(t)
	foreignConnectionID := seedGitLabVCSConnectionInWorkspace(t, foreignWorkspaceID, isolatedGitLabProjectTestURL("https://gitlab-foreign-list.example"))
	foreignProjectID := seedCanonicalGitLabProject(t, foreignWorkspaceID, foreignConnectionID, 90203, "group/foreign", nil, false)

	rec := httptest.NewRecorder()
	testHandler.ListVCSGitLabProjects(rec, directGitLabProjectRequest(http.MethodGet,
		"/api/workspaces/"+testWorkspaceID+"/vcs/gitlab/projects", testWorkspaceID, nil, "", ""))
	if rec.Code != http.StatusOK {
		t.Fatalf("ListVCSGitLabProjects: %d %s", rec.Code, rec.Body.String())
	}
	var response struct {
		Projects []GitLabProjectBindingResponse `json:"projects"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	foundAllowedProject := false
	for _, project := range response.Projects {
		if project.ID == gitlabProjectID {
			foundAllowedProject = true
		}
		if project.ID == wrongProviderProjectID || project.ID == foreignProjectID {
			t.Fatalf("project list included out-of-scope project %+v", project)
		}
	}
	if !foundAllowedProject {
		t.Fatalf("project list omitted seeded workspace GitLab project %q: %+v", gitlabProjectID, response.Projects)
	}
}

func TestVCSGitLabProjectDeleteRejectsForeignWorkspaceBeforeRemoteHook(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("handler test fixture not initialized (no DB?)")
	}
	box := withVCSBox(t)
	connectionID, _ := seedGitLabProjectTestConnection(t, box, "gitlab", "https://gitlab-delete.example")
	hookID := int64(90302)
	projectID := seedCanonicalGitLabProject(t, testWorkspaceID, connectionID, 90301, "group/private", &hookID, true)
	fake := &canonicalGitLabProjectAPI{}
	installCanonicalGitLabProjectAPI(t, fake)
	foreignWorkspaceID := "00000000-0000-0000-0000-000000000002"

	rec := httptest.NewRecorder()
	testHandler.DeleteVCSGitLabProject(rec, directGitLabProjectRequest(http.MethodDelete,
		"/api/workspaces/"+foreignWorkspaceID+"/vcs/gitlab/projects/"+projectID,
		foreignWorkspaceID, nil, "projectId", projectID))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("foreign workspace delete: want 404, got %d (%s)", rec.Code, rec.Body.String())
	}
	if fake.deletedHookID != 0 {
		t.Fatal("foreign workspace delete reached GitLab")
	}
	var stillExists bool
	if err := testPool.QueryRow(context.Background(), `SELECT EXISTS (SELECT 1 FROM vcs_gitlab_project WHERE id=$1)`, projectID).Scan(&stillExists); err != nil {
		t.Fatalf("check project remains: %v", err)
	}
	if !stillExists {
		t.Fatal("foreign workspace request deleted canonical project")
	}
}

func TestVCSGitLabProjectDeletePreservesProjectWhenHookRemovalFails(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("handler test fixture not initialized (no DB?)")
	}
	box := withVCSBox(t)
	connectionID, _ := seedGitLabProjectTestConnection(t, box, "gitlab", "https://gitlab-delete-error.example")
	hookID := int64(90402)
	projectID := seedCanonicalGitLabProject(t, testWorkspaceID, connectionID, 90401, "group/keep", &hookID, true)
	fake := &canonicalGitLabProjectAPI{deleteHookErr: errors.New("GitLab API failure")}
	installCanonicalGitLabProjectAPI(t, fake)

	rec := httptest.NewRecorder()
	testHandler.DeleteVCSGitLabProject(rec, directGitLabProjectRequest(http.MethodDelete,
		"/api/workspaces/"+testWorkspaceID+"/vcs/gitlab/projects/"+projectID,
		testWorkspaceID, nil, "projectId", projectID))
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("hook removal failure: want 502, got %d (%s)", rec.Code, rec.Body.String())
	}
	if fake.deletedProjectID != 90401 || fake.deletedHookID != hookID {
		t.Fatalf("remote hook delete target = %d/%d", fake.deletedProjectID, fake.deletedHookID)
	}
	var stillExists bool
	if err := testPool.QueryRow(context.Background(), `SELECT EXISTS (SELECT 1 FROM vcs_gitlab_project WHERE id=$1)`, projectID).Scan(&stillExists); err != nil {
		t.Fatalf("check project remains: %v", err)
	}
	if !stillExists {
		t.Fatal("project was removed despite failed remote hook removal")
	}
}

func TestVCSGitLabProjectRefreshSynchronizesCanonicalPullRequests(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("handler test fixture not initialized (no DB?)")
	}
	box := withVCSBox(t)
	connectionID, instanceURL := seedGitLabProjectTestConnection(t, box, "gitlab", "https://gitlab-refresh.example/gitlab")
	projectID := seedCanonicalGitLabProject(t, testWorkspaceID, connectionID, 90501, "group/subgroup/project", nil, false)
	var pullRequestID string
	if err := testPool.QueryRow(context.Background(), `
INSERT INTO vcs_pull_request (
    workspace_id, connection_id, provider, repo_owner, repo_name, pr_number,
    title, state, html_url, pr_created_at, pr_updated_at, additions, deletions,
    changed_files, head_sha
) VALUES (
    $1, $2, 'gitlab', 'group/subgroup', 'project', 17,
    'before refresh', 'open', $3,
    '2026-01-01T00:00:00Z', '2026-01-02T00:00:00Z', 0, 0, 0, 'oldsha'
) RETURNING id::text
`, testWorkspaceID, connectionID, instanceURL+"/group/subgroup/project/-/merge_requests/17").Scan(&pullRequestID); err != nil {
		t.Fatalf("seed canonical pull request: %v", err)
	}
	t.Cleanup(func() {
		_, _ = testPool.Exec(context.Background(), `DELETE FROM gitlab_mr_note WHERE vcs_pull_request_id=$1`, pullRequestID)
		_, _ = testPool.Exec(context.Background(), `DELETE FROM gitlab_mr_discussion WHERE vcs_pull_request_id=$1`, pullRequestID)
		_, _ = testPool.Exec(context.Background(), `DELETE FROM gitlab_mr_approval_state WHERE vcs_pull_request_id=$1`, pullRequestID)
		_, _ = testPool.Exec(context.Background(), `DELETE FROM gitlab_pipeline_job WHERE vcs_pull_request_id=$1`, pullRequestID)
		_, _ = testPool.Exec(context.Background(), `DELETE FROM vcs_gitlab_pipeline WHERE pull_request_id=$1`, pullRequestID)
		_, _ = testPool.Exec(context.Background(), `DELETE FROM vcs_gitlab_mr_detail WHERE vcs_pull_request_id=$1`, pullRequestID)
		_, _ = testPool.Exec(context.Background(), `DELETE FROM issue_vcs_pull_request WHERE pull_request_id=$1`, pullRequestID)
		_, _ = testPool.Exec(context.Background(), `DELETE FROM vcs_gitlab_migration_id_map WHERE entity_kind='pull_request' AND unified_id=$1`, parseUUID(pullRequestID))
		_, _ = testPool.Exec(context.Background(), `DELETE FROM vcs_pull_request WHERE id=$1`, parseUUID(pullRequestID))
	})
	fake := &fakeGitLabDetailAPI{mr: gitlab.MergeRequest{
		IID:             17,
		Title:           "after refresh",
		State:           "opened",
		WebURL:          instanceURL + "/group/subgroup/project/-/merge_requests/17",
		SHA:             "newsha",
		SourceBranch:    "feature/refresh",
		TargetBranch:    "main",
		CreatedAt:       "2026-01-01T00:00:00Z",
		UpdatedAt:       "2026-01-03T00:00:00Z",
		SourceProjectID: 90501,
		TargetProjectID: 90501,
		SourceProject:   gitlab.Project{ID: 90501, PathWithNamespace: "group/subgroup/project"},
	}}
	installCanonicalGitLabProjectAPI(t, fake)

	rec := httptest.NewRecorder()
	testHandler.RefreshVCSGitLabProject(rec, directGitLabProjectRequest(http.MethodPost,
		"/api/workspaces/"+testWorkspaceID+"/vcs/gitlab/projects/"+projectID+"/refresh",
		testWorkspaceID, nil, "projectId", projectID))
	if rec.Code != http.StatusOK {
		t.Fatalf("RefreshVCSGitLabProject: %d %s", rec.Code, rec.Body.String())
	}
	var summary gitLabRefreshSummary
	if err := json.Unmarshal(rec.Body.Bytes(), &summary); err != nil {
		t.Fatalf("decode refresh summary: %v", err)
	}
	if summary.UpdatedMRs != 1 {
		t.Fatalf("refresh summary = %+v, want one canonical MR update", summary)
	}
	var title, headSHA string
	if err := testPool.QueryRow(context.Background(), `SELECT title, head_sha FROM vcs_pull_request WHERE id=$1`, pullRequestID).Scan(&title, &headSHA); err != nil {
		t.Fatalf("read canonical pull request after refresh: %v", err)
	}
	if title != "after refresh" || headSHA != "newsha" {
		t.Fatalf("canonical pull request after refresh = %q/%q", title, headSHA)
	}
	var lastRefresh pgtype.Timestamptz
	var lastError pgtype.Text
	var inProgress pgtype.Timestamptz
	if err := testPool.QueryRow(context.Background(), `SELECT last_refresh_at, last_refresh_error, refresh_in_progress_at FROM vcs_gitlab_project WHERE id=$1`, projectID).Scan(&lastRefresh, &lastError, &inProgress); err != nil {
		t.Fatalf("read canonical project diagnostics: %v", err)
	}
	if !lastRefresh.Valid || lastError.Valid || inProgress.Valid {
		t.Fatalf("refresh diagnostics = at %+v error %+v in_progress %+v", lastRefresh, lastError, inProgress)
	}
}
func TestVCSGitLabProjectRefreshKeepsStoredPathErrorInternal(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("handler test fixture not initialized (no DB?)")
	}
	box := withVCSBox(t)
	connectionID, _ := seedGitLabProjectTestConnection(t, box, "gitlab", "https://gitlab-refresh-error.example")
	projectID := seedCanonicalGitLabProject(t, testWorkspaceID, connectionID, 90502, "group/project", nil, false)
	if _, err := testPool.Exec(context.Background(), `
		UPDATE vcs_gitlab_project SET path_with_namespace='invalid-path' WHERE id=$1
	`, projectID); err != nil {
		t.Fatalf("corrupt stored project path for regression case: %v", err)
	}
	installCanonicalGitLabProjectAPI(t, &fakeGitLabDetailAPI{})

	previousLogger := slog.Default()
	var logs bytes.Buffer
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
	t.Cleanup(func() { slog.SetDefault(previousLogger) })

	rec := httptest.NewRecorder()
	testHandler.RefreshVCSGitLabProject(rec, directGitLabProjectRequest(http.MethodPost,
		"/api/workspaces/"+testWorkspaceID+"/vcs/gitlab/projects/"+projectID+"/refresh",
		testWorkspaceID, nil, "projectId", projectID))
	if rec.Code != http.StatusInternalServerError ||
		!strings.Contains(rec.Body.String(), "failed to refresh gitlab project") ||
		strings.Contains(rec.Body.String(), "invalid GitLab project path") {
		t.Fatalf("refresh response = %d %s; want a stable public error without the internal cause", rec.Code, rec.Body.String())
	}
	if !strings.Contains(logs.String(), "invalid GitLab project path") ||
		!strings.Contains(logs.String(), projectID) ||
		!strings.Contains(logs.String(), testWorkspaceID) {
		t.Fatalf("internal refresh cause was not logged with workspace/project context: %s", logs.String())
	}
	var storedError string
	if err := testPool.QueryRow(context.Background(),
		`SELECT last_refresh_error FROM vcs_gitlab_project WHERE id=$1`, projectID).Scan(&storedError); err != nil {
		t.Fatalf("read stored refresh diagnostic: %v", err)
	}
	if storedError != "invalid GitLab project path" {
		t.Fatalf("stored refresh diagnostic = %q, want the internal cause", storedError)
	}
}

func TestGitLabLegacyConnectionConfigUsesCanonicalTables(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("handler test fixture not initialized (no DB?)")
	}
	box := withVCSBox(t)
	connectionID, _ := seedGitLabProjectTestConnection(t, box, "gitlab", "https://gitlab-legacy-adapter.example/gitlab")
	projectID := seedCanonicalGitLabProject(t, testWorkspaceID, connectionID, 90601, "group/legacy", nil, false)
	insertGitLabConnectionAlias(t, testWorkspaceID, connectionID, connectionID)
	rec := httptest.NewRecorder()
	req := directGitLabProjectRequest(http.MethodGet,
		"/api/workspaces/"+testWorkspaceID+"/gitlab/config", testWorkspaceID, nil, "", "")
	testHandler.GetGitLabConfig(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("GetGitLabConfig: %d %s", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "gitlab_project_binding") || strings.Contains(rec.Body.String(), "gitlab_connection") {
		t.Fatal("legacy configuration adapter exposed legacy table names")
	}
	var response GitLabConfigResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
		t.Fatalf("decode legacy config: %v", err)
	}
	connectionFound, projectFound := false, false
	for _, connection := range response.Connections {
		if connection.ID == connectionID {
			connectionFound = true
			if !connection.ServerConfigured {
				t.Fatalf("legacy config connection is not credential-configured: %+v", connection)
			}
		}
	}
	for _, project := range response.Projects {
		if project.ID == projectID {
			projectFound = true
		}
	}
	if !response.Configured || !connectionFound || !projectFound {
		t.Fatalf("legacy config omitted configured seeded records: configured=%t connection=%t project=%t response=%+v", response.Configured, connectionFound, projectFound, response)
	}
}
func TestGitLabConnectionBoundaryStoresCanonicalCredentials(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("handler test fixture not initialized (no DB?)")
	}
	withVCSBox(t)
	withPublicURLForGitLabTest(t, "https://webhooks.example")
	t.Setenv("GITLAB_BASE_URL", "")
	t.Setenv("GITLAB_TOKEN", "")
	t.Setenv("GITLAB_WEBHOOK_SECRET", "")
	t.Setenv("GITLAB_PROXY_URL", "http://proxy.gitlab.invalid:8080")

	rawBaseURL := "https://GitLab-" + strings.ToUpper(randomID()) + ".invalid/root/"
	canonicalBaseURL := strings.ToLower(strings.TrimRight(rawBaseURL, "/"))
	previousValidator := validateGitLabVCSConnectionToken
	var validatedURL, validatedToken string
	validateGitLabVCSConnectionToken = func(_ context.Context, instanceURL, token string) (vcs.Account, error) {
		validatedURL, validatedToken = instanceURL, token
		return vcs.Account{Login: "alice"}, nil
	}
	t.Cleanup(func() { validateGitLabVCSConnectionToken = previousValidator })

	create := func(token string) (*httptest.ResponseRecorder, struct {
		Connection    GitLabConnectionResponse `json:"connection"`
		WebhookSecret string                   `json:"webhook_secret"`
		WebhookURL    string                   `json:"webhook_url"`
		WebhookPath   string                   `json:"webhook_path"`
	}) {
		rec := httptest.NewRecorder()
		req := newRequest(http.MethodPost, "/api/workspaces/"+testWorkspaceID+"/gitlab/connections", map[string]string{
			"base_url": rawBaseURL, "access_token": token,
		})
		req = withURLParam(req, "id", testWorkspaceID)
		testHandler.CreateGitLabConnection(rec, req)
		var response struct {
			Connection    GitLabConnectionResponse `json:"connection"`
			WebhookSecret string                   `json:"webhook_secret"`
			WebhookURL    string                   `json:"webhook_url"`
			WebhookPath   string                   `json:"webhook_path"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
			t.Fatalf("decode GitLab connection response: %v", err)
		}
		return rec, response
	}

	token := "private-token-" + randomID()
	firstRec, first := create(token)
	if firstRec.Code != http.StatusOK {
		t.Fatalf("CreateGitLabConnection: %d %s", firstRec.Code, firstRec.Body.String())
	}
	if first.Connection.ID == "" || first.WebhookSecret == "" {
		t.Fatalf("first connection response omitted identity or one-time webhook secret: %+v", first)
	}
	webhookPath := "/api/webhooks/vcs/" + first.Connection.ID
	if first.WebhookPath != webhookPath || first.WebhookURL != "https://webhooks.example"+webhookPath ||
		first.Connection.WebhookPath != webhookPath || first.Connection.WebhookURL != first.WebhookURL {
		t.Fatalf("connection did not expose its unified webhook endpoint: %+v", first)
	}
	if validatedURL != canonicalBaseURL || validatedToken != token {
		t.Fatalf("GitLab validator received %q/%q, want %q/token", validatedURL, validatedToken, canonicalBaseURL)
	}
	if strings.Contains(firstRec.Body.String(), token) {
		t.Fatal("GitLab access token was returned")
	}

	ctx := context.Background()
	connection, err := testHandler.Queries.GetVCSConnectionByID(ctx, parseUUID(first.Connection.ID))
	if err != nil {
		t.Fatalf("load canonical GitLab connection: %v", err)
	}
	if connection.Provider != "gitlab" || connection.InstanceUrl != canonicalBaseURL ||
		connection.AccessTokenEncrypted == "" || connection.AccessTokenEncrypted == token ||
		connection.WebhookSecretEncrypted == "" || connection.WebhookSecretEncrypted == first.WebhookSecret {
		t.Fatalf("canonical GitLab connection did not persist encrypted credentials: %+v", connection)
	}
	config, err := testHandler.gitLabConfigForVCSConnectionRow(connection)
	if err != nil {
		t.Fatalf("load canonical GitLab config: %v", err)
	}
	if config.Token != token || config.WebhookSecret != first.WebhookSecret ||
		config.BaseURL != canonicalBaseURL || config.ProxyURL != "http://proxy.gitlab.invalid:8080" {
		t.Fatalf("canonical GitLab config = %+v", config)
	}

	secondToken := "replacement-token-" + randomID()
	secondRec, second := create(secondToken)
	if secondRec.Code != http.StatusOK {
		t.Fatalf("reconnect GitLab: %d %s", secondRec.Code, secondRec.Body.String())
	}
	if second.Connection.ID != first.Connection.ID || second.WebhookSecret != "" {
		t.Fatalf("reconnect did not retain identity and hide the existing webhook secret: %+v", second)
	}
	connection, err = testHandler.Queries.GetVCSConnectionByID(ctx, parseUUID(first.Connection.ID))
	if err != nil {
		t.Fatalf("reload canonical GitLab connection: %v", err)
	}
	config, err = testHandler.gitLabConfigForVCSConnectionRow(connection)
	if err != nil {
		t.Fatalf("load reconnected GitLab config: %v", err)
	}
	if config.Token != secondToken || config.WebhookSecret != first.WebhookSecret {
		t.Fatal("reconnecting changed the preserved webhook secret or failed to replace the access token")
	}
	t.Cleanup(func() {
		_, _ = testPool.Exec(ctx, `DELETE FROM vcs_gitlab_migration_id_map WHERE workspace_id=$1 AND entity_kind='connection' AND unified_id=$2`, parseUUID(testWorkspaceID), connection.ID)
		_, _ = testPool.Exec(ctx, `DELETE FROM vcs_connection WHERE id = $1`, connection.ID)
	})
}

func seedGitLabVCSWebhookProject(t *testing.T, projectID int64, path string, legacyLinkPolicy bool) string {
	t.Helper()
	ctx := context.Background()
	tokenEncrypted, err := testHandler.sealVCSSecret("tok")
	if err != nil {
		t.Fatalf("encrypt GitLab token: %v", err)
	}
	webhookSecretEncrypted, err := testHandler.sealVCSSecret(vcsTestSecret)
	if err != nil {
		t.Fatalf("encrypt GitLab webhook secret: %v", err)
	}
	connection, err := testHandler.Queries.UpsertVCSConnection(ctx, db.UpsertVCSConnectionParams{
		WorkspaceID:            parseUUID(testWorkspaceID),
		Provider:               "gitlab",
		InstanceUrl:            fmt.Sprintf("https://gitlab-project-%s.invalid/root", randomID()),
		AccountLogin:           "test-user",
		AccessTokenEncrypted:   tokenEncrypted,
		WebhookSecretEncrypted: webhookSecretEncrypted,
		ConnectedByID:          pgtype.UUID{},
	})
	if err != nil {
		t.Fatalf("seed GitLab VCS connection: %v", err)
	}
	if _, err := testPool.Exec(ctx, `
INSERT INTO vcs_gitlab_project (
    workspace_id, connection_id, gitlab_project_id, path_with_namespace, web_url, legacy_link_policy
) VALUES ($1, $2, $3, $4, $5, $6)
`, parseUUID(testWorkspaceID), connection.ID, projectID, path, "https://code.mlamp.cn/"+path, legacyLinkPolicy); err != nil {
		t.Fatalf("seed GitLab VCS project: %v", err)
	}
	t.Cleanup(func() {
		_, _ = testPool.Exec(ctx, `DELETE FROM gitlab_mr_note WHERE vcs_pull_request_id IN (SELECT id FROM vcs_pull_request WHERE connection_id = $1)`, connection.ID)
		_, _ = testPool.Exec(ctx, `DELETE FROM gitlab_mr_discussion WHERE vcs_pull_request_id IN (SELECT id FROM vcs_pull_request WHERE connection_id = $1)`, connection.ID)
		_, _ = testPool.Exec(ctx, `DELETE FROM gitlab_mr_approval_state WHERE vcs_pull_request_id IN (SELECT id FROM vcs_pull_request WHERE connection_id = $1)`, connection.ID)
		_, _ = testPool.Exec(ctx, `DELETE FROM gitlab_pipeline_job WHERE vcs_pull_request_id IN (SELECT id FROM vcs_pull_request WHERE connection_id = $1)`, connection.ID)
		_, _ = testPool.Exec(ctx, `DELETE FROM vcs_gitlab_pipeline WHERE connection_id = $1`, connection.ID)
		_, _ = testPool.Exec(ctx, `DELETE FROM vcs_gitlab_mr_detail WHERE vcs_pull_request_id IN (SELECT id FROM vcs_pull_request WHERE connection_id = $1)`, connection.ID)
		_, _ = testPool.Exec(ctx, `DELETE FROM issue_vcs_pull_request WHERE pull_request_id IN (SELECT id FROM vcs_pull_request WHERE connection_id = $1)`, connection.ID)
		_, _ = testPool.Exec(ctx, `DELETE FROM vcs_gitlab_migration_id_map WHERE workspace_id = $1 AND ((entity_kind='connection' AND unified_id=$2) OR (entity_kind='pull_request' AND unified_id IN (SELECT id FROM vcs_pull_request WHERE connection_id=$2)))`, parseUUID(testWorkspaceID), connection.ID)
		_, _ = testPool.Exec(ctx, `DELETE FROM vcs_pull_request WHERE workspace_id = $1 AND connection_id = $2`, parseUUID(testWorkspaceID), connection.ID)
		_, _ = testPool.Exec(ctx, `DELETE FROM vcs_gitlab_project WHERE workspace_id = $1 AND connection_id = $2`, parseUUID(testWorkspaceID), connection.ID)
		_, _ = testPool.Exec(ctx, `DELETE FROM vcs_connection WHERE workspace_id = $1 AND id = $2`, parseUUID(testWorkspaceID), connection.ID)
	})
	return uuidToString(connection.ID)
}

func fireVCSGitLabMRWebhook(t *testing.T, connectionID string, projectID int64, path string, iid int32, title, description, branch, state, action, sha, updatedAt string) {
	t.Helper()
	raw, err := json.Marshal(map[string]any{
		"object_kind": "merge_request",
		"user":        map[string]any{"username": "gitlab-user", "avatar_url": "https://code.mlamp.cn/avatar.png"},
		"project": map[string]any{
			"id":                  projectID,
			"path_with_namespace": path,
			"web_url":             "https://code.mlamp.cn/" + path,
		},
		"object_attributes": map[string]any{
			"iid":                   iid,
			"title":                 title,
			"description":           description,
			"state":                 state,
			"action":                action,
			"source_branch":         branch,
			"target_branch":         "main",
			"url":                   fmt.Sprintf("https://code.mlamp.cn/%s/-/merge_requests/%d", path, iid),
			"last_commit":           map[string]any{"id": sha},
			"has_conflicts":         false,
			"detailed_merge_status": "mergeable",
			"created_at":            "2026-04-28T00:00:00Z",
			"updated_at":            updatedAt,
		},
	})
	if err != nil {
		t.Fatalf("marshal GitLab VCS webhook: %v", err)
	}
	rec := httptest.NewRecorder()
	testHandler.HandleVCSWebhook(rec, vcsWebhookReq(connectionID, map[string]string{
		"X-Gitlab-Event": "Merge Request Hook",
		"X-Gitlab-Token": vcsTestSecret,
	}, raw))
	if rec.Code != http.StatusAccepted {
		t.Fatalf("GitLab VCS webhook: expected 202, got %d (%s)", rec.Code, rec.Body.String())
	}
}

func TestVCSGitLabProjectDeleteRemovesGitLabOnlyDataButKeepsOfficialPR(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("handler test fixture not initialized (no DB?)")
	}
	_ = withVCSBox(t)
	workspaceID := createGitLabCleanupWorkspace(t)
	connectionID := seedGitLabVCSConnectionInWorkspace(t, workspaceID, isolatedGitLabProjectTestURL("https://gitlab-project-cleanup.example"))
	projectID := seedCanonicalGitLabProject(t, workspaceID, connectionID, 90701, "group/cleanup", nil, false)
	otherProjectID := seedCanonicalGitLabProject(t, workspaceID, connectionID, 90702, "group/keep", nil, false)
	orphanPR, _ := seedGitLabDeletionMR(t, workspaceID, connectionID, "group/cleanup", 1, false)
	officialPR, _ := seedGitLabDeletionMR(t, workspaceID, connectionID, "group/cleanup", 2, true)
	otherPR, _ := seedGitLabDeletionMR(t, workspaceID, connectionID, "group/keep", 3, false)

	rec := httptest.NewRecorder()
	testHandler.DeleteVCSGitLabProject(rec, directGitLabProjectRequest(http.MethodDelete,
		"/api/workspaces/"+workspaceID+"/vcs/gitlab/projects/"+projectID,
		workspaceID, nil, "projectId", projectID))
	if rec.Code != http.StatusNoContent {
		t.Fatalf("DeleteVCSGitLabProject: %d %s", rec.Code, rec.Body.String())
	}
	assertCount := func(label, query, id string, want int) {
		t.Helper()
		var got int
		if err := testPool.QueryRow(context.Background(), query, id).Scan(&got); err != nil {
			t.Fatalf("%s count: %v", label, err)
		}
		if got != want {
			t.Fatalf("%s count = %d, want %d", label, got, want)
		}
	}
	assertCount("deleted project", `SELECT count(*) FROM vcs_gitlab_project WHERE id=$1`, projectID, 0)
	assertCount("orphan pull request", `SELECT count(*) FROM vcs_pull_request WHERE id=$1`, orphanPR, 0)
	assertCount("orphan issue link", `SELECT count(*) FROM issue_vcs_pull_request WHERE pull_request_id=$1`, orphanPR, 0)
	for _, dependent := range []struct {
		label, query string
	}{
		{"orphan GitLab detail", `SELECT count(*) FROM vcs_gitlab_mr_detail WHERE vcs_pull_request_id=$1`},
		{"orphan approval", `SELECT count(*) FROM gitlab_mr_approval_state WHERE vcs_pull_request_id=$1`},
		{"orphan discussion", `SELECT count(*) FROM gitlab_mr_discussion WHERE vcs_pull_request_id=$1`},
		{"orphan note", `SELECT count(*) FROM gitlab_mr_note WHERE vcs_pull_request_id=$1`},
		{"orphan job", `SELECT count(*) FROM gitlab_pipeline_job WHERE vcs_pull_request_id=$1`},
		{"orphan pipeline", `SELECT count(*) FROM vcs_gitlab_pipeline WHERE pull_request_id=$1`},
		{"orphan migration alias", `SELECT count(*) FROM vcs_gitlab_migration_id_map WHERE entity_kind='pull_request' AND unified_id=$1`},
	} {
		assertCount(dependent.label, dependent.query, orphanPR, 0)
	}
	assertCount("official pull request", `SELECT count(*) FROM vcs_pull_request WHERE id=$1`, officialPR, 1)
	assertCount("official detail", `SELECT count(*) FROM vcs_gitlab_mr_detail WHERE vcs_pull_request_id=$1`, officialPR, 1)
	assertCount("official pipeline", `SELECT count(*) FROM vcs_gitlab_pipeline WHERE pull_request_id=$1`, officialPR, 1)
	var officialLink, genericCloseIntent, gitlabCloseIntent bool
	var gitlabLinkedAt pgtype.Timestamptz
	var linkedByType pgtype.Text
	if err := testPool.QueryRow(context.Background(), `
SELECT vcs_linked, gitlab_linked_at, linked_by_type, close_intent, gitlab_close_intent
FROM issue_vcs_pull_request WHERE pull_request_id=$1
`, officialPR).Scan(&officialLink, &gitlabLinkedAt, &linkedByType, &genericCloseIntent, &gitlabCloseIntent); err != nil {
		t.Fatalf("read preserved official link: %v", err)
	}
	if !officialLink || gitlabLinkedAt.Valid || !linkedByType.Valid || linkedByType.String != "member" || !genericCloseIntent || gitlabCloseIntent {
		t.Fatalf("official source after project removal = vcs:%t gitlab_at:%+v linked_by:%+v generic_close:%t gitlab_close:%t", officialLink, gitlabLinkedAt, linkedByType, genericCloseIntent, gitlabCloseIntent)
	}
	assertCount("other project", `SELECT count(*) FROM vcs_gitlab_project WHERE id=$1`, otherProjectID, 1)
	assertCount("other project pull request", `SELECT count(*) FROM vcs_pull_request WHERE id=$1`, otherPR, 1)
	assertCount("other project detail", `SELECT count(*) FROM vcs_gitlab_mr_detail WHERE vcs_pull_request_id=$1`, otherPR, 1)
	assertCount("other project issue link", `SELECT count(*) FROM issue_vcs_pull_request WHERE pull_request_id=$1`, otherPR, 1)
}

func TestVCSConnectionDeleteCleansGitLabDependentsOnlyInItsWorkspaceAndConnection(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("handler test fixture not initialized (no DB?)")
	}
	_ = withVCSBox(t)
	workspaceID := createGitLabCleanupWorkspace(t)
	targetConnectionID := seedGitLabVCSConnectionInWorkspace(t, workspaceID, isolatedGitLabProjectTestURL("https://gitlab-connection-cleanup.example"))
	otherConnectionID := seedGitLabVCSConnectionInWorkspace(t, workspaceID, isolatedGitLabProjectTestURL("https://gitlab-connection-keep.example"))
	foreignWorkspaceID := createGitLabCleanupWorkspace(t)
	foreignConnectionID := seedGitLabVCSConnectionInWorkspace(t, foreignWorkspaceID, isolatedGitLabProjectTestURL("https://gitlab-foreign-workspace.example"))
	targetProjectID := seedCanonicalGitLabProject(t, workspaceID, targetConnectionID, 90801, "group/target", nil, false)
	otherProjectID := seedCanonicalGitLabProject(t, workspaceID, otherConnectionID, 90802, "group/other", nil, false)
	foreignProjectID := seedCanonicalGitLabProject(t, foreignWorkspaceID, foreignConnectionID, 90803, "group/foreign", nil, false)
	targetPR, _ := seedGitLabDeletionMR(t, workspaceID, targetConnectionID, "group/target", 1, false)
	otherPR, _ := seedGitLabDeletionMR(t, workspaceID, otherConnectionID, "group/other", 2, false)
	foreignPR, _ := seedGitLabDeletionMR(t, foreignWorkspaceID, foreignConnectionID, "group/foreign", 3, false)
	if _, err := testPool.Exec(context.Background(), `
INSERT INTO vcs_gitlab_migration_id_map (workspace_id, entity_kind, legacy_id, unified_id)
VALUES ($1, 'connection', gen_random_uuid(), $2)
`, workspaceID, parseUUID(targetConnectionID)); err != nil {
		t.Fatalf("seed connection alias: %v", err)
	}
	t.Cleanup(func() {
		_, _ = testPool.Exec(context.Background(), `DELETE FROM vcs_gitlab_migration_id_map WHERE workspace_id=$1 AND entity_kind='connection' AND unified_id=$2`, workspaceID, parseUUID(targetConnectionID))
	})
	rec := httptest.NewRecorder()
	testHandler.DeleteVCSConnection(rec, directGitLabProjectRequest(http.MethodDelete,
		"/api/workspaces/"+workspaceID+"/vcs/connections/"+targetConnectionID,
		workspaceID, nil, "connectionId", targetConnectionID))
	if rec.Code != http.StatusNoContent {
		t.Fatalf("DeleteVCSConnection: %d %s", rec.Code, rec.Body.String())
	}
	assertCount := func(label, query, id string, want int) {
		t.Helper()
		var got int
		if err := testPool.QueryRow(context.Background(), query, id).Scan(&got); err != nil {
			t.Fatalf("%s count: %v", label, err)
		}
		if got != want {
			t.Fatalf("%s count = %d, want %d", label, got, want)
		}
	}
	for _, deleted := range []struct {
		label, query, id string
	}{
		{"target connection", `SELECT count(*) FROM vcs_connection WHERE id=$1`, targetConnectionID},
		{"target project", `SELECT count(*) FROM vcs_gitlab_project WHERE id=$1`, targetProjectID},
		{"target pull request", `SELECT count(*) FROM vcs_pull_request WHERE id=$1`, targetPR},
		{"target issue link", `SELECT count(*) FROM issue_vcs_pull_request WHERE pull_request_id=$1`, targetPR},
		{"target detail", `SELECT count(*) FROM vcs_gitlab_mr_detail WHERE vcs_pull_request_id=$1`, targetPR},
		{"target approval", `SELECT count(*) FROM gitlab_mr_approval_state WHERE vcs_pull_request_id=$1`, targetPR},
		{"target discussion", `SELECT count(*) FROM gitlab_mr_discussion WHERE vcs_pull_request_id=$1`, targetPR},
		{"target note", `SELECT count(*) FROM gitlab_mr_note WHERE vcs_pull_request_id=$1`, targetPR},
		{"target job", `SELECT count(*) FROM gitlab_pipeline_job WHERE vcs_pull_request_id=$1`, targetPR},
		{"target pipeline", `SELECT count(*) FROM vcs_gitlab_pipeline WHERE pull_request_id=$1`, targetPR},
		{"target migration alias", `SELECT count(*) FROM vcs_gitlab_migration_id_map WHERE entity_kind='pull_request' AND unified_id=$1`, targetPR},
		{"target connection alias", `SELECT count(*) FROM vcs_gitlab_migration_id_map WHERE entity_kind='connection' AND unified_id=$1`, targetConnectionID},
	} {
		assertCount(deleted.label, deleted.query, deleted.id, 0)
	}
	assertCount("other connection", `SELECT count(*) FROM vcs_connection WHERE id=$1`, otherConnectionID, 1)
	assertCount("other project", `SELECT count(*) FROM vcs_gitlab_project WHERE id=$1`, otherProjectID, 1)
	assertCount("other pull request", `SELECT count(*) FROM vcs_pull_request WHERE id=$1`, otherPR, 1)
	assertCount("other detail", `SELECT count(*) FROM vcs_gitlab_mr_detail WHERE vcs_pull_request_id=$1`, otherPR, 1)
	assertCount("foreign connection", `SELECT count(*) FROM vcs_connection WHERE id=$1`, foreignConnectionID, 1)
	assertCount("foreign project", `SELECT count(*) FROM vcs_gitlab_project WHERE id=$1`, foreignProjectID, 1)
	assertCount("foreign pull request", `SELECT count(*) FROM vcs_pull_request WHERE id=$1`, foreignPR, 1)
	assertCount("foreign detail", `SELECT count(*) FROM vcs_gitlab_mr_detail WHERE vcs_pull_request_id=$1`, foreignPR, 1)
}

func seedGitLabVCSConnectionInWorkspace(t *testing.T, workspaceID, instanceURL string) string {
	t.Helper()
	ctx := context.Background()
	tokenEncrypted, err := testHandler.sealVCSSecret("cleanup-token")
	if err != nil {
		t.Fatalf("encrypt connection token: %v", err)
	}
	webhookSecretEncrypted, err := testHandler.sealVCSSecret("cleanup-webhook-secret")
	if err != nil {
		t.Fatalf("encrypt connection webhook secret: %v", err)
	}
	connection, err := testHandler.Queries.UpsertVCSConnection(ctx, db.UpsertVCSConnectionParams{
		WorkspaceID:            parseUUID(workspaceID),
		Provider:               "gitlab",
		InstanceUrl:            instanceURL,
		AccountLogin:           "cleanup-test-user",
		AccessTokenEncrypted:   tokenEncrypted,
		WebhookSecretEncrypted: webhookSecretEncrypted,
		ConnectedByID:          pgtype.UUID{},
	})
	if err != nil {
		t.Fatalf("seed cleanup connection: %v", err)
	}
	t.Cleanup(func() {
		_, _ = testPool.Exec(ctx, `DELETE FROM vcs_connection WHERE id=$1`, connection.ID)
	})
	return uuidToString(connection.ID)
}

func createGitLabCleanupWorkspace(t *testing.T) string {
	t.Helper()
	ctx := context.Background()
	suffix := randomID()
	var workspaceID string
	if err := testPool.QueryRow(ctx, `
INSERT INTO workspace (name, slug, description, issue_prefix)
VALUES ($1, $2, $3, $4)
RETURNING id::text
`, "GitLab cleanup "+suffix, "gitlab-cleanup-"+suffix, "GitLab dependency cleanup fixture", "GC"+suffix[:6]).Scan(&workspaceID); err != nil {
		t.Fatalf("create cleanup workspace: %v", err)
	}
	t.Cleanup(func() {
		_, _ = testPool.Exec(ctx, `DELETE FROM workspace WHERE id=$1`, workspaceID)
	})
	return workspaceID
}

func seedGitLabDeletionMR(t *testing.T, workspaceID, connectionID, path string, number int32, officialLink bool) (string, string) {
	t.Helper()
	ctx := context.Background()
	owner, repo, ok := strings.Cut(path, "/")
	if !ok || owner == "" || repo == "" {
		t.Fatalf("invalid cleanup project path %q", path)
	}
	var pullRequestID string
	if err := testPool.QueryRow(ctx, `
INSERT INTO vcs_pull_request (
    workspace_id, connection_id, provider, repo_owner, repo_name, pr_number,
    title, state, html_url, pr_created_at, pr_updated_at, additions, deletions,
    changed_files, head_sha
) VALUES (
    $1, $2, 'gitlab', $3, $4, $5, 'cleanup MR', 'open', 'https://gitlab.example/cleanup',
    now(), now(), 0, 0, 0, 'cleanup-sha'
)
RETURNING id::text
`, workspaceID, connectionID, owner, repo, number).Scan(&pullRequestID); err != nil {
		t.Fatalf("seed cleanup MR: %v", err)
	}
	var issueNumber int
	if err := testPool.QueryRow(ctx, `
UPDATE workspace
SET issue_counter = GREATEST(issue_counter, (SELECT COALESCE(MAX(number), 0) FROM issue WHERE workspace_id=$1)) + 1
WHERE id=$1
RETURNING issue_counter
`, workspaceID).Scan(&issueNumber); err != nil {
		t.Fatalf("next cleanup issue number: %v", err)
	}
	var issueID string
	if err := testPool.QueryRow(ctx, `
INSERT INTO issue (workspace_id, title, status, priority, creator_id, creator_type, number)
VALUES ($1, $2, 'todo', 'medium', $3, 'member', $4)
RETURNING id::text
`, workspaceID, "GitLab cleanup "+randomID(), testUserID, issueNumber).Scan(&issueID); err != nil {
		t.Fatalf("seed cleanup issue: %v", err)
	}
	if _, err := testPool.Exec(ctx, `
INSERT INTO issue_vcs_pull_request (
    issue_id, pull_request_id, linked_by_type, close_intent, vcs_linked,
    gitlab_close_intent, gitlab_linked_by_type, gitlab_linked_at
) VALUES ($1, $2, 'member', TRUE, $3, TRUE, 'system', now())
`, issueID, pullRequestID, officialLink); err != nil {
		t.Fatalf("seed cleanup issue link: %v", err)
	}
	if _, err := testPool.Exec(ctx, `INSERT INTO vcs_gitlab_mr_detail (vcs_pull_request_id, description) VALUES ($1, 'cleanup detail')`, pullRequestID); err != nil {
		t.Fatalf("seed cleanup detail: %v", err)
	}
	if _, err := testPool.Exec(ctx, `
INSERT INTO gitlab_mr_approval_state (workspace_id, vcs_pull_request_id, approved)
VALUES ($1, $2, TRUE)
`, workspaceID, pullRequestID); err != nil {
		t.Fatalf("seed cleanup approval: %v", err)
	}
	var discussionID string
	if err := testPool.QueryRow(ctx, `
INSERT INTO gitlab_mr_discussion (workspace_id, vcs_pull_request_id, gitlab_discussion_id)
VALUES ($1, $2, $3)
RETURNING id::text
`, workspaceID, pullRequestID, "cleanup-discussion-"+randomID()).Scan(&discussionID); err != nil {
		t.Fatalf("seed cleanup discussion: %v", err)
	}
	if _, err := testPool.Exec(ctx, `
INSERT INTO gitlab_mr_note (workspace_id, discussion_id, vcs_pull_request_id, gitlab_note_id, body)
VALUES ($1, $2, $3, $4, 'cleanup note')
`, workspaceID, discussionID, pullRequestID, int64(number)+1000); err != nil {
		t.Fatalf("seed cleanup note: %v", err)
	}
	if _, err := testPool.Exec(ctx, `
INSERT INTO gitlab_pipeline_job (workspace_id, vcs_pull_request_id, pipeline_id, job_id, name, status)
VALUES ($1, $2, $3, $4, 'cleanup-job', 'failed')
`, workspaceID, pullRequestID, int64(number)+2000, int64(number)+3000); err != nil {
		t.Fatalf("seed cleanup job: %v", err)
	}
	if _, err := testPool.Exec(ctx, `
INSERT INTO vcs_gitlab_pipeline (
    workspace_id, connection_id, pull_request_id, pipeline_id, pipeline_project_id,
    pipeline_project_path, sha, status, pipeline_updated_at
) VALUES ($1, $2, $3, $4, $5, $6, 'cleanup-sha', 'failed', now())
`, workspaceID, connectionID, pullRequestID, int64(number)+2000, int64(number)+4000, path); err != nil {
		t.Fatalf("seed cleanup pipeline: %v", err)
	}
	if _, err := testPool.Exec(ctx, `
INSERT INTO vcs_gitlab_migration_id_map (workspace_id, entity_kind, legacy_id, unified_id)
VALUES ($1, 'pull_request', gen_random_uuid(), $2)
`, workspaceID, pullRequestID); err != nil {
		t.Fatalf("seed cleanup PR alias: %v", err)
	}
	t.Cleanup(func() {
		_, _ = testPool.Exec(ctx, `DELETE FROM gitlab_mr_note WHERE vcs_pull_request_id=$1`, pullRequestID)
		_, _ = testPool.Exec(ctx, `DELETE FROM gitlab_mr_discussion WHERE vcs_pull_request_id=$1`, pullRequestID)
		_, _ = testPool.Exec(ctx, `DELETE FROM gitlab_mr_approval_state WHERE vcs_pull_request_id=$1`, pullRequestID)
		_, _ = testPool.Exec(ctx, `DELETE FROM gitlab_pipeline_job WHERE vcs_pull_request_id=$1`, pullRequestID)
		_, _ = testPool.Exec(ctx, `DELETE FROM vcs_gitlab_pipeline WHERE pull_request_id=$1`, pullRequestID)
		_, _ = testPool.Exec(ctx, `DELETE FROM vcs_gitlab_mr_detail WHERE vcs_pull_request_id=$1`, pullRequestID)
		_, _ = testPool.Exec(ctx, `DELETE FROM issue_vcs_pull_request WHERE pull_request_id=$1`, pullRequestID)
		_, _ = testPool.Exec(ctx, `DELETE FROM vcs_gitlab_migration_id_map WHERE entity_kind='pull_request' AND unified_id=$1`, pullRequestID)
		_, _ = testPool.Exec(ctx, `DELETE FROM vcs_pull_request WHERE id=$1`, pullRequestID)
		_, _ = testPool.Exec(ctx, `DELETE FROM issue WHERE id=$1`, issueID)
	})
	return pullRequestID, issueID
}

func TestLegacyGitLabProjectCreateRequiresExistingLegacyConnection(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("handler test fixture not initialized (no DB?)")
	}
	_ = withVCSBox(t)
	baseURL := "https://gitlab-legacy-security.example"
	connectionID := seedGitLabVCSConnectionInWorkspace(t, testWorkspaceID, baseURL)
	t.Setenv("GITLAB_BASE_URL", baseURL)
	t.Setenv("GITLAB_TOKEN", "process-global-token")
	t.Setenv("GITLAB_WEBHOOK_SECRET", "process-global-webhook-secret")
	var beforeToken, beforeSecret string
	if err := testPool.QueryRow(context.Background(), `SELECT access_token_encrypted, webhook_secret_encrypted FROM vcs_connection WHERE id=$1`, connectionID).Scan(&beforeToken, &beforeSecret); err != nil {
		t.Fatalf("read canonical credentials before legacy request: %v", err)
	}
	fake := &canonicalGitLabProjectAPI{}
	installCanonicalGitLabProjectAPI(t, fake)
	previousValidator := validateGitLabVCSConnectionToken
	validateCalls := 0
	validateGitLabVCSConnectionToken = func(context.Context, string, string) (vcs.Account, error) {
		validateCalls++
		return vcs.Account{Login: "process-global-user"}, nil
	}
	t.Cleanup(func() { validateGitLabVCSConnectionToken = previousValidator })

	rec := httptest.NewRecorder()
	testHandler.CreateGitLabProjectBinding(rec, directGitLabProjectRequest(http.MethodPost,
		"/api/workspaces/"+testWorkspaceID+"/gitlab/projects", testWorkspaceID,
		map[string]any{"project": ""}, "", ""))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("legacy create without a legacy-owned connection: want 404, got %d (%s)", rec.Code, rec.Body.String())
	}
	if validateCalls != 0 || fake.projectPath != "" {
		t.Fatalf("legacy request used global credentials or reached GitLab: validates=%d project=%q", validateCalls, fake.projectPath)
	}
	var afterToken, afterSecret string
	if err := testPool.QueryRow(context.Background(), `SELECT access_token_encrypted, webhook_secret_encrypted FROM vcs_connection WHERE id=$1`, connectionID).Scan(&afterToken, &afterSecret); err != nil {
		t.Fatalf("read canonical credentials after legacy request: %v", err)
	}
	if afterToken != beforeToken || afterSecret != beforeSecret {
		t.Fatal("legacy project request overwrote canonical credentials")
	}
}

func TestLegacyGitLabProjectCreateResolvesOnlyWorkspaceAliasAndUsesCanonicalSecrets(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("handler test fixture not initialized (no DB?)")
	}
	_ = withVCSBox(t)
	baseURL := "https://gitlab-legacy-alias.example/subpath"
	connectionID := seedGitLabVCSConnectionInWorkspace(t, testWorkspaceID, baseURL)
	var legacyID string
	if err := testPool.QueryRow(context.Background(), `SELECT gen_random_uuid()::text`).Scan(&legacyID); err != nil {
		t.Fatalf("allocate legacy connection ID: %v", err)
	}
	insertGitLabConnectionAlias(t, testWorkspaceID, legacyID, connectionID)
	t.Setenv("GITLAB_BASE_URL", baseURL)
	t.Setenv("GITLAB_TOKEN", "process-global-token")
	t.Setenv("GITLAB_WEBHOOK_SECRET", "process-global-webhook-secret")
	t.Setenv("GITLAB_PROXY_URL", "socks5h://proxy.example:1080")
	withPublicURLForGitLabTest(t, "")
	fake := &canonicalGitLabProjectAPI{project: gitlab.Project{
		ID:                90901,
		PathWithNamespace: "group/legacy",
		WebURL:            "https://gitlab-legacy-alias.example/subpath/group/legacy",
	}}
	installCanonicalGitLabProjectAPI(t, fake)

	foreignWorkspaceID := "00000000-0000-0000-0000-000000000099"
	foreign := httptest.NewRecorder()
	testHandler.CreateGitLabProjectBinding(foreign, directGitLabProjectRequest(http.MethodPost,
		"/api/workspaces/"+foreignWorkspaceID+"/gitlab/projects", foreignWorkspaceID,
		map[string]any{"connection_id": legacyID, "project": "group/legacy"}, "", ""))
	if foreign.Code != http.StatusNotFound {
		t.Fatalf("cross-workspace legacy alias: want 404, got %d (%s)", foreign.Code, foreign.Body.String())
	}
	if fake.projectPath != "" {
		t.Fatalf("cross-workspace alias reached GitLab project %q", fake.projectPath)
	}

	rec := httptest.NewRecorder()
	testHandler.CreateGitLabProjectBinding(rec, directGitLabProjectRequest(http.MethodPost,
		"/api/workspaces/"+testWorkspaceID+"/gitlab/projects", testWorkspaceID,
		map[string]any{"connection_id": legacyID, "project": "group/legacy"}, "", ""))
	if rec.Code != http.StatusOK {
		t.Fatalf("workspace-owned legacy alias: %d %s", rec.Code, rec.Body.String())
	}
	if fake.config.Token != "cleanup-token" || fake.config.WebhookSecret != "cleanup-webhook-secret" || fake.config.ProxyURL != "socks5h://proxy.example:1080" {
		t.Fatalf("legacy adapter credentials = token %q secret %q proxy %q", fake.config.Token, fake.config.WebhookSecret, fake.config.ProxyURL)
	}
}

func TestLegacyGitLabConnectionAliasIsIdempotentAndPreservesMigratedAlias(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("handler test fixture not initialized (no DB?)")
	}
	_ = withVCSBox(t)
	migratedConnectionID := seedGitLabVCSConnectionInWorkspace(t, testWorkspaceID, "https://gitlab-migrated-alias.example")
	var legacyID string
	if err := testPool.QueryRow(context.Background(), `SELECT gen_random_uuid()::text`).Scan(&legacyID); err != nil {
		t.Fatalf("allocate legacy connection ID: %v", err)
	}
	insertGitLabConnectionAlias(t, testWorkspaceID, legacyID, migratedConnectionID)
	migratedParams := db.EnsureLegacyGitLabVCSConnectionAliasParams{
		WorkspaceID:  parseUUID(testWorkspaceID),
		ConnectionID: parseUUID(migratedConnectionID),
	}
	for i := 0; i < 2; i++ {
		if err := testHandler.Queries.EnsureLegacyGitLabVCSConnectionAlias(context.Background(), migratedParams); err != nil {
			t.Fatalf("preserve migrated connection alias: %v", err)
		}
	}
	var aliasCount int
	var actualLegacyID string
	if err := testPool.QueryRow(context.Background(), `
SELECT count(*), min(legacy_id::text)
FROM vcs_gitlab_migration_id_map
WHERE workspace_id=$1 AND entity_kind='connection' AND unified_id=$2
`, testWorkspaceID, parseUUID(migratedConnectionID)).Scan(&aliasCount, &actualLegacyID); err != nil {
		t.Fatalf("read migrated connection aliases: %v", err)
	}
	if aliasCount != 1 || actualLegacyID != legacyID {
		t.Fatalf("existing alias changed: count=%d legacy_id=%q want %q", aliasCount, actualLegacyID, legacyID)
	}

	unaliasedConnectionID := seedGitLabVCSConnectionInWorkspace(t, testWorkspaceID, "https://gitlab-self-alias.example")
	selfAliasParams := db.EnsureLegacyGitLabVCSConnectionAliasParams{
		WorkspaceID:  parseUUID(testWorkspaceID),
		ConnectionID: parseUUID(unaliasedConnectionID),
	}
	t.Cleanup(func() {
		_, _ = testPool.Exec(context.Background(), `
DELETE FROM vcs_gitlab_migration_id_map
WHERE workspace_id=$1 AND entity_kind='connection' AND unified_id=$2
`, testWorkspaceID, parseUUID(unaliasedConnectionID))
	})
	for i := 0; i < 2; i++ {
		if err := testHandler.Queries.EnsureLegacyGitLabVCSConnectionAlias(context.Background(), selfAliasParams); err != nil {
			t.Fatalf("create self alias: %v", err)
		}
	}
	if err := testPool.QueryRow(context.Background(), `
SELECT count(*), min(legacy_id::text)
FROM vcs_gitlab_migration_id_map
WHERE workspace_id=$1 AND entity_kind='connection' AND unified_id=$2
`, testWorkspaceID, parseUUID(unaliasedConnectionID)).Scan(&aliasCount, &actualLegacyID); err != nil {
		t.Fatalf("read self alias: %v", err)
	}
	if aliasCount != 1 || actualLegacyID != unaliasedConnectionID {
		t.Fatalf("self alias is not idempotent: count=%d legacy_id=%q want %q", aliasCount, actualLegacyID, unaliasedConnectionID)
	}
}

func TestGitLabCanonicalConnectionRejectsMissingEncryptedCredentialsDespiteEnvironment(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("handler test fixture not initialized (no DB?)")
	}
	_ = withVCSBox(t)
	baseURL := "https://gitlab-missing-secrets.example"
	connectionID := seedGitLabVCSConnectionInWorkspace(t, testWorkspaceID, baseURL)
	t.Setenv("GITLAB_BASE_URL", baseURL)
	t.Setenv("GITLAB_TOKEN", "process-global-token")
	t.Setenv("GITLAB_WEBHOOK_SECRET", "process-global-webhook-secret")
	var originalToken, originalSecret string
	if err := testPool.QueryRow(context.Background(), `SELECT access_token_encrypted, webhook_secret_encrypted FROM vcs_connection WHERE id=$1`, connectionID).Scan(&originalToken, &originalSecret); err != nil {
		t.Fatalf("read canonical credentials: %v", err)
	}
	for _, column := range []string{"access_token_encrypted", "webhook_secret_encrypted"} {
		if _, err := testPool.Exec(context.Background(), `UPDATE vcs_connection SET access_token_encrypted=$2, webhook_secret_encrypted=$3 WHERE id=$1`, connectionID, originalToken, originalSecret); err != nil {
			t.Fatalf("restore canonical credentials: %v", err)
		}
		if _, err := testPool.Exec(context.Background(), `UPDATE vcs_connection SET `+column+`='' WHERE id=$1`, connectionID); err != nil {
			t.Fatalf("clear canonical %s: %v", column, err)
		}
		connection, err := testHandler.Queries.GetVCSGitLabConnectionByIDAndWorkspace(context.Background(), db.GetVCSGitLabConnectionByIDAndWorkspaceParams{
			ID:          parseUUID(connectionID),
			WorkspaceID: parseUUID(testWorkspaceID),
		})
		if err != nil {
			t.Fatalf("load connection with missing %s: %v", column, err)
		}
		if _, err := testHandler.gitLabConfigForVCSConnectionRow(connection); !errors.Is(err, errGitLabConnectionUnavailable) {
			t.Fatalf("missing canonical %s accepted despite matching process environment: %v", column, err)
		}
	}
	if _, err := testPool.Exec(context.Background(), `UPDATE vcs_connection SET access_token_encrypted='invalid-ciphertext' WHERE id=$1`, connectionID); err != nil {
		t.Fatalf("corrupt canonical access token: %v", err)
	}
	connection, err := testHandler.Queries.GetVCSGitLabConnectionByIDAndWorkspace(context.Background(), db.GetVCSGitLabConnectionByIDAndWorkspaceParams{
		ID:          parseUUID(connectionID),
		WorkspaceID: parseUUID(testWorkspaceID),
	})
	if err != nil {
		t.Fatalf("load connection with invalid access token: %v", err)
	}
	config, err := testHandler.gitLabConfigForVCSConnectionRow(connection)
	if err == nil || config.Token != "" || config.WebhookSecret != "" {
		t.Fatalf("invalid encrypted token did not fail closed despite process environment: config=%+v err=%v", config, err)
	}
}

func insertGitLabConnectionAlias(t *testing.T, workspaceID, legacyID, connectionID string) {
	t.Helper()
	if _, err := testPool.Exec(context.Background(), `
INSERT INTO vcs_gitlab_migration_id_map (workspace_id, entity_kind, legacy_id, unified_id)
VALUES ($1, 'connection', $2, $3)
`, workspaceID, parseUUID(legacyID), parseUUID(connectionID)); err != nil {
		t.Fatalf("seed legacy GitLab connection alias: %v", err)
	}
	t.Cleanup(func() {
		_, _ = testPool.Exec(context.Background(), `
DELETE FROM vcs_gitlab_migration_id_map
WHERE workspace_id=$1 AND entity_kind='connection' AND legacy_id=$2
`, workspaceID, parseUUID(legacyID))
	})
}
