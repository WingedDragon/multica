package handler

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/events"
	"github.com/multica-ai/multica/server/internal/integrations/gitlab"
	"github.com/multica-ai/multica/server/internal/middleware"
	"github.com/multica-ai/multica/server/internal/util/secretbox"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/multica-ai/multica/server/pkg/protocol"
)

type fakeGitLabAPI struct {
	project            gitlab.Project
	projectLookupPaths []string
	createHookID       int64
	createHookCalls    int
	deleteHookCalls    int
	deleteErr          error
	createHookErr      error
	validationErr      error
	validationCalls    int
	clientConfig       gitlab.Config
}

func (f *fakeGitLabAPI) ValidateCredentials(context.Context) error {
	f.validationCalls++
	return f.validationErr
}

func (f *fakeGitLabAPI) GetProject(_ context.Context, path string) (gitlab.Project, error) {
	f.projectLookupPaths = append(f.projectLookupPaths, path)
	return f.project, nil
}

func (f *fakeGitLabAPI) CreateProjectHook(context.Context, int64, string, string) (gitlab.ProjectHook, error) {
	f.createHookCalls++
	if f.createHookErr != nil {
		return gitlab.ProjectHook{}, f.createHookErr
	}
	return gitlab.ProjectHook{ID: f.createHookID}, nil
}

func (f *fakeGitLabAPI) DeleteProjectHook(context.Context, int64, int64) error {
	f.deleteHookCalls++
	return f.deleteErr
}

var gitLabClientOverrideForTest bool

type fakeGitLabDetailAPI struct {
	fakeGitLabAPI
	mr             gitlab.MergeRequest
	mrErr          error
	changes        gitlab.MergeRequestChanges
	changesErr     error
	approval       gitlab.ApprovalState
	approvalErr    error
	discussions    []gitlab.Discussion
	discussionsErr error
	mrPipelines    []gitlab.Pipeline
	mrPipelinesErr error
	pipelines      []gitlab.Pipeline
	pipelinesErr   error
	jobs           []gitlab.Job
	jobsErr        error
	trace          gitlab.JobTrace
	traceErr       error
	artifactsResp  *http.Response
	artifactsErr   error
	mergeCalled    bool
	mergeProjectID int64
	mergeIID       int32
	mergeOptions   gitlab.MergeRequestMergeOptions
	mergeErr       error
}

func (f *fakeGitLabDetailAPI) GetMergeRequest(context.Context, int64, int32) (gitlab.MergeRequest, error) {
	if f.mrErr != nil {
		return gitlab.MergeRequest{}, f.mrErr
	}
	return f.mr, nil
}

func (f *fakeGitLabDetailAPI) GetMergeRequestChanges(context.Context, int64, int32) (gitlab.MergeRequestChanges, error) {
	if f.changesErr != nil {
		return gitlab.MergeRequestChanges{}, f.changesErr
	}
	return f.changes, nil
}

func (f *fakeGitLabDetailAPI) GetMergeRequestApprovalState(context.Context, int64, int32) (gitlab.ApprovalState, error) {
	if f.approvalErr != nil {
		return gitlab.ApprovalState{}, f.approvalErr
	}
	return f.approval, nil
}

func (f *fakeGitLabDetailAPI) ListMergeRequestDiscussions(context.Context, int64, int32) ([]gitlab.Discussion, error) {
	if f.discussionsErr != nil {
		return nil, f.discussionsErr
	}
	return f.discussions, nil
}

func (f *fakeGitLabDetailAPI) ListProjectPipelines(context.Context, int64, gitlab.PipelineFilters) ([]gitlab.Pipeline, error) {
	if f.pipelinesErr != nil {
		return nil, f.pipelinesErr
	}
	return f.pipelines, nil
}

func (f *fakeGitLabDetailAPI) ListMergeRequestPipelines(context.Context, int64, int32) ([]gitlab.Pipeline, error) {
	if f.mrPipelinesErr != nil {
		return nil, f.mrPipelinesErr
	}
	return f.mrPipelines, nil
}

func (f *fakeGitLabDetailAPI) GetPipeline(context.Context, int64, int64) (gitlab.Pipeline, error) {
	if len(f.pipelines) == 0 {
		return gitlab.Pipeline{}, errors.New("not found")
	}
	return f.pipelines[0], nil
}

func (f *fakeGitLabDetailAPI) ListPipelineJobs(context.Context, int64, int64) ([]gitlab.Job, error) {
	if f.jobsErr != nil {
		return nil, f.jobsErr
	}
	return f.jobs, nil
}

func (f *fakeGitLabDetailAPI) GetJobTrace(context.Context, int64, int64, int) (gitlab.JobTrace, error) {
	if f.traceErr != nil {
		return gitlab.JobTrace{}, f.traceErr
	}
	return f.trace, nil
}

func (f *fakeGitLabDetailAPI) DownloadJobArtifacts(context.Context, int64, int64) (*http.Response, error) {
	if f.artifactsErr != nil {
		return nil, f.artifactsErr
	}
	if f.artifactsResp != nil {
		return f.artifactsResp, nil
	}
	return nil, errors.New("not used")
}

func (f *fakeGitLabDetailAPI) MergeMergeRequest(_ context.Context, projectID int64, iid int32, opts gitlab.MergeRequestMergeOptions) (gitlab.MergeRequest, error) {
	f.mergeCalled = true
	f.mergeProjectID = projectID
	f.mergeIID = iid
	f.mergeOptions = opts
	if f.mergeErr != nil {
		return gitlab.MergeRequest{}, f.mergeErr
	}
	return f.mr, nil
}

func installFakeGitLabClient(t *testing.T, fake *fakeGitLabAPI) {
	t.Helper()
	prev := newGitLabClient
	prevOverride := gitLabClientOverrideForTest
	gitLabClientOverrideForTest = true
	newGitLabClient = func(cfg gitlab.Config) (gitLabAPI, error) {
		fake.clientConfig = cfg
		return fake, nil
	}
	t.Cleanup(func() {
		newGitLabClient = prev
		gitLabClientOverrideForTest = prevOverride
	})
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
		_, _ = testPool.Exec(ctx, `DELETE FROM issue_vcs_pull_request WHERE issue_id = $1`, out.ID)
		_, _ = testPool.Exec(ctx, `DELETE FROM activity_log WHERE issue_id = $1`, out.ID)
		_, _ = testPool.Exec(ctx, `DELETE FROM issue WHERE id = $1`, out.ID)
	})
	return out
}

func installFakeGitLabDetailClient(t *testing.T, fake *fakeGitLabDetailAPI) {
	t.Helper()
	prev := newGitLabClient
	prevOverride := gitLabClientOverrideForTest
	gitLabClientOverrideForTest = true
	newGitLabClient = func(cfg gitlab.Config) (gitLabAPI, error) {
		fake.clientConfig = cfg
		return fake, nil
	}
	t.Cleanup(func() {
		newGitLabClient = prev
		gitLabClientOverrideForTest = prevOverride
	})
}

func installFakeGitLabProviderProxy(t *testing.T, connectionID string, projectID int64, path string, iid int32, state, branch, sha, updatedAt, mergedAt, closedAt string) chan string {
	t.Helper()
	requests := make(chan string, 16)
	remoteMR := map[string]any{
		"iid": iid, "state": state, "sha": sha, "source_branch": branch,
		"source_project_id": projectID,
		"source_project":    map[string]any{"id": projectID, "path_with_namespace": path},
		"target_project_id": projectID, "target_branch": "main",
		"detailed_merge_status": "mergeable", "has_conflicts": false,
		"updated_at": updatedAt, "merged_at": mergedAt, "closed_at": closedAt,
	}
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.Header.Get("PRIVATE-TOKEN") != "tok" {
			requests <- "invalid request"
			http.Error(w, "invalid GitLab provider request", http.StatusUnauthorized)
			return
		}
		requestPath := r.URL.Path
		var kind string
		var response any
		switch {
		case strings.HasSuffix(requestPath, fmt.Sprintf("/merge_requests/%d/pipelines", iid)):
			kind, response = "merge_request_pipelines", []any{}
		case strings.HasSuffix(requestPath, fmt.Sprintf("/merge_requests/%d", iid)):
			kind, response = "merge_request", remoteMR
		case strings.HasSuffix(requestPath, fmt.Sprintf("/projects/%d/pipelines", projectID)):
			kind, response = "source_pipelines", []any{}
		case strings.HasSuffix(requestPath, "/api/v4/projects/"+path):
			kind = "project"
			response = map[string]any{"id": projectID, "path_with_namespace": path}
		default:
			requests <- "unexpected: " + requestPath
			http.NotFound(w, r)
			return
		}
		requests <- kind
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(response)
	}))
	t.Cleanup(proxy.Close)
	t.Setenv("GITLAB_PROXY_URL", proxy.URL)
	if _, err := testPool.Exec(context.Background(), `
UPDATE vcs_connection
SET instance_url = 'http://gitlab-provider.invalid/root'
WHERE id = $1
`, parseUUID(connectionID)); err != nil {
		t.Fatalf("point GitLab VCS connection at fake provider: %v", err)
	}
	return requests
}

func assertGitLabProviderProxyRequests(t *testing.T, requests chan string) {
	t.Helper()
	close(requests)
	counts := make(map[string]int)
	for request := range requests {
		counts[request]++
	}
	for _, kind := range []string{"project", "merge_request", "merge_request_pipelines", "source_pipelines"} {
		if counts[kind] != 1 {
			t.Fatalf("unified GitLab provider requests = %v, want one %q request", counts, kind)
		}
	}
	if len(counts) != 4 {
		t.Fatalf("unified GitLab provider made unexpected requests: %v", counts)
	}
}

func fireGitLabMRWebhookWithFakeAPI(t *testing.T, connectionID string, projectID int64, path string, iid int32, title, description, branch, state, action, sha, updatedAt string) {
	t.Helper()
	mergedAt, closedAt := "", ""
	if state == "merged" {
		mergedAt = updatedAt
	}
	if state == "closed" {
		closedAt = updatedAt
	}
	providerRequests := installFakeGitLabProviderProxy(t, connectionID, projectID, path, iid, state, branch, sha, updatedAt, mergedAt, closedAt)
	hasConflicts := false
	project := gitlab.Project{
		ID: projectID, PathWithNamespace: path,
		WebURL: "https://code.mlamp.cn/" + path,
	}
	remoteMR := gitlab.MergeRequest{
		IID: iid, Title: title, Description: description, State: state,
		WebURL:       fmt.Sprintf("https://code.mlamp.cn/%s/-/merge_requests/%d", path, iid),
		SourceBranch: branch, TargetBranch: "main", SHA: sha,
		SourceProjectID: projectID, TargetProjectID: projectID, SourceProject: project,
		DetailedMergeStatus: "mergeable", HasConflicts: &hasConflicts,
		CreatedAt: "2026-04-28T00:00:00Z", UpdatedAt: updatedAt,
		MergedAt: mergedAt, ClosedAt: closedAt,
	}
	remoteMR.Author.Username = "gitlab-user"
	remoteMR.Author.AvatarURL = "https://code.mlamp.cn/avatar.png"
	installFakeGitLabDetailClient(t, &fakeGitLabDetailAPI{
		fakeGitLabAPI: fakeGitLabAPI{project: project},
		mr:            remoteMR,
	})
	fireVCSGitLabMRWebhook(t, connectionID, projectID, path, iid, title, description, branch, state, action, sha, updatedAt)
	assertGitLabProviderProxyRequests(t, providerRequests)
}

func TestGitLabLateSyncCannotOverwriteNewerHeadAndPipelines(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("handler test fixture not initialized (no DB?)")
	}
	ctx := context.Background()
	withVCSBox(t)
	const projectID int64 = 99120
	path := fmt.Sprintf("group/project-%d", projectID)
	connectionID := seedGitLabVCSWebhookProject(t, projectID, path, true)
	connectionUUID := parseUUID(connectionID)
	var pullRequestID pgtype.UUID
	if err := testPool.QueryRow(ctx, `
INSERT INTO vcs_pull_request (
    workspace_id, connection_id, provider, repo_owner, repo_name, pr_number,
    title, state, html_url, branch, pr_created_at, pr_updated_at,
    additions, deletions, changed_files, head_sha
) VALUES (
    $1, $2, 'gitlab', 'group', $3, 12, 'concurrent MR', 'open',
    $4, 'feature/concurrent', '2026-11-01T09:00:00Z', '2026-11-01T09:00:00Z',
    0, 0, 0, 'initial-head'
) RETURNING id
`, parseUUID(testWorkspaceID), connectionUUID, fmt.Sprintf("project-%d", projectID), "https://code.mlamp.cn/"+path+"/-/merge_requests/12").Scan(&pullRequestID); err != nil {
		t.Fatalf("seed canonical GitLab MR: %v", err)
	}
	if _, err := testHandler.Queries.EnsureVCSGitLabMRDetail(ctx, db.EnsureVCSGitLabMRDetailParams{
		WorkspaceID: parseUUID(testWorkspaceID), PullRequestID: pullRequestID,
	}); err != nil {
		t.Fatalf("seed canonical GitLab MR details: %v", err)
	}
	load := func() db.VcsPullRequest {
		t.Helper()
		pr, err := testHandler.Queries.GetVCSGitLabPullRequestByWakeupIdentity(ctx, db.GetVCSGitLabPullRequestByWakeupIdentityParams{
			WorkspaceID: parseUUID(testWorkspaceID), ConnectionID: connectionUUID,
			RepoOwner: "group", RepoName: fmt.Sprintf("project-%d", projectID), PrNumber: 12,
		})
		if err != nil {
			t.Fatalf("load canonical GitLab MR: %v", err)
		}
		return pr
	}
	setHead := func(sha, updatedAt string) {
		t.Helper()
		if _, err := testPool.Exec(ctx, `
UPDATE vcs_pull_request
SET head_sha = $2, pr_updated_at = $3
WHERE id = $1 AND workspace_id = $4
`, pullRequestID, sha, parseGitLabTimeRequired(updatedAt), parseUUID(testWorkspaceID)); err != nil {
			t.Fatalf("set canonical MR head %q: %v", sha, err)
		}
	}

	oldGeneration, err := testHandler.Queries.BeginGitLabVCSPullRequestWakeupSync(ctx, db.BeginGitLabVCSPullRequestWakeupSyncParams{
		ID: pullRequestID, WorkspaceID: parseUUID(testWorkspaceID),
	})
	if err != nil {
		t.Fatalf("begin old GitLab sync: %v", err)
	}
	setHead("older-head", "2026-11-01T10:00:00Z")
	oldMR := load()
	newGeneration, err := testHandler.Queries.BeginGitLabVCSPullRequestWakeupSync(ctx, db.BeginGitLabVCSPullRequestWakeupSyncParams{
		ID: pullRequestID, WorkspaceID: parseUUID(testWorkspaceID),
	})
	if err != nil {
		t.Fatalf("begin new GitLab sync: %v", err)
	}
	setHead("newer-head", "2026-11-01T11:00:00Z")
	newMR := load()
	if newGeneration <= oldGeneration || newMR.WakeupSyncGeneration != newGeneration {
		t.Fatalf("canonical MR generation = %d, old=%d new=%d", newMR.WakeupSyncGeneration, oldGeneration, newGeneration)
	}

	pipelineTime := parseGitLabTimeRequired("2026-11-01T11:01:00Z").Time
	newPipelines := []gitLabCanonicalVersionedPipeline{{
		pipeline: gitlab.Pipeline{
			ID: 202, ProjectID: projectID, SHA: "newer-head", Ref: "feature/concurrent",
			Status: "success", WebURL: "https://code.mlamp.cn/pipelines/202", UpdatedAt: "2026-11-01T11:01:00Z",
		},
		projectPath: path,
		updatedAt:   pipelineTime,
	}}
	persisted, err := testHandler.persistVCSGitLabMergeRequestPipelines(ctx, newMR, newPipelines)
	if err != nil || !persisted {
		t.Fatalf("new canonical Pipeline sync: persisted=%v err=%v", persisted, err)
	}
	if err := testHandler.markVCSGitLabMRPipelineSync(ctx, newMR, nil); err != nil {
		t.Fatalf("mark new canonical Pipeline sync: %v", err)
	}

	if err := testHandler.markVCSGitLabMRPipelineSync(ctx, oldMR, errors.New("late old Pipeline failure")); err != nil {
		t.Fatalf("mark late old canonical Pipeline failure: %v", err)
	}
	oldPipelines := []gitLabCanonicalVersionedPipeline{{
		pipeline: gitlab.Pipeline{
			ID: 101, ProjectID: projectID, SHA: "older-head", Ref: "feature/concurrent",
			Status: "failed", WebURL: "https://code.mlamp.cn/pipelines/101", UpdatedAt: "2026-11-01T10:01:00Z",
		},
		projectPath: path,
		updatedAt:   parseGitLabTimeRequired("2026-11-01T10:01:00Z").Time,
	}}
	persisted, err = testHandler.persistVCSGitLabMergeRequestPipelines(ctx, oldMR, oldPipelines)
	if err != nil || persisted {
		t.Fatalf("late old canonical Pipeline ledger: persisted=%v err=%v", persisted, err)
	}

	final := load()
	if final.HeadSha != "newer-head" || final.WakeupSyncGeneration != newGeneration ||
		!final.PipelineSyncAt.Valid || final.PipelineSyncError.Valid {
		t.Fatalf("newer canonical MR/Pipeline state was overwritten: %+v", final)
	}
	var pipelineID, pipelineProjectID int64
	var pipelineSHA string
	if err := testPool.QueryRow(ctx, `
SELECT pipeline_id, pipeline_project_id, sha
FROM vcs_gitlab_pipeline
WHERE pull_request_id = $1
`, pullRequestID).Scan(&pipelineID, &pipelineProjectID, &pipelineSHA); err != nil {
		t.Fatalf("read canonical Pipeline ledger: %v", err)
	}
	if pipelineID != 202 || pipelineProjectID != projectID || pipelineSHA != "newer-head" {
		t.Fatalf("canonical Pipeline ledger = (%d, %d, %q), want (202, %d, newer-head)", pipelineID, pipelineProjectID, pipelineSHA, projectID)
	}
}

func TestVCSGitLabDetailsUsesCanonicalPullRequestID(t *testing.T) {
	if testHandler == nil {
		t.Skip("handler test fixture not initialized (no DB?)")
	}
	ctx := context.Background()
	const projectID int64 = 299101
	issue := createGitLabTestIssue(t, "canonical gitlab details", "in_progress")
	var connectionID, prID string
	instanceURL := "https://gitlab-details-" + strings.ReplaceAll(issue.ID, "-", "") + ".example.test"
	if err := testPool.QueryRow(ctx, `
INSERT INTO vcs_connection (workspace_id, provider, instance_url, account_login, access_token_encrypted, webhook_secret_encrypted)
VALUES ($1, 'gitlab', $2, 'test-user', '', '')
RETURNING id::text`, issue.WorkspaceID, instanceURL).Scan(&connectionID); err != nil {
		t.Fatalf("insert VCS connection: %v", err)
	}
	if _, err := testPool.Exec(ctx, `
INSERT INTO vcs_gitlab_project (workspace_id, connection_id, gitlab_project_id, path_with_namespace, web_url)
VALUES ($1, $2, $3, 'group/subgroup/project', $4)`,
		issue.WorkspaceID, connectionID, projectID, instanceURL+"/group/subgroup/project"); err != nil {
		t.Fatalf("insert unified GitLab project: %v", err)
	}
	if err := testPool.QueryRow(ctx, `
INSERT INTO vcs_pull_request (
	workspace_id, connection_id, provider, repo_owner, repo_name, pr_number,
	title, state, html_url, branch, head_sha, pr_created_at, pr_updated_at
) VALUES (
	$1, $2, 'gitlab', 'group/subgroup', 'project', 17,
	'Canonical MR', 'open', $3, 'feature/canonical', 'head-17', now(), now()
)
RETURNING id::text`, issue.WorkspaceID, connectionID, instanceURL+"/group/subgroup/project/-/merge_requests/17").Scan(&prID); err != nil {
		t.Fatalf("insert canonical MR: %v", err)
	}
	if _, err := testPool.Exec(ctx, `INSERT INTO vcs_gitlab_mr_detail (vcs_pull_request_id, target_branch) VALUES ($1, 'main')`, prID); err != nil {
		t.Fatalf("insert GitLab MR details: %v", err)
	}
	if _, err := testPool.Exec(ctx, `INSERT INTO issue_vcs_pull_request (issue_id, pull_request_id) VALUES ($1, $2)`, issue.ID, prID); err != nil {
		t.Fatalf("link issue to canonical MR: %v", err)
	}
	t.Cleanup(func() {
		_, _ = testPool.Exec(context.Background(), `DELETE FROM issue_vcs_pull_request WHERE pull_request_id = $1`, prID)
		_, _ = testPool.Exec(context.Background(), `DELETE FROM vcs_gitlab_pipeline WHERE pull_request_id = $1`, prID)
		_, _ = testPool.Exec(context.Background(), `DELETE FROM vcs_gitlab_mr_detail WHERE vcs_pull_request_id = $1`, prID)
		_, _ = testPool.Exec(context.Background(), `DELETE FROM vcs_pull_request WHERE id = $1`, prID)
		_, _ = testPool.Exec(context.Background(), `DELETE FROM vcs_gitlab_project WHERE workspace_id = $1 AND connection_id = $2 AND gitlab_project_id = $3`, issue.WorkspaceID, connectionID, projectID)
		_, _ = testPool.Exec(context.Background(), `DELETE FROM vcs_connection WHERE id = $1`, connectionID)
	})

	rec := httptest.NewRecorder()
	req := newRequest(http.MethodGet, "/api/issues/"+issue.ID+"/vcs/pull-requests/"+prID+"/gitlab", nil)
	req = withURLParams(req, "id", issue.ID, "prId", prID)
	testHandler.GetVCSGitLabMergeRequestDetails(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d (%s)", rec.Code, rec.Body.String())
	}
	var out struct {
		MergeRequest struct {
			ID          string `json:"id"`
			ProjectPath string `json:"project_path"`
			IID         int32  `json:"iid"`
		} `json:"merge_request"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if out.MergeRequest.ID != prID || out.MergeRequest.ProjectPath != "group/subgroup/project" || out.MergeRequest.IID != 17 {
		t.Fatalf("merge_request identity = %+v, want canonical id %s and group/subgroup/project#17", out.MergeRequest, prID)
	}
}
func TestLegacyGitLabDetailsResolveMigrationIDWithinIssueWorkspace(t *testing.T) {
	if testHandler == nil {
		t.Skip("handler test fixture not initialized (no DB?)")
	}
	ctx := context.Background()
	newWorkspace := func(name string) string {
		t.Helper()
		var suffix, workspaceID string
		if err := testPool.QueryRow(ctx, `SELECT gen_random_uuid()::text`).Scan(&suffix); err != nil {
			t.Fatalf("generate workspace suffix: %v", err)
		}
		slug := "gitlab-mr-alias-" + strings.ReplaceAll(suffix, "-", "")
		if err := testPool.QueryRow(ctx, `INSERT INTO workspace (name, slug) VALUES ($1, $2) RETURNING id::text`, name, slug).Scan(&workspaceID); err != nil {
			t.Fatalf("create workspace: %v", err)
		}
		if _, err := testPool.Exec(ctx, `INSERT INTO member (workspace_id, user_id, role) VALUES ($1, $2, 'owner')`, workspaceID, testUserID); err != nil {
			t.Fatalf("add workspace owner: %v", err)
		}
		t.Cleanup(func() {
			_, _ = testPool.Exec(context.Background(), `DELETE FROM workspace WHERE id = $1`, workspaceID)
		})
		return workspaceID
	}
	workspaceID := newWorkspace("GitLab alias source")
	otherWorkspaceID := newWorkspace("GitLab alias other")
	issue := createGitLabIssueInWorkspace(t, workspaceID, "aliased GitLab details", "in_progress")
	otherIssue := createGitLabIssueInWorkspace(t, otherWorkspaceID, "unrelated GitLab details", "in_progress")

	var legacyID string
	if err := testPool.QueryRow(ctx, `SELECT gen_random_uuid()::text`).Scan(&legacyID); err != nil {
		t.Fatalf("generate legacy MR ID: %v", err)
	}
	instanceURL := "https://gitlab-alias-" + strings.ReplaceAll(issue.ID, "-", "") + ".example.test"
	var connectionID, prID string
	if err := testPool.QueryRow(ctx, `
INSERT INTO vcs_connection (workspace_id, provider, instance_url, account_login, access_token_encrypted, webhook_secret_encrypted)
VALUES ($1, 'gitlab', $2, 'test-user', '', '')
RETURNING id::text`, workspaceID, instanceURL).Scan(&connectionID); err != nil {
		t.Fatalf("insert VCS connection: %v", err)
	}
	if _, err := testPool.Exec(ctx, `
INSERT INTO vcs_gitlab_project (workspace_id, connection_id, gitlab_project_id, path_with_namespace, web_url)
VALUES ($1, $2, 399101, 'group/alias-project', $3)`, workspaceID, connectionID, instanceURL+"/group/alias-project"); err != nil {
		t.Fatalf("insert unified GitLab project: %v", err)
	}
	if err := testPool.QueryRow(ctx, `
INSERT INTO vcs_pull_request (
	workspace_id, connection_id, provider, repo_owner, repo_name, pr_number,
	title, state, html_url, branch, head_sha, pr_created_at, pr_updated_at
) VALUES (
	$1, $2, 'gitlab', 'group', 'alias-project', 19,
	'Canonical aliased MR', 'open', $3, 'feature/alias', 'head-19', now(), now()
)
RETURNING id::text`, workspaceID, connectionID, instanceURL+"/group/alias-project/-/merge_requests/19").Scan(&prID); err != nil {
		t.Fatalf("insert canonical MR: %v", err)
	}
	if _, err := testPool.Exec(ctx, `INSERT INTO vcs_gitlab_mr_detail (vcs_pull_request_id, target_branch) VALUES ($1, 'main')`, prID); err != nil {
		t.Fatalf("insert GitLab MR details: %v", err)
	}
	if _, err := testPool.Exec(ctx, `INSERT INTO issue_vcs_pull_request (issue_id, pull_request_id) VALUES ($1, $2)`, issue.ID, prID); err != nil {
		t.Fatalf("link issue to canonical MR: %v", err)
	}
	if _, err := testPool.Exec(ctx, `
INSERT INTO vcs_gitlab_migration_id_map (workspace_id, entity_kind, legacy_id, unified_id)
VALUES ($1, 'pull_request', $2, $3)`, workspaceID, legacyID, prID); err != nil {
		t.Fatalf("insert legacy MR alias: %v", err)
	}
	t.Cleanup(func() {
		cleanupCtx := context.Background()
		_, _ = testPool.Exec(cleanupCtx, `DELETE FROM vcs_gitlab_migration_id_map WHERE workspace_id = $1 AND entity_kind = 'pull_request' AND legacy_id = $2`, workspaceID, legacyID)
		_, _ = testPool.Exec(cleanupCtx, `DELETE FROM issue_vcs_pull_request WHERE pull_request_id = $1`, prID)
		_, _ = testPool.Exec(cleanupCtx, `DELETE FROM gitlab_mr_approval_state WHERE vcs_pull_request_id = $1`, prID)
		_, _ = testPool.Exec(cleanupCtx, `DELETE FROM gitlab_mr_discussion WHERE vcs_pull_request_id = $1`, prID)
		_, _ = testPool.Exec(cleanupCtx, `DELETE FROM gitlab_mr_note WHERE vcs_pull_request_id = $1`, prID)
		_, _ = testPool.Exec(cleanupCtx, `DELETE FROM gitlab_pipeline_job WHERE vcs_pull_request_id = $1`, prID)
		_, _ = testPool.Exec(cleanupCtx, `DELETE FROM vcs_gitlab_pipeline WHERE pull_request_id = $1`, prID)
		_, _ = testPool.Exec(cleanupCtx, `DELETE FROM vcs_gitlab_mr_detail WHERE vcs_pull_request_id = $1`, prID)
		_, _ = testPool.Exec(cleanupCtx, `DELETE FROM vcs_pull_request WHERE id = $1`, prID)
		_, _ = testPool.Exec(cleanupCtx, `DELETE FROM vcs_gitlab_project WHERE workspace_id = $1 AND connection_id = $2 AND gitlab_project_id = 399101`, workspaceID, connectionID)
		_, _ = testPool.Exec(cleanupCtx, `DELETE FROM vcs_connection WHERE id = $1`, connectionID)
	})

	rec := httptest.NewRecorder()
	req := newRequest(http.MethodGet, "/api/issues/"+issue.ID+"/gitlab/merge-requests/"+legacyID+"/details", nil)
	req.Header.Set("X-Workspace-ID", issue.WorkspaceID)
	req = withURLParams(req, "id", issue.ID, "mrId", legacyID)
	testHandler.GetGitLabMergeRequestDetails(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected aliased MR to resolve in its workspace, got %d (%s)", rec.Code, rec.Body.String())
	}
	var out struct {
		MergeRequest struct {
			ID string `json:"id"`
		} `json:"merge_request"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode aliased response: %v", err)
	}
	if out.MergeRequest.ID != prID {
		t.Fatalf("response MR ID = %q, want canonical ID %q", out.MergeRequest.ID, prID)
	}

	other := httptest.NewRecorder()
	otherReq := newRequest(http.MethodGet, "/api/issues/"+otherIssue.ID+"/gitlab/merge-requests/"+legacyID+"/details", nil)
	otherReq.Header.Set("X-Workspace-ID", otherIssue.WorkspaceID)
	otherReq = withURLParams(otherReq, "id", otherIssue.ID, "mrId", legacyID)
	testHandler.GetGitLabMergeRequestDetails(other, otherReq)
	if other.Code != http.StatusNotFound {
		t.Fatalf("expected cross-workspace alias to be hidden, got %d (%s)", other.Code, other.Body.String())
	}
}

func seedGitLabVCSJobForIssue(t *testing.T, issue IssueResponse, projectID int64, iid int32, headSHA string, pipelineID, jobID int64, status, artifactName string) (string, string) {
	t.Helper()
	withVCSBox(t)
	path := fmt.Sprintf("group/project-%d", projectID)
	connectionID := seedGitLabVCSWebhookProject(t, projectID, path, true)
	connectionUUID := parseUUID(connectionID)
	var pullRequestID, jobRowID pgtype.UUID
	if err := testPool.QueryRow(context.Background(), `
INSERT INTO vcs_pull_request (
    workspace_id, connection_id, provider, repo_owner, repo_name, pr_number,
    title, state, html_url, branch, head_sha, pr_created_at, pr_updated_at,
    source_project_id, source_project_path
) VALUES (
    $1, $2, 'gitlab', 'group', $3, $4, 'canonical test MR', 'open',
    $5, 'feature/gitlab-test', $6, now(), now(), $7, $8
)
RETURNING id
`, parseUUID(issue.WorkspaceID), connectionUUID, fmt.Sprintf("project-%d", projectID), iid,
		fmt.Sprintf("https://code.mlamp.cn/%s/-/merge_requests/%d", path, iid), headSHA, projectID, path,
	).Scan(&pullRequestID); err != nil {
		t.Fatalf("seed canonical GitLab MR: %v", err)
	}
	if _, err := testPool.Exec(context.Background(), `
INSERT INTO issue_vcs_pull_request (issue_id, pull_request_id, linked_by_type, vcs_linked)
VALUES ($1, $2, 'system', TRUE)
`, parseUUID(issue.ID), pullRequestID); err != nil {
		t.Fatalf("link canonical GitLab MR to issue: %v", err)
	}
	if _, err := testPool.Exec(context.Background(), `
INSERT INTO vcs_gitlab_pipeline (
    workspace_id, connection_id, pull_request_id, pipeline_id,
    pipeline_project_id, pipeline_project_path, sha, ref, status, web_url, pipeline_updated_at
) VALUES ($1, $2, $3, $4, $5, $6, $7, 'feature/gitlab-test', $8, $9, now())
`, parseUUID(issue.WorkspaceID), connectionUUID, pullRequestID, pipelineID, projectID, path, headSHA, normalizeGitLabPipelineStatus(status),
		fmt.Sprintf("https://code.mlamp.cn/%s/-/pipelines/%d", path, pipelineID)); err != nil {
		t.Fatalf("seed canonical GitLab pipeline: %v", err)
	}
	var artifactFileName any
	var artifactFileSize any
	if artifactName != "" {
		artifactFileName = artifactName
		artifactFileSize = int64(len("artifact-bytes"))
	}
	if err := testPool.QueryRow(context.Background(), `
INSERT INTO gitlab_pipeline_job (
    workspace_id, merge_request_id, vcs_pull_request_id, pipeline_id, job_id,
    name, status, ref, sha, web_url, artifacts_file_name, artifacts_file_size
) VALUES ($1, NULL, $2, $3, $4, 'test', $5, 'feature/gitlab-test', $6, $7, $8, $9)
RETURNING id
`, parseUUID(issue.WorkspaceID), pullRequestID, pipelineID, jobID, status, headSHA,
		fmt.Sprintf("https://code.mlamp.cn/%s/-/jobs/%d", path, jobID), artifactFileName, artifactFileSize,
	).Scan(&jobRowID); err != nil {
		t.Fatalf("seed canonical GitLab pipeline job: %v", err)
	}
	t.Cleanup(func() {
		ctx := context.Background()
		_, _ = testPool.Exec(ctx, `DELETE FROM gitlab_pipeline_job WHERE vcs_pull_request_id = $1`, pullRequestID)
		_, _ = testPool.Exec(ctx, `DELETE FROM issue_vcs_pull_request WHERE pull_request_id = $1`, pullRequestID)
		_, _ = testPool.Exec(ctx, `DELETE FROM vcs_gitlab_pipeline WHERE pull_request_id = $1`, pullRequestID)
		_, _ = testPool.Exec(ctx, `DELETE FROM vcs_pull_request WHERE id = $1`, pullRequestID)
	})
	return uuidToString(jobRowID), uuidToString(pullRequestID)
}

func TestGitLabMRDetailsAndOperationsWorkWithoutProjectBinding(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("handler test fixture not initialized (no DB?)")
	}
	for _, tc := range []struct {
		name          string
		deleteBinding bool
		projectID     int64
		iid           int32
	}{
		{name: "official MR after project deletion", deleteBinding: true, projectID: 99171, iid: 31},
		{name: "official MR without initial binding", projectID: 99172, iid: 32},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			issue := createGitLabTestIssue(t, tc.name, "in_progress")
			path := fmt.Sprintf("group/project-%d", tc.projectID)
			headSHA := fmt.Sprintf("head-%d", tc.projectID)
			var jobID, prID string
			if tc.deleteBinding {
				jobID, prID = seedGitLabVCSJobForIssue(t, issue, tc.projectID, tc.iid, headSHA, tc.projectID+1000, tc.projectID+2000, "failed", "artifacts.zip")
				if _, err := testHandler.Queries.EnsureVCSGitLabMRDetail(ctx, db.EnsureVCSGitLabMRDetailParams{
					WorkspaceID: parseUUID(issue.WorkspaceID), PullRequestID: parseUUID(prID),
				}); err != nil {
					t.Fatalf("seed official MR details: %v", err)
				}
				var projectUUID string
				if err := testPool.QueryRow(ctx, `
SELECT project.id::text
FROM vcs_gitlab_project project
JOIN vcs_pull_request pr ON pr.workspace_id = project.workspace_id AND pr.connection_id = project.connection_id
WHERE pr.id = $1 AND project.gitlab_project_id = $2
`, parseUUID(prID), tc.projectID).Scan(&projectUUID); err != nil {
					t.Fatalf("load project binding before deletion: %v", err)
				}
				deleteRec := httptest.NewRecorder()
				deleteReq := newRequest(http.MethodDelete, "/api/workspaces/"+issue.WorkspaceID+"/vcs/gitlab/projects/"+projectUUID, nil)
				deleteReq = withURLParams(deleteReq, "id", issue.WorkspaceID, "projectId", projectUUID)
				deleteReq.Header.Set("X-Workspace-ID", issue.WorkspaceID)
				testHandler.DeleteVCSGitLabProject(deleteRec, deleteReq)
				if deleteRec.Code != http.StatusNoContent {
					t.Fatalf("delete GitLab project binding: %d %s", deleteRec.Code, deleteRec.Body.String())
				}
				var retainedLinks, retainedDetails int
				if err := testPool.QueryRow(ctx, `
SELECT count(*) FROM issue_vcs_pull_request
WHERE issue_id = $1 AND pull_request_id = $2 AND vcs_linked
`, parseUUID(issue.ID), parseUUID(prID)).Scan(&retainedLinks); err != nil {
					t.Fatalf("read official issue link after project deletion: %v", err)
				}
				if err := testPool.QueryRow(ctx, `
SELECT count(*) FROM vcs_gitlab_mr_detail WHERE vcs_pull_request_id = $1
`, parseUUID(prID)).Scan(&retainedDetails); err != nil {
					t.Fatalf("read official MR details after project deletion: %v", err)
				}
				if retainedLinks != 1 || retainedDetails != 1 {
					t.Fatalf("project deletion retained %d official links and %d MR details, want one each", retainedLinks, retainedDetails)
				}
			} else {
				jobID, prID, path = seedGitLabVCSJobForIssueWithoutProjectBinding(t, issue, tc.projectID, tc.iid, headSHA, tc.projectID+1000, tc.projectID+2000, "failed", "artifacts.zip")
				var projects int
				if err := testPool.QueryRow(ctx, `
SELECT count(*) FROM vcs_gitlab_project
WHERE workspace_id = $1 AND connection_id = (SELECT connection_id FROM vcs_pull_request WHERE id = $2)
`, parseUUID(issue.WorkspaceID), parseUUID(prID)).Scan(&projects); err != nil {
					t.Fatalf("check initially unbound GitLab MR: %v", err)
				}
				if projects != 0 {
					t.Fatalf("initially unbound MR has %d project rows", projects)
				}
			}

			updatedAt := time.Now().UTC().Add(time.Minute).Format(time.RFC3339)
			fake := &fakeGitLabDetailAPI{
				fakeGitLabAPI: fakeGitLabAPI{project: gitlab.Project{
					ID: tc.projectID, PathWithNamespace: path, WebURL: "https://gitlab.example/" + path,
				}},
				mr: gitlab.MergeRequest{
					IID: tc.iid, Title: "official canonical MR", State: "merged",
					WebURL:       "https://gitlab.example/" + path + fmt.Sprintf("/-/merge_requests/%d", tc.iid),
					SourceBranch: "feature/gitlab-test", TargetBranch: "main", SHA: headSHA,
					CreatedAt: time.Now().UTC().Add(-time.Hour).Format(time.RFC3339), UpdatedAt: updatedAt,
					SourceProjectID: tc.projectID,
					SourceProject:   gitlab.Project{ID: tc.projectID, PathWithNamespace: path},
				},
				trace: gitlab.JobTrace{Text: "failed test summary"},
				artifactsResp: &http.Response{
					StatusCode: http.StatusOK,
					Header:     http.Header{"Content-Type": []string{"application/zip"}},
					Body:       io.NopCloser(strings.NewReader("artifact bytes")),
				},
			}
			installFakeGitLabDetailClient(t, fake)

			details := httptest.NewRecorder()
			detailsReq := newRequest(http.MethodGet, "/api/issues/"+issue.ID+"/vcs/pull-requests/"+prID+"/gitlab", nil)
			detailsReq = withURLParams(detailsReq, "id", issue.ID, "prId", prID)
			testHandler.GetVCSGitLabMergeRequestDetails(details, detailsReq)
			if details.Code != http.StatusOK {
				t.Fatalf("get details without project binding: %d %s", details.Code, details.Body.String())
			}
			var out struct {
				MergeRequest struct {
					ID              string `json:"id"`
					ProjectPath     string `json:"project_path"`
					GitLabProjectID int64  `json:"gitlab_project_id"`
				} `json:"merge_request"`
			}
			if err := json.Unmarshal(details.Body.Bytes(), &out); err != nil {
				t.Fatalf("decode project-unbound MR details: %v", err)
			}
			if out.MergeRequest.ID != prID || out.MergeRequest.ProjectPath != path || out.MergeRequest.GitLabProjectID != tc.projectID {
				t.Fatalf("project-unbound MR identity = %+v", out.MergeRequest)
			}

			traceRec := httptest.NewRecorder()
			traceReq := newRequest(http.MethodGet, "/api/issues/"+issue.ID+"/vcs/pull-requests/"+prID+"/gitlab/jobs/"+jobID+"/trace", nil)
			traceReq = withURLParams(traceReq, "id", issue.ID, "prId", prID, "jobId", jobID)
			testHandler.GetVCSGitLabJobTrace(traceRec, traceReq)
			if traceRec.Code != http.StatusOK || !strings.Contains(traceRec.Body.String(), "failed test summary") {
				t.Fatalf("get project-unbound job trace: %d %s", traceRec.Code, traceRec.Body.String())
			}

			artifactRec := httptest.NewRecorder()
			artifactReq := newRequest(http.MethodGet, "/api/issues/"+issue.ID+"/vcs/pull-requests/"+prID+"/gitlab/jobs/"+jobID+"/artifacts", nil)
			artifactReq = withURLParams(artifactReq, "id", issue.ID, "prId", prID, "jobId", jobID)
			testHandler.OpenVCSGitLabJobArtifacts(artifactRec, artifactReq)
			if artifactRec.Code != http.StatusOK || artifactRec.Body.String() != "artifact bytes" {
				t.Fatalf("get project-unbound job artifacts: %d %q", artifactRec.Code, artifactRec.Body.String())
			}

			refreshRec := httptest.NewRecorder()
			refreshReq := newRequest(http.MethodPost, "/api/issues/"+issue.ID+"/vcs/pull-requests/"+prID+"/gitlab/refresh", nil)
			refreshReq = withURLParams(refreshReq, "id", issue.ID, "prId", prID)
			testHandler.RefreshVCSGitLabMergeRequestForIssue(refreshRec, refreshReq)
			if refreshRec.Code != http.StatusOK {
				t.Fatalf("refresh project-unbound MR: %d %s", refreshRec.Code, refreshRec.Body.String())
			}

			mergeRec := httptest.NewRecorder()
			mergeReq := newRequest(http.MethodPost, "/api/issues/"+issue.ID+"/vcs/pull-requests/"+prID+"/gitlab/merge", nil)
			mergeReq = withURLParams(mergeReq, "id", issue.ID, "prId", prID)
			testHandler.MergeVCSGitLabMergeRequestForIssue(mergeRec, mergeReq)
			if mergeRec.Code != http.StatusOK {
				t.Fatalf("merge project-unbound MR: %d %s", mergeRec.Code, mergeRec.Body.String())
			}
			if !fake.mergeCalled || fake.mergeProjectID != tc.projectID || fake.mergeIID != tc.iid {
				t.Fatalf("merge used wrong project/MR identity: called=%v project=%d iid=%d", fake.mergeCalled, fake.mergeProjectID, fake.mergeIID)
			}
			if len(fake.projectLookupPaths) == 0 {
				t.Fatal("GitLab project identity was not resolved through the canonical connection")
			}
			for _, lookedUpPath := range fake.projectLookupPaths {
				if lookedUpPath != path {
					t.Fatalf("GitLab project lookup path = %q, want full canonical path %q", lookedUpPath, path)
				}
			}
			if fake.clientConfig.Token != "tok" {
				t.Fatal("project-unbound operation did not use the canonical PR connection credentials")
			}
		})
	}
}

func seedGitLabVCSJobForIssueWithoutProjectBinding(t *testing.T, issue IssueResponse, projectID int64, iid int32, headSHA string, pipelineID, jobID int64, status, artifactName string) (string, string, string) {
	t.Helper()
	ctx := context.Background()
	box := withVCSBox(t)
	path := fmt.Sprintf("group/project-%d", projectID)
	connectionID := seedVCSConnection(t, ctx, box, "gitlab", fmt.Sprintf("https://gitlab-unbound-%d.example", projectID))
	connectionUUID := parseUUID(connectionID)
	var pullRequestID, jobRowID pgtype.UUID
	if err := testPool.QueryRow(ctx, `
INSERT INTO vcs_pull_request (
    workspace_id, connection_id, provider, repo_owner, repo_name, pr_number,
    title, state, html_url, branch, head_sha, pr_created_at, pr_updated_at,
    source_project_id, source_project_path
) VALUES (
    $1, $2, 'gitlab', 'group', $3, $4, 'canonical test MR', 'open',
    $5, 'feature/gitlab-test', $6, now(), now(), $7, $8
)
RETURNING id
`, parseUUID(issue.WorkspaceID), connectionUUID, fmt.Sprintf("project-%d", projectID), iid,
		fmt.Sprintf("https://gitlab-unbound-%d.example/%s/-/merge_requests/%d", projectID, path, iid),
		headSHA, projectID, path,
	).Scan(&pullRequestID); err != nil {
		t.Fatalf("seed project-unbound canonical GitLab MR: %v", err)
	}
	if _, err := testPool.Exec(ctx, `
INSERT INTO issue_vcs_pull_request (issue_id, pull_request_id, linked_by_type, vcs_linked)
VALUES ($1, $2, 'system', TRUE)
`, parseUUID(issue.ID), pullRequestID); err != nil {
		t.Fatalf("link project-unbound canonical GitLab MR to issue: %v", err)
	}
	if _, err := testPool.Exec(ctx, `
INSERT INTO vcs_gitlab_mr_detail (vcs_pull_request_id, description, target_branch)
VALUES ($1, 'project-unbound detail', 'main')
`, pullRequestID); err != nil {
		t.Fatalf("seed project-unbound GitLab MR details: %v", err)
	}
	if _, err := testPool.Exec(ctx, `
INSERT INTO vcs_gitlab_pipeline (
    workspace_id, connection_id, pull_request_id, pipeline_id,
    pipeline_project_id, pipeline_project_path, sha, ref, status, web_url, pipeline_updated_at
) VALUES ($1, $2, $3, $4, $5, $6, $7, 'feature/gitlab-test', $8, $9, now())
`, parseUUID(issue.WorkspaceID), connectionUUID, pullRequestID, pipelineID, projectID, path, headSHA,
		normalizeGitLabPipelineStatus(status), fmt.Sprintf("https://gitlab-unbound-%d.example/%s/-/pipelines/%d", projectID, path, pipelineID)); err != nil {
		t.Fatalf("seed project-unbound GitLab pipeline: %v", err)
	}
	var artifactFileName any
	var artifactFileSize any
	if artifactName != "" {
		artifactFileName = artifactName
		artifactFileSize = int64(len("artifact-bytes"))
	}
	if err := testPool.QueryRow(ctx, `
INSERT INTO gitlab_pipeline_job (
    workspace_id, merge_request_id, vcs_pull_request_id, pipeline_id, job_id,
    name, status, ref, sha, web_url, artifacts_file_name, artifacts_file_size
) VALUES ($1, NULL, $2, $3, $4, 'test', $5, 'feature/gitlab-test', $6, $7, $8, $9)
RETURNING id
`, parseUUID(issue.WorkspaceID), pullRequestID, pipelineID, jobID, status, headSHA,
		fmt.Sprintf("https://gitlab-unbound-%d.example/%s/-/jobs/%d", projectID, path, jobID), artifactFileName, artifactFileSize,
	).Scan(&jobRowID); err != nil {
		t.Fatalf("seed project-unbound GitLab job: %v", err)
	}
	t.Cleanup(func() {
		_, _ = testPool.Exec(context.Background(), `DELETE FROM gitlab_pipeline_job WHERE vcs_pull_request_id = $1`, pullRequestID)
		_, _ = testPool.Exec(context.Background(), `DELETE FROM issue_vcs_pull_request WHERE pull_request_id = $1`, pullRequestID)
		_, _ = testPool.Exec(context.Background(), `DELETE FROM vcs_gitlab_pipeline WHERE pull_request_id = $1`, pullRequestID)
		_, _ = testPool.Exec(context.Background(), `DELETE FROM vcs_gitlab_mr_detail WHERE vcs_pull_request_id = $1`, pullRequestID)
		_, _ = testPool.Exec(context.Background(), `DELETE FROM vcs_pull_request WHERE id = $1`, pullRequestID)
		_, _ = testPool.Exec(context.Background(), `DELETE FROM vcs_connection WHERE workspace_id = $1 AND id = $2`, parseUUID(issue.WorkspaceID), connectionUUID)
	})
	return uuidToString(jobRowID), uuidToString(pullRequestID), path
}

func TestGetGitLabJobTraceRequiresIssueLinkAndRedactsTrace(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("handler test fixture not initialized (no DB?)")
	}
	t.Setenv("GITLAB_BASE_URL", "https://code.mlamp.cn")
	t.Setenv("GITLAB_TOKEN", "token")
	t.Setenv("GITLAB_WEBHOOK_SECRET", "secret")
	const projectID int64 = 99116
	issue := createGitLabTestIssue(t, "gitlab trace allowed", "in_progress")
	other := createGitLabTestIssue(t, "gitlab trace denied", "in_progress")
	jobID, prID := seedGitLabVCSJobForIssue(t, issue, projectID, 22, "tracesha", 221, 2201, "failed", "")
	installFakeGitLabDetailClient(t, &fakeGitLabDetailAPI{
		trace: gitlab.JobTrace{Text: "assert failed\ntoken: secret-token", Truncated: false},
	})

	denied := httptest.NewRecorder()
	deniedReq := newRequest(http.MethodGet, "/api/issues/"+other.ID+"/gitlab/jobs/"+jobID+"/trace", nil)
	deniedReq = withURLParams(deniedReq, "id", other.ID, "jobId", jobID)
	testHandler.GetGitLabJobTrace(denied, deniedReq)
	if denied.Code != http.StatusNotFound {
		t.Fatalf("expected 404 for unlinked issue, got %d (%s)", denied.Code, denied.Body.String())
	}

	rec := httptest.NewRecorder()
	req := newRequest(http.MethodGet, "/api/issues/"+issue.ID+"/gitlab/jobs/"+jobID+"/trace", nil)
	req = withURLParams(req, "id", issue.ID, "jobId", jobID)
	testHandler.GetGitLabJobTrace(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d (%s)", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "secret-token") || !strings.Contains(rec.Body.String(), "assert failed") {
		t.Fatalf("trace response was not redacted as expected: %s", rec.Body.String())
	}
	var storedTrace string
	if err := testPool.QueryRow(context.Background(), `SELECT trace_summary FROM gitlab_pipeline_job WHERE id = $1`, parseUUID(jobID)).Scan(&storedTrace); err != nil {
		t.Fatalf("load stored trace summary: %v", err)
	}
	if strings.Contains(storedTrace, "secret-token") || !strings.Contains(storedTrace, "assert failed") {
		t.Fatalf("stored trace summary was not redacted as expected: %q", storedTrace)
	}
	canonicalRec := httptest.NewRecorder()
	canonicalReq := newRequest(http.MethodGet, "/api/issues/"+issue.ID+"/vcs/pull-requests/"+prID+"/gitlab/jobs/"+jobID+"/trace", nil)
	canonicalReq = withURLParams(canonicalReq, "id", issue.ID, "prId", prID, "jobId", jobID)
	testHandler.GetVCSGitLabJobTrace(canonicalRec, canonicalReq)
	if canonicalRec.Code != http.StatusOK || strings.Contains(canonicalRec.Body.String(), "secret-token") {
		t.Fatalf("canonical trace route returned %d (%s)", canonicalRec.Code, canonicalRec.Body.String())
	}
}

func TestOpenGitLabJobArtifactsStreamsArtifactWithoutToken(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("handler test fixture not initialized (no DB?)")
	}
	t.Setenv("GITLAB_BASE_URL", "https://code.mlamp.cn")
	t.Setenv("GITLAB_TOKEN", "token")
	t.Setenv("GITLAB_WEBHOOK_SECRET", "secret")
	const projectID int64 = 99117
	issue := createGitLabTestIssue(t, "gitlab artifact", "in_progress")
	jobID, prID := seedGitLabVCSJobForIssue(t, issue, projectID, 23, "artifactsha", 231, 2301, "success", "artifacts.zip")
	installFakeGitLabDetailClient(t, &fakeGitLabDetailAPI{
		artifactsResp: &http.Response{
			StatusCode: http.StatusOK,
			Header: http.Header{
				"Content-Type": []string{"application/zip"},
			},
			Body: io.NopCloser(strings.NewReader("artifact-bytes")),
		},
	})

	rec := httptest.NewRecorder()
	req := newRequest(http.MethodGet, "/api/issues/"+issue.ID+"/gitlab/jobs/"+jobID+"/artifacts", nil)
	req = withURLParams(req, "id", issue.ID, "jobId", jobID)
	testHandler.OpenGitLabJobArtifacts(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d (%s)", rec.Code, rec.Body.String())
	}
	if rec.Body.String() != "artifact-bytes" {
		t.Fatalf("artifact body = %q", rec.Body.String())
	}
	if got := rec.Header().Get("Content-Disposition"); !strings.Contains(got, "artifacts.zip") {
		t.Fatalf("Content-Disposition = %q", got)
	}
	for key, values := range rec.Header() {
		if strings.Contains(strings.ToLower(key), "token") || strings.Contains(strings.Join(values, ","), "PRIVATE-TOKEN") {
			t.Fatalf("artifact response leaked token header %s=%v", key, values)
		}
	}
	installFakeGitLabDetailClient(t, &fakeGitLabDetailAPI{
		artifactsResp: &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"application/zip"}},
			Body:       io.NopCloser(strings.NewReader("artifact-bytes")),
		},
	})
	canonicalRec := httptest.NewRecorder()
	canonicalReq := newRequest(http.MethodGet, "/api/issues/"+issue.ID+"/vcs/pull-requests/"+prID+"/gitlab/jobs/"+jobID+"/artifacts", nil)
	canonicalReq = withURLParams(canonicalReq, "id", issue.ID, "prId", prID, "jobId", jobID)
	testHandler.OpenVCSGitLabJobArtifacts(canonicalRec, canonicalReq)
	if canonicalRec.Code != http.StatusOK || canonicalRec.Body.String() != "artifact-bytes" {
		t.Fatalf("canonical artifact route returned %d (%q)", canonicalRec.Code, canonicalRec.Body.String())
	}
}

func int32Ptr(v int32) *int32 {
	return &v
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
	_ = withVCSBox(t)
	tokenEncrypted, err := testHandler.sealVCSSecret("role-gating-token")
	if err != nil {
		t.Fatalf("encrypt role-gating token: %v", err)
	}
	webhookSecretEncrypted, err := testHandler.sealVCSSecret("role-gating-secret")
	if err != nil {
		t.Fatalf("encrypt role-gating webhook secret: %v", err)
	}
	connection, err := testHandler.Queries.UpsertVCSConnection(ctx, db.UpsertVCSConnectionParams{
		WorkspaceID:            parseUUID(wsID),
		Provider:               "gitlab",
		InstanceUrl:            "https://gitlab-role-gating.invalid",
		AccountLogin:           "test-user",
		AccessTokenEncrypted:   tokenEncrypted,
		WebhookSecretEncrypted: webhookSecretEncrypted,
		ConnectedByID:          pgtype.UUID{},
	})
	if err != nil {
		t.Fatalf("seed canonical GitLab connection: %v", err)
	}
	var projectID string
	if err := testPool.QueryRow(ctx, `
INSERT INTO vcs_gitlab_project (workspace_id, connection_id, gitlab_project_id, path_with_namespace, web_url)
VALUES ($1, $2, 96001, 'group/repo', 'https://gitlab-role-gating.invalid/group/repo')
RETURNING id::text
`, wsID, connection.ID).Scan(&projectID); err != nil {
		t.Fatalf("seed canonical GitLab project: %v", err)
	}
	canonicalProjectID := projectID
	t.Cleanup(func() {
		_, _ = testPool.Exec(context.Background(), `DELETE FROM vcs_gitlab_project WHERE id = $1`, projectID)
		_, _ = testPool.Exec(context.Background(), `DELETE FROM vcs_connection WHERE id = $1`, connection.ID)
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
	if got := exercise(http.MethodDelete, "/api/workspaces/"+wsID+"/gitlab/projects/"+canonicalProjectID, memberUserID, nil); got != http.StatusForbidden {
		t.Fatalf("member DELETE project: want 403, got %d", got)
	}
	if got := exercise(http.MethodDelete, "/api/workspaces/"+wsID+"/gitlab/projects/"+canonicalProjectID, adminUserID, nil); got != http.StatusNoContent {
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
	if testHandler == nil || testPool == nil {
		t.Skip("handler test fixture not initialized (no DB?)")
	}
	ctx := context.Background()
	box := withVCSBox(t)
	connectionID := seedVCSConnection(t, ctx, box, "gitlab", "https://gitlab-redaction.invalid/root")
	connectionUUID := parseUUID(connectionID)
	if _, err := testPool.Exec(ctx, `
INSERT INTO vcs_gitlab_migration_id_map (workspace_id, entity_kind, legacy_id, unified_id)
VALUES ($1, 'connection', gen_random_uuid(), $2)
`, parseUUID(testWorkspaceID), connectionUUID); err != nil {
		t.Fatalf("seed legacy connection alias: %v", err)
	}
	t.Cleanup(func() {
		_, _ = testPool.Exec(ctx, `DELETE FROM vcs_gitlab_migration_id_map WHERE workspace_id = $1 AND entity_kind = 'connection' AND unified_id = $2`, parseUUID(testWorkspaceID), connectionUUID)
		_, _ = testPool.Exec(ctx, `DELETE FROM vcs_connection WHERE id = $1`, connectionUUID)
	})
	const gitlabProjectID int64 = 96002
	projectID := seedCanonicalGitLabProject(t, testWorkspaceID, connectionID, gitlabProjectID, "group/redaction", nil, false)
	rawErr := `hook failed: GITLAB_WEBHOOK_SECRET=super-secret {"token":"json-secret"} secret-token: raw-secret glpat-AbCdEfGhIjKlMnOpQrStUvWx`
	if _, err := testPool.Exec(
		ctx,
		`UPDATE vcs_gitlab_project SET last_sync_error = $1 WHERE id = $2`,
		rawErr,
		projectID,
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
	if testHandler == nil || testPool == nil {
		t.Skip("handler test fixture not initialized (no DB?)")
	}
	withVCSBox(t)
	const projectID int64 = 97001
	path := fmt.Sprintf("group/project-%d", projectID)
	connectionID := seedGitLabVCSWebhookProject(t, projectID, path, true)
	issue := createGitLabTestIssue(t, "gitlab link", "in_progress")

	fireGitLabMRWebhookWithFakeAPI(t, connectionID, projectID, path, 1, "Refs "+issue.Identifier, "See "+issue.Identifier, strings.ToLower(issue.Identifier)+"/branch", "opened", "open", "abc123", "2026-04-29T00:00:00Z")

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
		t.Fatalf("unexpected canonical list response: %+v", body.MergeRequests)
	}
}

func TestGitLabClosingIntentTitleBodyOnly(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("handler test fixture not initialized (no DB?)")
	}
	ctx := context.Background()
	withVCSBox(t)
	const projectID int64 = 97002
	path := fmt.Sprintf("group/project-%d", projectID)
	connectionID := seedGitLabVCSWebhookProject(t, projectID, path, true)
	branchOnly := createGitLabTestIssue(t, "gitlab branch only", "in_progress")
	titleBody := createGitLabTestIssue(t, "gitlab title body", "in_progress")

	fireGitLabMRWebhookWithFakeAPI(t, connectionID, projectID, path, 1, "Implement branch only", "", strings.ToLower(branchOnly.Identifier)+"/fix", "merged", "merge", "sha-branch", "2026-04-29T00:00:00Z")
	got, err := testHandler.Queries.GetIssue(ctx, parseUUID(branchOnly.ID))
	if err != nil {
		t.Fatalf("GetIssue branchOnly: %v", err)
	}
	if got.Status != "in_progress" {
		t.Fatalf("branch-only identifier should not auto-complete, got %q", got.Status)
	}
	counts, err := testHandler.Queries.GetGitLabVCSIssueCloseAggregate(ctx, db.GetGitLabVCSIssueCloseAggregateParams{
		WorkspaceID: parseUUID(testWorkspaceID),
		IssueID:     parseUUID(branchOnly.ID),
	})
	if err != nil {
		t.Fatalf("GetGitLabVCSIssueCloseAggregate branchOnly: %v", err)
	}
	if counts.MergedWithCloseIntentCount != 0 {
		t.Fatalf("branch-only close intent count = %d, want 0", counts.MergedWithCloseIntentCount)
	}

	fireGitLabMRWebhookWithFakeAPI(t, connectionID, projectID, path, 2, "Fixes "+titleBody.Identifier, "", "feature/title-body", "merged", "merge", "sha-title", "2026-04-30T00:00:00Z")
	got, err = testHandler.Queries.GetIssue(ctx, parseUUID(titleBody.ID))
	if err != nil {
		t.Fatalf("GetIssue titleBody: %v", err)
	}
	if got.Status != "done" {
		t.Fatalf("title/body closing keyword should auto-complete, got %q", got.Status)
	}
}

func TestGitLabMergedCloseIntentAdvancesIssueToDone(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("handler test fixture not initialized (no DB?)")
	}
	withVCSBox(t)
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
	path := fmt.Sprintf("group/project-%d", projectID)
	connectionID := seedGitLabVCSWebhookProject(t, projectID, path, true)
	issue := createGitLabTestIssue(t, "gitlab merged close intent", "in_progress")

	fireGitLabMRWebhookWithFakeAPI(t, connectionID, projectID, path, 1, "Implement", "Fixes "+issue.Identifier, "feature/close", "merged", "merge", "sha-close", "2026-04-29T00:00:00Z")
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
	if testHandler == nil || testPool == nil {
		t.Skip("handler test fixture not initialized (no DB?)")
	}
	ctx := context.Background()
	withVCSBox(t)
	const projectID int64 = 97004
	path := fmt.Sprintf("group/project-%d", projectID)
	connectionID := seedGitLabVCSWebhookProject(t, projectID, path, true)
	issue := createGitLabTestIssue(t, "gitlab multiple mrs", "in_progress")

	fireGitLabMRWebhookWithFakeAPI(t, connectionID, projectID, path, 1, issue.Identifier+": follow-up", "", "feature/open", "opened", "open", "sha-open", "2026-04-29T00:00:00Z")
	fireGitLabMRWebhookWithFakeAPI(t, connectionID, projectID, path, 2, "Implement primary", "Closes "+issue.Identifier, "feature/primary", "merged", "merge", "sha-primary", "2026-04-30T00:00:00Z")
	got, err := testHandler.Queries.GetIssue(ctx, parseUUID(issue.ID))
	if err != nil {
		t.Fatalf("GetIssue after primary merge: %v", err)
	}
	if got.Status != "in_progress" {
		t.Fatalf("issue should stay in_progress while linked MR is open, got %q", got.Status)
	}

	fireGitLabMRWebhookWithFakeAPI(t, connectionID, projectID, path, 1, issue.Identifier+": follow-up", "", "feature/open", "merged", "merge", "sha-open", "2026-05-01T00:00:00Z")
	got, err = testHandler.Queries.GetIssue(ctx, parseUUID(issue.ID))
	if err != nil {
		t.Fatalf("GetIssue after follow-up merge: %v", err)
	}
	if got.Status != "done" {
		t.Fatalf("issue should advance after all MRs resolved, got %q", got.Status)
	}
}

func TestNormalizeGitLabPipelineStatus(t *testing.T) {
	for _, tc := range []struct {
		status string
		want   string
	}{
		{"success", "passed"},
		{"skipped", "passed"},
		{"failed", "failed"},
		{"canceled", "failed"},
		{"manual", "pending"},
		{"running", "pending"},
	} {
		if got := normalizeGitLabPipelineStatus(tc.status); got != tc.want {
			t.Errorf("normalizeGitLabPipelineStatus(%q) = %q, want %q", tc.status, got, tc.want)
		}
	}
}

func withGitLabSecretBox(t *testing.T) {
	t.Helper()
	box, err := secretbox.New(bytes.Repeat([]byte{9}, secretbox.KeySize))
	if err != nil {
		t.Fatalf("secretbox.New: %v", err)
	}
	previous := testHandler.VCSSecretBox
	testHandler.VCSSecretBox = box
	t.Cleanup(func() { testHandler.VCSSecretBox = previous })
}
