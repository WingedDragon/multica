package vcs

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"
)

func TestRegistry(t *testing.T) {
	for _, k := range []string{"forgejo", "gitea", "gitlab"} {
		if _, ok := For(k); !ok {
			t.Errorf("registry missing provider %q", k)
		}
	}
	if _, ok := For("bitbucket"); ok {
		t.Error("unknown provider should not resolve")
	}
}

func TestKindValid(t *testing.T) {
	if !KindForgejo.Valid() || !KindGitea.Valid() || !KindGitLab.Valid() {
		t.Error("known kinds must be valid")
	}
	if Kind("svn").Valid() {
		t.Error("unknown kind must be invalid")
	}
}

func TestNormalizeInstanceURL(t *testing.T) {
	cases := map[string]string{
		"https://forgejo.example.com/": "https://forgejo.example.com",
		"  https://forge.test  ":       "https://forge.test",
		"https://forge.test/sub/":      "https://forge.test/sub",
	}
	for in, want := range cases {
		if got := NormalizeInstanceURL(in); got != want {
			t.Errorf("NormalizeInstanceURL(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestForgejoVerifySignature(t *testing.T) {
	p, _ := For("forgejo")
	secret, body := "s3cr3t", []byte(`{"action":"opened"}`)
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	valid := hex.EncodeToString(mac.Sum(nil))

	h := http.Header{}
	h.Set("X-Gitea-Signature", valid)
	if !p.VerifySignature(secret, h, body) {
		t.Error("valid signature rejected")
	}
	h.Set("X-Gitea-Signature", "sha256="+valid)
	if !p.VerifySignature(secret, h, body) {
		t.Error("sha256-prefixed signature rejected")
	}
	if p.VerifySignature("wrong", h, body) {
		t.Error("wrong secret accepted")
	}
	if p.VerifySignature(secret, h, []byte("tampered")) {
		t.Error("tampered body accepted")
	}
	// An empty secret must be rejected outright — HMAC with an empty key is
	// forgeable. Sign the body with the empty key and confirm it's still refused.
	emptyMac := hmac.New(sha256.New, []byte(""))
	emptyMac.Write(body)
	h.Set("X-Gitea-Signature", hex.EncodeToString(emptyMac.Sum(nil)))
	if p.VerifySignature("", h, body) {
		t.Error("empty secret accepted")
	}
}

func TestGitlabVerifySignature(t *testing.T) {
	p, _ := For("gitlab")
	h := http.Header{}
	h.Set("X-Gitlab-Token", "tok123")
	if !p.VerifySignature("tok123", h, nil) {
		t.Error("matching token rejected")
	}
	if p.VerifySignature("other", h, nil) {
		t.Error("mismatched token accepted")
	}
	if p.VerifySignature("", h, nil) {
		t.Error("empty stored secret must never validate")
	}
}

func TestForgejoEventKindAndParse(t *testing.T) {
	p, _ := For("gitea")
	h := http.Header{}
	h.Set("X-Gitea-Event", "pull_request")
	if p.EventKind(h, nil) != EventPullRequest {
		t.Error("pull_request not classified")
	}
	h.Set("X-Gitea-Event", "status")
	if p.EventKind(h, nil) != EventCIStatus {
		t.Error("status not classified")
	}

	pr, err := p.ParsePullRequest([]byte(`{
		"action":"closed",
		"pull_request":{"number":7,"title":"Fix MUL-1","state":"closed","merged":true,
			"html_url":"https://g/acme/widget/pulls/7","head":{"ref":"fix","sha":"abc"},
			"user":{"username":"octo"},"additions":3},
		"repository":{"name":"widget","owner":{"username":"acme"}}}`))
	if err != nil {
		t.Fatalf("ParsePullRequest: %v", err)
	}
	if pr.RepoOwner != "acme" || pr.RepoName != "widget" || pr.Number != 7 {
		t.Errorf("bad identity: %+v", pr)
	}
	if pr.State != "merged" {
		t.Errorf("state = %q, want merged", pr.State)
	}
	if pr.AuthorLogin != "octo" || pr.HeadSHA != "abc" {
		t.Errorf("bad author/sha: %+v", pr)
	}
	if !pr.Terminal() {
		t.Error("closed action must be terminal")
	}

	st, err := p.ParseCIStatus([]byte(`{"sha":"abc","context":"ci","state":"success"}`))
	if err != nil {
		t.Fatalf("ParseCIStatus: %v", err)
	}
	if st.State != "passed" {
		t.Errorf("state = %q, want passed", st.State)
	}
}

func TestGitlabParse(t *testing.T) {
	p, _ := For("gitlab")
	h := http.Header{}
	h.Set("X-Gitlab-Event", "Merge Request Hook")
	if p.EventKind(h, nil) != EventPullRequest {
		t.Error("MR hook not classified")
	}

	pr, err := p.ParsePullRequest([]byte(`{
		"object_kind":"merge_request",
		"user":{"username":"alice","avatar_url":"a"},
		"project":{"path_with_namespace":"group/sub/widget"},
		"object_attributes":{"iid":42,"title":"Add MUL-9","state":"merged","action":"merge",
			"source_branch":"feat","url":"https://gl/group/sub/widget/-/merge_requests/42",
			"last_commit":{"id":"deadbeef"}}}`))
	if err != nil {
		t.Fatalf("ParsePullRequest: %v", err)
	}
	if pr.RepoOwner != "group/sub" || pr.RepoName != "widget" {
		t.Errorf("namespace split wrong: owner=%q name=%q", pr.RepoOwner, pr.RepoName)
	}
	if pr.Number != 42 || pr.State != "merged" || pr.HeadSHA != "deadbeef" {
		t.Errorf("bad MR: %+v", pr)
	}
	if !pr.Terminal() {
		t.Error("merge action must be terminal")
	}

	h.Set("X-Gitlab-Event", "Pipeline Hook")
	if p.EventKind(h, nil) != EventCIStatus {
		t.Error("pipeline hook not classified")
	}
	st, err := p.ParseCIStatus([]byte(`{"object_kind":"pipeline","object_attributes":{"sha":"deadbeef","status":"failed"}}`))
	if err != nil {
		t.Fatalf("ParseCIStatus: %v", err)
	}
	if st.SHA != "deadbeef" || st.State != "failed" || st.Context != "gitlab/pipeline" {
		t.Errorf("bad status: %+v", st)
	}
}

func TestGitlabSystemHookMergeRequest(t *testing.T) {
	p, _ := For("gitlab")
	h := http.Header{}
	h.Set("X-Gitlab-Event", "System Hook")
	// GitLab 19.5.3-ee: https://github.com/multica-ai/multica/pull/10411#issuecomment-6943978266
	body, err := os.ReadFile("testdata/gitlab-system-hook-merge.json")
	if err != nil {
		t.Fatal(err)
	}
	if got := p.EventKind(h, body); got != EventPullRequest {
		t.Fatalf("system MR kind = %v, want EventPullRequest", got)
	}
	pr, err := p.ParsePullRequest(body)
	if err != nil {
		t.Fatal(err)
	}
	if pr.State != "merged" || pr.RepoOwner != "example-user" || pr.RepoName != "example-project" || pr.Number != 10 ||
		pr.Body != "" || pr.HeadSHA != "2222222222222222222222222222222222222222" ||
		pr.UpdatedAt != "2026-11-01T09:15:57.549Z" || pr.AuthorLogin != "project_200_bot_00000000000000000000000000000000" {
		t.Fatalf("system MR = %+v", pr)
	}
	if got := p.EventKind(h, []byte(`{"object_kind":"project_create"}`)); got != EventOther {
		t.Fatalf("unrelated system event kind = %v, want EventOther", got)
	}
}

func TestGitlabSystemHookStateUsesExplicitState(t *testing.T) {
	p, _ := For("gitlab")
	for _, tc := range []struct {
		state   string
		stateID int
		want    string
	}{
		{"merged", 2, "merged"},
		{"closed", 3, "closed"},
		{"opened", 3, "open"},
		{"", 3, "open"},
	} {
		name := tc.state
		if name == "" {
			name = "missing_state"
		}
		t.Run(name, func(t *testing.T) {
			body, err := os.ReadFile("testdata/gitlab-system-hook-merge.json")
			if err != nil {
				t.Fatal(err)
			}
			var payload map[string]any
			if err := json.Unmarshal(body, &payload); err != nil {
				t.Fatal(err)
			}
			attrs := payload["object_attributes"].(map[string]any)
			attrs["state"], attrs["state_id"] = tc.state, tc.stateID
			if tc.state == "" {
				delete(attrs, "state")
			}
			body, err = json.Marshal(payload)
			if err != nil {
				t.Fatal(err)
			}
			pr, err := p.ParsePullRequest(body)
			if err != nil || pr.State != tc.want {
				t.Fatalf("state=%q state_id=%d: got %q err=%v, want %q", tc.state, tc.stateID, pr.State, err, tc.want)
			}
		})
	}
}

func TestGitlabDraftDetection(t *testing.T) {
	p, _ := For("gitlab")
	pr, _ := p.ParsePullRequest([]byte(`{"object_kind":"merge_request",
		"project":{"path_with_namespace":"g/r"},
		"object_attributes":{"iid":1,"title":"Draft: wip","state":"opened","work_in_progress":true}}`))
	if pr.State != "draft" {
		t.Errorf("state = %q, want draft", pr.State)
	}
}

func TestValidateToken(t *testing.T) {
	cases := []struct {
		kind, path, authHeader, authValue, bodyJSON, wantLogin string
	}{
		{"forgejo", "/api/v1/user", "Authorization", "token tok", `{"login":"octo"}`, "octo"},
		{"gitlab", "/api/v4/user", "PRIVATE-TOKEN", "tok", `{"username":"alice"}`, "alice"},
	}
	for _, c := range cases {
		t.Run(c.kind, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != c.path {
					t.Errorf("path = %q, want %q", r.URL.Path, c.path)
				}
				if r.Header.Get(c.authHeader) != c.authValue {
					t.Errorf("%s = %q, want %q", c.authHeader, r.Header.Get(c.authHeader), c.authValue)
				}
				_, _ = w.Write([]byte(c.bodyJSON))
			}))
			defer srv.Close()
			p, _ := For(c.kind)
			acct, err := p.ValidateToken(context.Background(), srv.URL, "tok")
			if err != nil {
				t.Fatalf("ValidateToken: %v", err)
			}
			if acct.Login != c.wantLogin {
				t.Errorf("login = %q, want %q", acct.Login, c.wantLogin)
			}
		})
	}
}

// GitLab sends timestamps as "2017-09-20 08:31:45 UTC" (not RFC3339). The
// shared handler parser is RFC3339-only, so without normalization every GitLab
// timestamp was dropped and replaced with ingestion time — defeating the
// monotonic guards on PR upsert and commit status for GitLab specifically, and
// skewing PR list ordering. These assert the adapter normalizes to RFC3339.
func TestNormalizeGitLabTime(t *testing.T) {
	cases := []struct{ in, want string }{
		{"", ""},
		{"2017-09-20 08:31:45 UTC", "2017-09-20T08:31:45Z"},
		{"2017-09-20T08:31:45Z", "2017-09-20T08:31:45Z"},
		// Sub-second precision is preserved (RFC3339Nano output) so two events in
		// the same wall-clock second still order correctly under the monotonic
		// guards; a zero fraction still formats without a decimal part.
		{"2017-09-20T08:31:45.123Z", "2017-09-20T08:31:45.123Z"},
		{"2017-09-20 08:31:45.123456 UTC", "2017-09-20T08:31:45.123456Z"},
		{"not a time", ""},
	}
	for _, c := range cases {
		if got := normalizeGitLabTime(c.in); got != c.want {
			t.Errorf("normalizeGitLabTime(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestGitlabParseTimestamps(t *testing.T) {
	p, _ := For("gitlab")
	// A merge_request payload with GitLab-format created_at/updated_at.
	pr, err := p.ParsePullRequest([]byte(`{
		"object_kind":"merge_request",
		"project":{"path_with_namespace":"g/r"},
		"object_attributes":{"iid":1,"title":"MUL-1","state":"opened","action":"open",
			"created_at":"2017-09-20 08:31:45 UTC","updated_at":"2017-09-21 09:00:00 UTC",
			"last_commit":{"id":"abc"}}}`))
	if err != nil {
		t.Fatalf("ParsePullRequest: %v", err)
	}
	if pr.CreatedAt != "2017-09-20T08:31:45Z" || pr.UpdatedAt != "2017-09-21T09:00:00Z" {
		t.Errorf("timestamps not normalized: created=%q updated=%q", pr.CreatedAt, pr.UpdatedAt)
	}
	if _, err := time.Parse(time.RFC3339, pr.UpdatedAt); err != nil {
		t.Errorf("UpdatedAt not RFC3339: %v", err)
	}

	// A pipeline payload's finished_at feeds the commit-status guard.
	st, err := p.ParseCIStatus([]byte(`{"object_kind":"pipeline",
		"object_attributes":{"sha":"abc","status":"success",
			"created_at":"2017-09-20 08:00:00 UTC","finished_at":"2017-09-20 08:05:00 UTC"}}`))
	if err != nil {
		t.Fatalf("ParseCIStatus: %v", err)
	}
	if st.UpdatedAt != "2017-09-20T08:05:00Z" {
		t.Errorf("CI UpdatedAt = %q, want finished_at normalized", st.UpdatedAt)
	}
}

func TestGitLabSystemHookWakeupPayloads(t *testing.T) {
	provider, _ := For(string(KindGitLab))
	for _, tc := range []struct {
		objectKind string
		want       EventKind
	}{
		{"merge_request", EventPullRequest},
		{"pipeline", EventCIStatus},
	} {
		header := http.Header{"X-Gitlab-Event": []string{"System Hook"}}
		body := []byte(`{"object_kind":"` + tc.objectKind + `"}`)
		if got := provider.EventKind(header, body); got != tc.want {
			t.Errorf("System Hook %q kind = %v, want %v", tc.objectKind, got, tc.want)
		}
	}

	mr, err := provider.ParsePullRequest([]byte(`{
		"object_kind":"merge_request",
		"project":{"path_with_namespace":"group/sub/repo"},
		"object_attributes":{
			"iid":17,"state":"opened","action":"open","source_branch":"feature/fix",
			"source":{"id":55,"path_with_namespace":"fork/user/repo"},
			"last_commit":{"id":"head-current"},
			"has_conflicts":true,"detailed_merge_status":"checking"
		}}`))
	if err != nil {
		t.Fatalf("ParsePullRequest: %v", err)
	}
	if mr.SourceProjectID != 55 || mr.SourceProjectPath != "fork/user/repo" {
		t.Errorf("source project = %d/%q, want 55/fork/user/repo", mr.SourceProjectID, mr.SourceProjectPath)
	}
	if mr.HeadSHA != "head-current" {
		t.Errorf("head SHA = %q, want current commit", mr.HeadSHA)
	}
	if mr.HasConflicts != nil {
		t.Errorf("conflicts during asynchronous checking = %v, want unknown", *mr.HasConflicts)
	}

	status, err := provider.ParseCIStatus([]byte(`{
		"object_kind":"pipeline",
		"project":{"id":55,"path_with_namespace":"fork/user/repo"},
		"object_attributes":{"id":91,"sha":"head","ref":"feature/fix","status":"skipped"}
	}`))
	if err != nil {
		t.Fatalf("ParseCIStatus: %v", err)
	}
	if status.PipelineID != 91 || status.ProjectID != 55 || status.Ref != "feature/fix" || status.SHA != "head" || status.State != "passed" {
		t.Errorf("pipeline identity/status = %+v", status)
	}
}

func TestGitLabConflictStateRequiresAuthoritativeStatus(t *testing.T) {
	yes, no := true, false
	for _, tc := range []struct {
		hasConflicts *bool
		detailed     string
		want         *bool
	}{
		{&yes, "checking", nil},
		{&no, "preparing", nil},
		{&no, "cannot_be_merged", nil},
		{&yes, "cannot_be_merged", &yes},
		{&no, "can_be_merged", &no},
	} {
		got := gitLabConflictState(tc.hasConflicts, tc.detailed)
		if (got == nil) != (tc.want == nil) || (got != nil && *got != *tc.want) {
			t.Errorf("gitLabConflictState(%v, %q) = %v, want %v", tc.hasConflicts, tc.detailed, got, tc.want)
		}
	}
}

func TestGitLabWakeupSnapshotUsesCurrentHeadAndForkProject(t *testing.T) {
	var targetDetails, sourceDetails bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.EscapedPath() {
		case "/api/v4/projects/group%2Fsub%2Frepo":
			_, _ = w.Write([]byte(`{"id":10,"path_with_namespace":"group/sub/repo"}`))
		case "/api/v4/projects/10/merge_requests/17":
			_, _ = w.Write([]byte(`{"iid":17,"state":"opened","sha":"head-current","source_branch":"feature/fix","source_project_id":20,"source_project":{"id":20,"path_with_namespace":"fork/user/repo"},"has_conflicts":false,"detailed_merge_status":"checking","updated_at":"2025-01-02T03:04:05Z"}`))
		case "/api/v4/projects/10/merge_requests/17/pipelines":
			_, _ = w.Write([]byte(`[{"id":101,"sha":"head-current","ref":"refs/merge-requests/17/head","status":"success"}]`))
		case "/api/v4/projects/10/pipelines/101":
			targetDetails = true
			_, _ = w.Write([]byte(`{"id":101,"project_id":10,"sha":"head-current","ref":"refs/merge-requests/17/head","status":"success","updated_at":"2025-01-02T03:05:00Z"}`))
		case "/api/v4/projects/20/pipelines":
			if r.URL.Query().Get("sha") != "head-current" || r.URL.Query().Get("ref") != "feature/fix" {
				t.Errorf("source pipeline filters = %v", r.URL.Query())
			}
			_, _ = w.Write([]byte(`[
				{"id":202,"sha":"head-current","ref":"feature/fix","status":"failed"},
				{"id":203,"sha":"old-head","ref":"feature/fix","status":"success"},
				{"id":204,"sha":"head-current","ref":"old-branch","status":"success"}
			]`))
		case "/api/v4/projects/20/pipelines/202":
			sourceDetails = true
			_, _ = w.Write([]byte(`{"id":202,"project_id":20,"sha":"head-current","ref":"feature/fix","status":"failed","updated_at":"2025-01-02T03:06:00Z"}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	reader := &gitlabProvider{}
	mrSnapshot, err := reader.ReadGitLabWakeupMergeRequest(context.Background(), srv.URL, "group/sub/repo", "tok", 17)
	if err != nil {
		t.Fatalf("ReadGitLabWakeupMergeRequest: %v", err)
	}
	pipelineSnapshot := reader.ReadGitLabWakeupPipelines(context.Background(), srv.URL, "tok", 17, mrSnapshot)
	if mrSnapshot.MergeRequest.HasConflicts != nil {
		t.Errorf("asynchronous conflict state = %v, want unknown", *mrSnapshot.MergeRequest.HasConflicts)
	}
	if !targetDetails || !sourceDetails {
		t.Errorf("pipeline details fetched: target=%v source=%v", targetDetails, sourceDetails)
	}
	if len(pipelineSnapshot.Pipelines) != 2 {
		t.Fatalf("pipelines = %d, want current target MR and fork branch pipelines", len(pipelineSnapshot.Pipelines))
	}
	if pipelineSnapshot.Pipelines[0].ID != 101 || pipelineSnapshot.Pipelines[0].ProjectID != 10 || pipelineSnapshot.Pipelines[0].Status != "passed" {
		t.Errorf("target MR pipeline = %+v", pipelineSnapshot.Pipelines[0])
	}
	if pipelineSnapshot.Pipelines[1].ID != 202 || pipelineSnapshot.Pipelines[1].ProjectID != 20 || pipelineSnapshot.Pipelines[1].ProjectPath != "fork/user/repo" || pipelineSnapshot.Pipelines[1].Status != "failed" {
		t.Errorf("fork branch pipeline = %+v", pipelineSnapshot.Pipelines[1])
	}
}

func TestGitLabWakeupSnapshotFailsWhenForkProjectIsUnreadable(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.EscapedPath() {
		case "/api/v4/projects/group%2Frepo":
			_, _ = w.Write([]byte(`{"id":10,"path_with_namespace":"group/repo"}`))
		case "/api/v4/projects/10/merge_requests/17":
			_, _ = w.Write([]byte(`{"iid":17,"state":"opened","sha":"head","source_branch":"feature","source_project_id":20,"source_project":{"id":20,"path_with_namespace":"fork/repo"},"updated_at":"2025-01-02T03:04:05Z"}`))
		case "/api/v4/projects/10/merge_requests/17/pipelines":
			_, _ = w.Write([]byte(`[]`))
		case "/api/v4/projects/20/pipelines":
			w.WriteHeader(http.StatusForbidden)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	reader := &gitlabProvider{}
	mrSnapshot, err := reader.ReadGitLabWakeupMergeRequest(context.Background(), srv.URL, "group/repo", "tok", 17)
	if err != nil {
		t.Fatalf("ReadGitLabWakeupMergeRequest: %v", err)
	}
	pipelineSnapshot := reader.ReadGitLabWakeupPipelines(context.Background(), srv.URL, "tok", 17, mrSnapshot)
	if mrSnapshot.MergeRequest.HeadSHA != "head" || pipelineSnapshot.PipelineError == nil {
		t.Fatalf("MR = %+v, pipelines = %+v; want current MR state and a Pipeline access error", mrSnapshot.MergeRequest, pipelineSnapshot)
	}
}

func TestForgejoStatusTimestamp(t *testing.T) {
	p, _ := For("forgejo")
	// Forgejo/Gitea status payloads carry RFC3339 created_at/updated_at.
	st, err := p.ParseCIStatus([]byte(`{"sha":"abc","context":"ci","state":"success",
		"created_at":"2017-09-20T08:00:00Z","updated_at":"2017-09-20T08:05:00Z"}`))
	if err != nil {
		t.Fatalf("ParseCIStatus: %v", err)
	}
	if st.UpdatedAt != "2017-09-20T08:05:00Z" {
		t.Errorf("CI UpdatedAt = %q, want updated_at", st.UpdatedAt)
	}
}

func TestGitlabRepositoryReadable(t *testing.T) {
	cases := []struct {
		status int
		want   bool
	}{
		{http.StatusOK, true},
		{http.StatusForbidden, false},
		{http.StatusNotFound, false},
	}
	for _, tc := range cases {
		t.Run(http.StatusText(tc.status), func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/api/v4/projects/group%2Fsub%2Frepo" && r.URL.EscapedPath() != "/api/v4/projects/group%2Fsub%2Frepo" {
					t.Errorf("path = %q", r.URL.Path)
				}
				if r.Header.Get("PRIVATE-TOKEN") != "tok" {
					t.Errorf("PRIVATE-TOKEN missing")
				}
				w.WriteHeader(tc.status)
				if tc.status == http.StatusOK {
					_, _ = w.Write([]byte(`{"path_with_namespace":"group/sub/repo"}`))
				}
			}))
			defer srv.Close()
			got, err := (&gitlabProvider{}).RepositoryReadable(context.Background(), srv.URL, "group/sub", "repo", "tok")
			if err != nil || got != tc.want {
				t.Fatalf("got=%v err=%v want=%v", got, err, tc.want)
			}
		})
	}
}
func TestValidateTokenUnauthorized(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()
	p, _ := For("forgejo")
	_, err := p.ValidateToken(context.Background(), srv.URL, "bad")
	if !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("err = %v, want ErrUnauthorized", err)
	}
}
