package gitlab

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestLoadConfigFromEnv(t *testing.T) {
	t.Setenv("GITLAB_BASE_URL", "https://code.mlamp.cn/")
	t.Setenv("GITLAB_TOKEN", "secret-token")
	t.Setenv("GITLAB_WEBHOOK_SECRET", "hook-secret")
	t.Setenv("GITLAB_PROXY_URL", "socks5h://127.0.0.1:18080")

	cfg := LoadConfigFromEnv()
	if cfg.BaseURL != "https://code.mlamp.cn" {
		t.Fatalf("BaseURL = %q", cfg.BaseURL)
	}
	if !cfg.Configured() {
		t.Fatal("expected configured")
	}
	if cfg.ProxyURL != "socks5h://127.0.0.1:18080" {
		t.Fatalf("ProxyURL = %q", cfg.ProxyURL)
	}
}

func TestNewClientRejectsUnsupportedProxyScheme(t *testing.T) {
	_, err := NewClient(Config{
		BaseURL:  "https://code.mlamp.cn",
		Token:    "token",
		ProxyURL: "ftp://127.0.0.1:18080",
	})
	if err == nil || !strings.Contains(err.Error(), "unsupported gitlab proxy scheme") {
		t.Fatalf("error = %v", err)
	}
}

func TestValidateCredentialsUsesCurrentUserEndpoint(t *testing.T) {
	var gotPath, gotToken string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotToken = r.Header.Get("PRIVATE-TOKEN")
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	client, err := NewClient(Config{BaseURL: srv.URL, Token: "secret-token"})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	if err := client.ValidateCredentials(context.Background()); err != nil {
		t.Fatalf("ValidateCredentials: %v", err)
	}
	if gotPath != "/api/v4/user" || gotToken != "secret-token" {
		t.Fatalf("validation request path=%q token=%q", gotPath, gotToken)
	}
}

func TestValidateCredentialsReturnsUnauthorizedForRejectedToken(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()

	client, err := NewClient(Config{BaseURL: srv.URL, Token: "bad-token"})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	if err := client.ValidateCredentials(context.Background()); err != ErrUnauthorized {
		t.Fatalf("ValidateCredentials error = %v, want ErrUnauthorized", err)
	}
}

func TestNewClientRejectsInvalidBaseURL(t *testing.T) {
	tests := []struct {
		name    string
		baseURL string
		wantErr string
	}{
		{
			name:    "missing scheme",
			baseURL: "code.mlamp.cn",
			wantErr: "invalid gitlab base url",
		},
		{
			name:    "parse error",
			baseURL: "://bad",
			wantErr: "invalid gitlab base url",
		},
		{
			name:    "unsupported scheme",
			baseURL: "ftp://code.mlamp.cn",
			wantErr: "unsupported gitlab base url scheme",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := NewClient(Config{BaseURL: tt.baseURL, Token: "token"})
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("error = %v, want substring %q", err, tt.wantErr)
			}
		})
	}
}

func TestGetProjectEncodesPathWithNamespace(t *testing.T) {
	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.EscapedPath()
		if got := r.Header.Get("PRIVATE-TOKEN"); got != "token" {
			t.Fatalf("PRIVATE-TOKEN = %q", got)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"id":42,"path_with_namespace":"group/repo","web_url":"https://code.mlamp.cn/group/repo","http_url_to_repo":"https://code.mlamp.cn/group/repo.git","ssh_url_to_repo":"git@code.mlamp.cn:group/repo.git"}`))
	}))
	defer srv.Close()

	c, err := NewClient(Config{BaseURL: srv.URL, Token: "token"})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	project, err := c.GetProject(context.Background(), "group/repo")
	if err != nil {
		t.Fatalf("GetProject: %v", err)
	}
	if gotPath != "/api/v4/projects/group%2Frepo" {
		t.Fatalf("path = %q", gotPath)
	}
	if project.ID != 42 || project.PathWithNamespace != "group/repo" {
		t.Fatalf("project = %+v", project)
	}
	if project.HTTPURLToRepo != "https://code.mlamp.cn/group/repo.git" || project.SSHURLToRepo != "git@code.mlamp.cn:group/repo.git" {
		t.Fatalf("project clone URLs = http %q ssh %q", project.HTTPURLToRepo, project.SSHURLToRepo)
	}
}

func TestCreateProjectHookIdentifiesMultica(t *testing.T) {
	var body map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && r.URL.Path == "/api/v4/user" {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"name":"董振兴","username":"dongzhenxing"}`))
			return
		}
		if r.Method != http.MethodPost || r.URL.Path != "/api/v4/projects/42/hooks" {
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decode request: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":123}`))
	}))
	defer srv.Close()

	client, err := NewClient(Config{BaseURL: srv.URL, Token: "token"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.CreateProjectHook(context.Background(), 42, "https://multica.example/hook", "secret"); err != nil {
		t.Fatalf("create project hook: %v", err)
	}
	if body["name"] != "董振兴 Multica Hook" {
		t.Fatalf("webhook name = %v, want authenticated user's name", body["name"])
	}
}

func TestGetProjectHook(t *testing.T) {
	const webhookURL = "https://multica.example/hook?tenant=acme&mode=full"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/api/v4/projects/42/hooks/123" {
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
		if got := r.Header.Get("PRIVATE-TOKEN"); got != "token" {
			t.Errorf("PRIVATE-TOKEN = %q", got)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":123,"url":"https://multica.example/hook?tenant=acme&mode=full"}`))
	}))
	defer srv.Close()

	client, err := NewClient(Config{BaseURL: srv.URL, Token: "token"})
	if err != nil {
		t.Fatal(err)
	}
	hook, err := client.GetProjectHook(context.Background(), 42, 123)
	if err != nil {
		t.Fatalf("get project hook: %v", err)
	}
	if hook.ID != 123 || hook.URL != webhookURL {
		t.Fatalf("project hook = %+v", hook)
	}
}

func TestUpdateProjectHookSecret(t *testing.T) {
	const webhookURL = "https://multica.example/hook?tenant=acme&mode=full"
	var gotMethod, gotPath string
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("PRIVATE-TOKEN"); got != "token" {
			t.Errorf("PRIVATE-TOKEN = %q", got)
		}
		if r.Method == http.MethodGet {
			if r.URL.Path != "/api/v4/projects/42/hooks/123" {
				t.Errorf("GET path = %q", r.URL.Path)
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"id":123,"url":"https://multica.example/hook?tenant=acme&mode=full"}`))
			return
		}
		gotMethod = r.Method
		gotPath = r.URL.Path
		if gotMethod != http.MethodPut || gotPath != "/api/v4/projects/42/hooks/123" {
			t.Errorf("unexpected request: %s %s", gotMethod, gotPath)
		}
		if err := json.NewDecoder(r.Body).Decode(&gotBody); err != nil {
			t.Errorf("decode request body: %v", err)
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	client, err := NewClient(Config{BaseURL: srv.URL, Token: "token"})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	hook, err := client.GetProjectHook(ctx, 42, 123)
	if err != nil {
		t.Fatalf("get project hook: %v", err)
	}
	if hook.URL != webhookURL {
		t.Fatalf("hook URL = %q, want %q", hook.URL, webhookURL)
	}
	if err := client.UpdateProjectHookSecret(ctx, 42, hook.ID, hook.URL, "new-secret", "Alice Multica Hook"); err != nil {
		t.Fatalf("update project hook secret: %v", err)
	}
	if gotMethod != http.MethodPut {
		t.Fatalf("method = %q, want %q", gotMethod, http.MethodPut)
	}
	if gotPath != "/api/v4/projects/42/hooks/123" {
		t.Fatalf("path = %q", gotPath)
	}
	if gotBody["url"] != webhookURL || gotBody["token"] != "new-secret" || gotBody["name"] != "Alice Multica Hook" {
		t.Fatalf("request body = %+v", gotBody)
	}
	if len(gotBody) != 3 {
		t.Fatalf("request body changed unrelated hook settings: %+v", gotBody)
	}
}

func TestUpdateProjectHookSecretReturnsServerError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte("hook update failed"))
	}))
	defer srv.Close()

	client, err := NewClient(Config{BaseURL: srv.URL, Token: "token"})
	if err != nil {
		t.Fatal(err)
	}
	err = client.UpdateProjectHookSecret(context.Background(), 42, 123, "https://multica.example/hook", "new-secret", "Alice Multica Hook")
	if err == nil || !strings.Contains(err.Error(), "hook update failed") {
		t.Fatalf("UpdateProjectHookSecret error = %v, want server error", err)
	}
}

func TestDeleteProjectHookReturnsNotFound(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	client, err := NewClient(Config{BaseURL: srv.URL, Token: "token"})
	if err != nil {
		t.Fatal(err)
	}
	err = client.DeleteProjectHook(context.Background(), 42, 123)
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("DeleteProjectHook error = %v, want ErrNotFound", err)
	}
}

func TestClientIgnoresProcessProxyEnvWithoutConfiguredProxy(t *testing.T) {
	t.Setenv("HTTP_PROXY", "http://127.0.0.1:1")
	t.Setenv("HTTPS_PROXY", "http://127.0.0.1:1")

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"id":42,"path_with_namespace":"group/repo","web_url":"https://code.mlamp.cn/group/repo"}`))
	}))
	defer srv.Close()

	c, err := NewClient(Config{BaseURL: srv.URL, Token: "token"})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	tr, ok := c.httpClient.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("transport = %T", c.httpClient.Transport)
	}
	if tr.Proxy != nil {
		t.Fatal("expected transport proxy to be nil")
	}

	project, err := c.GetProject(context.Background(), "group/repo")
	if err != nil {
		t.Fatalf("GetProject: %v", err)
	}
	if project.ID != 42 || project.PathWithNamespace != "group/repo" {
		t.Fatalf("project = %+v", project)
	}
}

func TestListPipelineJobsFollowsPagination(t *testing.T) {
	var paths []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.String())
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Query().Get("page") {
		case "", "1":
			w.Header().Set("X-Next-Page", "2")
			_, _ = w.Write([]byte(`[{"id":11,"name":"test","status":"failed","stage":"test","web_url":"https://gitlab/jobs/11"}]`))
		case "2":
			_, _ = w.Write([]byte(`[{"id":12,"name":"lint","status":"success","stage":"test","web_url":"https://gitlab/jobs/12"}]`))
		default:
			t.Fatalf("unexpected page %q", r.URL.Query().Get("page"))
		}
	}))
	defer srv.Close()

	c, err := NewClient(Config{BaseURL: srv.URL, Token: "token"})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	jobs, err := c.ListPipelineJobs(context.Background(), 42, 77)
	if err != nil {
		t.Fatalf("ListPipelineJobs: %v", err)
	}
	if len(jobs) != 2 || jobs[0].ID != 11 || jobs[1].ID != 12 {
		t.Fatalf("jobs = %+v", jobs)
	}
	if len(paths) != 2 || !strings.Contains(paths[1], "page=2") {
		t.Fatalf("paths = %+v", paths)
	}
}

func TestGetJobTraceTruncatesFromTail(t *testing.T) {
	body := strings.Repeat("a", 20) + "FAILED\nsecret-token"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(body))
	}))
	defer srv.Close()

	c, err := NewClient(Config{BaseURL: srv.URL, Token: "token"})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	trace, err := c.GetJobTrace(context.Background(), 42, 9, 12)
	if err != nil {
		t.Fatalf("GetJobTrace: %v", err)
	}
	if !trace.Truncated || !strings.Contains(trace.Text, "secret-token") || len(trace.Text) > 12 {
		t.Fatalf("trace = %+v", trace)
	}
}

func TestGetMergeRequestChangesParsesStats(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"changes_count":"2",
			"changes":[
				{"old_path":"a.go","new_path":"a.go","diff":"@@ -1 +1,2 @@\n-a\n+b\n+c\n"},
				{"old_path":"b.go","new_path":"b.go","diff":"@@ -1 +0,0 @@\n-z\n"}
			]
		}`))
	}))
	defer srv.Close()

	c, err := NewClient(Config{BaseURL: srv.URL, Token: "token"})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	changes, err := c.GetMergeRequestChanges(context.Background(), 42, 7)
	if err != nil {
		t.Fatalf("GetMergeRequestChanges: %v", err)
	}
	if changes.ChangedFiles() != 2 || changes.Additions() != 2 || changes.Deletions() != 2 {
		t.Fatalf("stats = files %d add %d del %d", changes.ChangedFiles(), changes.Additions(), changes.Deletions())
	}
}

func TestGetMergeRequestApprovalStateCombinesApprovalsAndRules(t *testing.T) {
	var paths []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/v4/projects/42/merge_requests/7/approvals":
			_, _ = w.Write([]byte(`{
				"approved": true,
				"approvals_required": 2,
				"approvals_left": 0,
				"approved_by": [{"user":{"id":1,"username":"grace","name":"Grace"}}]
			}`))
		case "/api/v4/projects/42/merge_requests/7/approval_state":
			_, _ = w.Write([]byte(`{
				"rules": [{
					"id": 10,
					"name": "Maintainers",
					"approved": true,
					"approvals_required": 2,
					"approved_by": [{"id":1,"username":"grace","name":"Grace"}]
				}]
			}`))
		default:
			t.Fatalf("unexpected path %q", r.URL.Path)
		}
	}))
	defer srv.Close()

	c, err := NewClient(Config{BaseURL: srv.URL, Token: "token"})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	approval, err := c.GetMergeRequestApprovalState(context.Background(), 42, 7)
	if err != nil {
		t.Fatalf("GetMergeRequestApprovalState: %v", err)
	}
	if len(paths) != 2 || paths[0] != "/api/v4/projects/42/merge_requests/7/approvals" || paths[1] != "/api/v4/projects/42/merge_requests/7/approval_state" {
		t.Fatalf("paths = %+v", paths)
	}
	if !approval.Approved || approval.ApprovalsRequired == nil || *approval.ApprovalsRequired != 2 || approval.ApprovalsLeft == nil || *approval.ApprovalsLeft != 0 {
		t.Fatalf("approval summary = %+v", approval)
	}
	if len(approval.ApprovedBy) != 1 || approval.ApprovedBy[0].User.Username != "grace" {
		t.Fatalf("approved_by = %+v", approval.ApprovedBy)
	}
	if len(approval.Rules) != 1 || approval.Rules[0].Name != "Maintainers" || len(approval.Rules[0].ApprovedBy) != 1 {
		t.Fatalf("rules = %+v", approval.Rules)
	}
}

func TestMergeMergeRequestRemovesSourceBranch(t *testing.T) {
	var gotPath string
	var gotMethod string
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotMethod = r.Method
		if got := r.Header.Get("PRIVATE-TOKEN"); got != "token" {
			t.Fatalf("PRIVATE-TOKEN = %q", got)
		}
		if err := json.NewDecoder(r.Body).Decode(&gotBody); err != nil {
			t.Fatalf("decode request body: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"iid":7,"title":"Fix issue","state":"merged","web_url":"https://code.mlamp.cn/group/repo/-/merge_requests/7","sha":"abc","created_at":"2026-07-06T00:00:00Z","updated_at":"2026-07-06T00:01:00Z"}`))
	}))
	defer srv.Close()

	c, err := NewClient(Config{BaseURL: srv.URL, Token: "token"})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	mr, err := c.MergeMergeRequest(context.Background(), 42, 7, MergeRequestMergeOptions{ShouldRemoveSourceBranch: true})
	if err != nil {
		t.Fatalf("MergeMergeRequest: %v", err)
	}
	if gotMethod != http.MethodPut {
		t.Fatalf("method = %q", gotMethod)
	}
	if gotPath != "/api/v4/projects/42/merge_requests/7/merge" {
		t.Fatalf("path = %q", gotPath)
	}
	if gotBody["should_remove_source_branch"] != true {
		t.Fatalf("request body = %+v", gotBody)
	}
	if mr.State != "merged" || mr.IID != 7 {
		t.Fatalf("merge request = %+v", mr)
	}
}
