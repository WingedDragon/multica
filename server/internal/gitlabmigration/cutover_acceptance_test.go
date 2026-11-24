package gitlabmigration

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/multica-ai/multica/server/internal/handler"
	"github.com/multica-ai/multica/server/internal/testutil"
	"github.com/multica-ai/multica/server/internal/util/secretbox"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

const (
	acceptanceProjectPath = "group/sub/project"
	acceptanceMRIID       = int32(81)
	acceptanceProjectID   = int64(77)
	acceptancePipelineID  = int64(901)
	acceptanceJobID       = int64(903)
)

type cutoverAcceptanceFixture struct {
	pool       *pgxpool.Pool
	remote     *cutoverAcceptanceGitLab
	remoteHTTP *httptest.Server
	appHTTP    *httptest.Server
	runner     *Runner
	handler    *handler.Handler
	env        Environment
	box        *secretbox.Box
	at         time.Time
}

type cutoverAcceptanceSource struct {
	workspaceID, userID, issueID string
	instance, baseURL, path      string
	legacyConnectionID           string
	canonicalConnectionID        string
	unifiedConnectionID          string
	legacyBindingID              string
	legacyMRID                   string
	canonicalProjectID           string
	canonicalMRID                string
	projectID, hookID            int64
	token, legacySecret          string
	unifiedSecret                string
	legacyTokenCipher            string
	legacySecretCipher           string
	canonicalTokenCipher         string
	canonicalSecretCipher        string
}

type cutoverAcceptanceHook struct {
	ID                    int64  `json:"id"`
	URL                   string `json:"url"`
	Token                 string `json:"token"`
	MergeRequestsEvents   bool   `json:"merge_requests_events"`
	PipelineEvents        bool   `json:"pipeline_events"`
	JobEvents             bool   `json:"job_events"`
	NoteEvents            bool   `json:"note_events"`
	EnableSSLVerification bool   `json:"enable_ssl_verification"`
}

type cutoverAcceptanceGitLab struct {
	mu                        sync.RWMutex
	tokens                    map[string]map[string]string
	projects                  map[string]map[int64]string
	hooks                     map[string]map[int64]map[int64]cutoverAcceptanceHook
	failPuts                  map[string]int
	unsupportedGroupHooks     map[string]bool
	systemHookInventoryStatus map[string]int
	createdAt                 string
	updatedAt                 string
	onHookUpdate              func(before, after cutoverAcceptanceHook)
}

func newCutoverAcceptanceFixture(t *testing.T) *cutoverAcceptanceFixture {
	t.Helper()
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		t.Skip("integration acceptance requires a disposable Postgres DATABASE_URL")
	}
	t.Setenv("GITLAB_PROXY_URL", "")
	pool := testutil.IsolatedDB(t, dsn)
	remote := &cutoverAcceptanceGitLab{
		tokens:                    make(map[string]map[string]string),
		projects:                  make(map[string]map[int64]string),
		hooks:                     make(map[string]map[int64]map[int64]cutoverAcceptanceHook),
		failPuts:                  make(map[string]int),
		unsupportedGroupHooks:     make(map[string]bool),
		systemHookInventoryStatus: make(map[string]int),
		createdAt:                 time.Now().UTC().Add(-2 * time.Hour).Truncate(time.Second).Format(time.RFC3339),
		updatedAt:                 time.Now().UTC().Add(time.Hour).Truncate(time.Second).Format(time.RFC3339),
	}
	remoteHTTP := httptest.NewServer(remote)
	boxKey := make([]byte, 32)
	for i := range boxKey {
		boxKey[i] = 0x5A
	}
	env := Environment{
		VCSSecretKey:          base64.StdEncoding.EncodeToString(boxKey),
		VCSIntegrationEnabled: true,
	}
	box, err := env.box()
	if err != nil {
		remoteHTTP.Close()
		t.Fatalf("create test credential box: %v", err)
	}
	replayHandler, err := handler.NewGitLabMigrationReplayHandler(pool, box, handler.Config{VCSIntegrationEnabled: true})
	if err != nil {
		remoteHTTP.Close()
		t.Fatalf("create real GitLab replay handler: %v", err)
	}
	router := chi.NewRouter()
	router.Post("/api/webhooks/vcs/{connectionId}", replayHandler.HandleVCSWebhook)
	appHTTP := httptest.NewServer(router)
	env.PublicURL = appHTTP.URL
	fixture := &cutoverAcceptanceFixture{
		pool: pool, remote: remote, remoteHTTP: remoteHTTP, appHTTP: appHTTP,
		runner: NewRunner(pool, env), handler: replayHandler, env: env, box: box,
		at: time.Now().UTC().Add(-time.Hour).Truncate(time.Second),
	}
	// A database upgraded from the legacy release enters the operator-controlled
	// maintenance window before data-bearing cutover commands are allowed.
	if _, err := pool.Exec(context.Background(), `
		UPDATE vcs_gitlab_migration_state
		SET phase='maintenance',maintenance_mode=true,generation=0,began_at=NULL,last_error=NULL
		WHERE id=1
	`); err != nil {
		appHTTP.Close()
		remoteHTTP.Close()
		t.Fatalf("prepare legacy migration state: %v", err)
	}
	t.Cleanup(appHTTP.Close)
	t.Cleanup(remoteHTTP.Close)
	return fixture
}

func (f *cutoverAcceptanceFixture) addInstance(instance string, token, username string, projects map[int64]string) {
	f.remote.mu.Lock()
	defer f.remote.mu.Unlock()
	if f.remote.tokens[instance] == nil {
		f.remote.tokens[instance] = make(map[string]string)
	}
	f.remote.tokens[instance][token] = username
	f.remote.projects[instance] = make(map[int64]string, len(projects))
	for id, projectPath := range projects {
		f.remote.projects[instance][id] = projectPath
	}
}

func (f *cutoverAcceptanceFixture) addHook(instance string, projectID int64, hook cutoverAcceptanceHook) {
	f.remote.mu.Lock()
	defer f.remote.mu.Unlock()
	if f.remote.hooks[instance] == nil {
		f.remote.hooks[instance] = make(map[int64]map[int64]cutoverAcceptanceHook)
	}
	if f.remote.hooks[instance][projectID] == nil {
		f.remote.hooks[instance][projectID] = make(map[int64]cutoverAcceptanceHook)
	}
	f.remote.hooks[instance][projectID][hook.ID] = hook
}

func (f *cutoverAcceptanceFixture) remoteHook(instance string, projectID, hookID int64) (cutoverAcceptanceHook, bool) {
	f.remote.mu.RLock()
	defer f.remote.mu.RUnlock()
	hook, ok := f.remote.hooks[instance][projectID][hookID]
	return hook, ok
}

func (f *cutoverAcceptanceFixture) failNextHookUpdate(instance string, projectID, hookID int64) {
	f.remote.mu.Lock()
	defer f.remote.mu.Unlock()
	f.remote.failPuts[cutoverAcceptanceHookKey(instance, projectID, hookID)]++
}

func (g *cutoverAcceptanceGitLab) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	escaped := r.URL.EscapedPath()
	var instance, apiPath string
	if strings.EqualFold(r.Host, "code.mlamp.cn") {
		instance = "code.mlamp.cn"
		if !strings.HasPrefix(escaped, "/api/v4/") {
			http.NotFound(w, r)
			return
		}
		apiPath = strings.TrimPrefix(escaped, "/api/v4")
	} else {
		trimmed := strings.TrimPrefix(escaped, "/")
		separator := strings.IndexByte(trimmed, '/')
		if separator <= 0 {
			http.NotFound(w, r)
			return
		}
		instance = trimmed[:separator]
		apiPath = strings.TrimPrefix(trimmed[separator:], "/api/v4")
		if apiPath == trimmed[separator:] {
			http.NotFound(w, r)
			return
		}
	}
	route, err := url.PathUnescape(apiPath)
	if err != nil {
		http.Error(w, "bad path", http.StatusBadRequest)
		return
	}
	token := r.Header.Get("PRIVATE-TOKEN")
	g.mu.RLock()
	username := g.tokens[instance][token]
	unsupportedGroupHooks := g.unsupportedGroupHooks[instance]
	systemHookStatus := g.systemHookInventoryStatus[instance]
	g.mu.RUnlock()
	if username == "" {
		http.Error(w, `{"message":"401 Unauthorized"}`, http.StatusUnauthorized)
		return
	}
	if r.Method == http.MethodGet && route == "/user" {
		cutoverAcceptanceJSON(w, http.StatusOK, map[string]any{"id": 42, "username": username, "is_admin": false})
		return
	}
	if r.Method == http.MethodGet && route == "/groups" {
		cutoverAcceptanceJSON(w, http.StatusOK, []any{
			map[string]any{"id": 701, "full_path": "group"},
			map[string]any{"id": 702, "full_path": "group/sub"},
		})
		return
	}
	if r.Method == http.MethodGet && strings.HasPrefix(route, "/groups/") && strings.Contains(route, "/members/all/") {
		cutoverAcceptanceJSON(w, http.StatusOK, map[string]int{"access_level": 50})
		return
	}
	if r.Method == http.MethodGet && strings.HasPrefix(route, "/groups/") && strings.HasSuffix(route, "/hooks") {
		if unsupportedGroupHooks {
			http.NotFound(w, r)
			return
		}
		cutoverAcceptanceJSON(w, http.StatusOK, []any{})
		return
	}
	if r.Method == http.MethodGet && route == "/hooks" {
		if systemHookStatus != 0 {
			http.Error(w, "System Hook inventory is not readable", systemHookStatus)
			return
		}
		cutoverAcceptanceJSON(w, http.StatusOK, []any{})
		return
	}
	if r.Method == http.MethodGet && strings.HasPrefix(route, "/hooks/") {
		cutoverAcceptanceJSON(w, http.StatusOK, []any{})
		return
	}
	if !strings.HasPrefix(route, "/projects/") {
		http.NotFound(w, r)
		return
	}
	projectKey := strings.TrimPrefix(route, "/projects/")
	first, rest, hasRest := strings.Cut(projectKey, "/")
	projectID, idErr := strconv.ParseInt(first, 10, 64)
	if idErr != nil {
		if r.Method != http.MethodGet {
			http.NotFound(w, r)
			return
		}
		g.mu.RLock()
		for id, projectPath := range g.projects[instance] {
			if projectPath == projectKey {
				g.mu.RUnlock()
				cutoverAcceptanceJSON(w, http.StatusOK, cutoverAcceptanceProject(id, projectPath))
				return
			}
		}
		g.mu.RUnlock()
		http.NotFound(w, r)
		return
	}
	g.mu.RLock()
	projectPath, projectExists := g.projects[instance][projectID]
	g.mu.RUnlock()
	if !projectExists {
		http.NotFound(w, r)
		return
	}
	if !hasRest {
		if r.Method == http.MethodGet {
			cutoverAcceptanceJSON(w, http.StatusOK, cutoverAcceptanceProject(projectID, projectPath))
			return
		}
		http.NotFound(w, r)
		return
	}
	parts := strings.Split(rest, "/")
	if len(parts) >= 1 && parts[0] == "hooks" {
		if len(parts) == 1 && r.Method == http.MethodGet {
			g.mu.RLock()
			hooks := make([]cutoverAcceptanceHook, 0, len(g.hooks[instance][projectID]))
			for _, hook := range g.hooks[instance][projectID] {
				hooks = append(hooks, hook)
			}
			g.mu.RUnlock()
			cutoverAcceptanceJSON(w, http.StatusOK, hooks)
			return
		}
		if len(parts) == 2 {
			hookID, parseErr := strconv.ParseInt(parts[1], 10, 64)
			if parseErr != nil {
				http.NotFound(w, r)
				return
			}
			switch r.Method {
			case http.MethodGet:
				hook, ok := g.hook(instance, projectID, hookID)
				if !ok {
					http.NotFound(w, r)
					return
				}
				cutoverAcceptanceJSON(w, http.StatusOK, hook)
				return
			case http.MethodPut:
				g.updateHook(w, r, instance, projectID, hookID)
				return
			}
		}
	}
	if len(parts) >= 2 && parts[0] == "merge_requests" && parts[1] == strconv.Itoa(int(acceptanceMRIID)) {
		if len(parts) == 2 && r.Method == http.MethodGet {
			cutoverAcceptanceJSON(w, http.StatusOK, g.mergeRequest(projectID, projectPath))
			return
		}
		if len(parts) == 3 && parts[2] == "changes" && r.Method == http.MethodGet {
			cutoverAcceptanceJSON(w, http.StatusOK, map[string]any{
				"changes_count": "1",
				"changes":       []any{map[string]any{"old_path": "file.txt", "new_path": "file.txt", "diff": "@@ -1 +1 @@\n+new\n-old"}},
			})
			return
		}
		if len(parts) == 3 && parts[2] == "approvals" && r.Method == http.MethodGet {
			cutoverAcceptanceJSON(w, http.StatusOK, map[string]any{
				"approved": true, "approvals_required": 1, "approvals_left": 0,
				"approved_by": []any{map[string]any{"user": map[string]any{"username": "reviewer"}}},
			})
			return
		}
		if len(parts) == 3 && parts[2] == "approval_state" && r.Method == http.MethodGet {
			cutoverAcceptanceJSON(w, http.StatusOK, map[string]any{"rules": []any{map[string]any{"id": 1, "name": "reviewer", "approved": true, "approvals_required": 1}}})
			return
		}
		if len(parts) == 3 && parts[2] == "discussions" && r.Method == http.MethodGet {
			cutoverAcceptanceJSON(w, http.StatusOK, []any{map[string]any{
				"id": "discussion-1", "individual_note": false, "resolved": false,
				"notes": []any{map[string]any{"id": 9901, "body": "review note", "created_at": g.createdAt, "updated_at": g.updatedAt, "author": map[string]any{"username": "reviewer"}}},
			}})
			return
		}
		if len(parts) == 3 && parts[2] == "pipelines" && r.Method == http.MethodGet {
			cutoverAcceptanceJSON(w, http.StatusOK, []any{g.pipeline(projectID)})
			return
		}
	}
	if len(parts) >= 1 && parts[0] == "pipelines" {
		if len(parts) == 1 && r.Method == http.MethodGet {
			cutoverAcceptanceJSON(w, http.StatusOK, []any{g.pipeline(projectID)})
			return
		}
		if len(parts) == 3 && parts[1] == strconv.FormatInt(acceptancePipelineID, 10) && parts[2] == "jobs" && r.Method == http.MethodGet {
			cutoverAcceptanceJSON(w, http.StatusOK, []any{map[string]any{
				"id": acceptanceJobID, "name": "unit-tests", "stage": "test", "status": "failed",
				"ref": "feature", "sha": "head-77", "web_url": "https://gitlab.example.test/jobs/903",
				"started_at": g.createdAt, "finished_at": g.updatedAt,
			}})
			return
		}
		if len(parts) == 2 && parts[1] == strconv.FormatInt(acceptancePipelineID, 10) && r.Method == http.MethodGet {
			cutoverAcceptanceJSON(w, http.StatusOK, g.pipeline(projectID))
			return
		}
	}
	if len(parts) == 3 && parts[0] == "jobs" && parts[1] == strconv.FormatInt(acceptanceJobID, 10) && parts[2] == "trace" && r.Method == http.MethodGet {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "test failed: expected assertion\n")
		return
	}
	http.NotFound(w, r)
}

func (g *cutoverAcceptanceGitLab) hook(instance string, projectID, hookID int64) (cutoverAcceptanceHook, bool) {
	g.mu.RLock()
	defer g.mu.RUnlock()
	hook, ok := g.hooks[instance][projectID][hookID]
	return hook, ok
}

func (g *cutoverAcceptanceGitLab) updateHook(w http.ResponseWriter, r *http.Request, instance string, projectID, hookID int64) {
	key := cutoverAcceptanceHookKey(instance, projectID, hookID)
	g.mu.Lock()
	if g.failPuts[key] > 0 {
		g.failPuts[key]--
		g.mu.Unlock()
		http.Error(w, `{"message":"403 Forbidden"}`, http.StatusForbidden)
		return
	}
	hook, ok := g.hooks[instance][projectID][hookID]
	if !ok {
		g.mu.Unlock()
		http.NotFound(w, r)
		return
	}
	previous := hook
	var body map[string]any
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		g.mu.Unlock()
		http.Error(w, "invalid JSON", http.StatusBadRequest)
		return
	}
	if value, ok := body["url"].(string); ok {
		hook.URL = value
	}
	if value, ok := body["token"].(string); ok {
		hook.Token = value
	}
	if value, ok := body["merge_requests_events"].(bool); ok {
		hook.MergeRequestsEvents = value
	}
	if value, ok := body["pipeline_events"].(bool); ok {
		hook.PipelineEvents = value
	}
	if value, ok := body["job_events"].(bool); ok {
		hook.JobEvents = value
	}
	if value, ok := body["note_events"].(bool); ok {
		hook.NoteEvents = value
	}
	if value, ok := body["enable_ssl_verification"].(bool); ok {
		hook.EnableSSLVerification = value
	}
	g.hooks[instance][projectID][hookID] = hook
	onHookUpdate := g.onHookUpdate
	g.mu.Unlock()
	if onHookUpdate != nil && previous.URL != hook.URL {
		onHookUpdate(previous, hook)
	}
	cutoverAcceptanceJSON(w, http.StatusOK, hook)
}

func (g *cutoverAcceptanceGitLab) pipeline(projectID int64) map[string]any {
	return map[string]any{
		"id": acceptancePipelineID, "project_id": projectID, "sha": "head-77", "ref": "feature",
		"status": "failed", "web_url": "https://gitlab.example.test/pipelines/901",
		"created_at": g.createdAt, "finished_at": g.updatedAt, "updated_at": g.updatedAt,
	}
}

func (g *cutoverAcceptanceGitLab) mergeRequest(projectID int64, projectPath string) map[string]any {
	noConflicts := false
	return map[string]any{
		"iid": acceptanceMRIID, "target_project_id": projectID, "source_project_id": projectID,
		"title": "authoritative GitLab title", "description": "authoritative GitLab description", "state": "opened",
		"web_url":       fmt.Sprintf("https://gitlab.example.test/%s/-/merge_requests/%d", projectPath, acceptanceMRIID),
		"source_branch": "feature", "target_branch": "main", "sha": "head-77",
		"source_project":   map[string]any{"id": projectID, "path_with_namespace": projectPath},
		"merge_commit_sha": "", "detailed_merge_status": "can_be_merged", "has_conflicts": noConflicts,
		"created_at": g.createdAt, "updated_at": g.updatedAt,
		"author":    map[string]any{"username": "merge-author"},
		"reviewers": []any{map[string]any{"username": "reviewer"}},
		"assignees": []any{map[string]any{"username": "assignee"}}, "labels": []string{"bug"},
	}
}

func cutoverAcceptanceProject(id int64, projectPath string) map[string]any {
	return map[string]any{
		"id": id, "path_with_namespace": projectPath,
		"web_url":          fmt.Sprintf("https://gitlab.example.test/%s", projectPath),
		"http_url_to_repo": fmt.Sprintf("https://gitlab.example.test/%s.git", projectPath),
	}
}

func cutoverAcceptanceJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func cutoverAcceptanceHookKey(instance string, projectID, hookID int64) string {
	return fmt.Sprintf("%s:%d:%d", instance, projectID, hookID)
}

func TestGitLabCutoverEndToEndAcceptance(t *testing.T) {
	ctx := context.Background()
	f := newCutoverAcceptanceFixture(t)
	f.remote.mu.Lock()
	f.remote.unsupportedGroupHooks["instance-a"] = true
	f.remote.mu.Unlock()
	seed := testutil.New(f.pool, "", "")
	workspaceA := seed.Workspace(t, "GitLab workspace A", "gitlab-cutover-a-"+uuid.NewString())
	userA := seed.User(t, "Workspace A", "gitlab-cutover-a-"+uuid.NewString()+"@example.test")
	issueA := testutil.New(f.pool, workspaceA, userA).Issue(t, "Issue with GitLab MR")
	workspaceB := seed.Workspace(t, "GitLab workspace B", "gitlab-cutover-b-"+uuid.NewString())
	userB := seed.User(t, "Workspace B", "gitlab-cutover-b-"+uuid.NewString()+"@example.test")

	sources := []*cutoverAcceptanceSource{
		{
			workspaceID: workspaceA, userID: userA, issueID: issueA, instance: "instance-a",
			legacyConnectionID: uuid.NewString(), canonicalConnectionID: uuid.NewString(),
			legacyBindingID: uuid.NewString(), legacyMRID: uuid.NewString(), canonicalProjectID: uuid.NewString(), canonicalMRID: uuid.NewString(),
			projectID: acceptanceProjectID, hookID: 401, token: "test-token-workspace-a-instance-a",
			legacySecret: "legacy-hook-secret-a", unifiedSecret: "canonical-hook-secret-a",
		},
		{
			workspaceID: workspaceA, userID: userA, instance: "instance-b",
			legacyConnectionID: uuid.NewString(), legacyBindingID: uuid.NewString(), legacyMRID: uuid.NewString(),
			projectID: acceptanceProjectID, hookID: 501, token: "test-token-workspace-a-instance-b",
			legacySecret: "legacy-hook-secret-b", unifiedSecret: "legacy-hook-secret-b",
		},
		{
			workspaceID: workspaceB, userID: userB, instance: "instance-a",
			legacyConnectionID: uuid.NewString(), legacyBindingID: uuid.NewString(), legacyMRID: uuid.NewString(),
			projectID: acceptanceProjectID, hookID: 402, token: "test-token-workspace-b-instance-a",
			legacySecret: "legacy-hook-secret-c", unifiedSecret: "legacy-hook-secret-c",
		},
	}
	for _, source := range sources {
		source.baseURL = f.remoteHTTP.URL + "/" + source.instance
		source.path = acceptanceProjectPath
		f.addInstance(source.instance, source.token, "gitlab-user-"+source.instance, map[int64]string{source.projectID: source.path})
		f.addHook(source.instance, source.projectID, cutoverAcceptanceHook{
			ID: source.hookID, URL: cutoverAcceptanceLegacyHookURL(source.legacyConnectionID), Token: source.legacySecret,
			MergeRequestsEvents: true, PipelineEvents: true, JobEvents: source != sources[0], NoteEvents: source != sources[0], EnableSSLVerification: true,
		})
		seedCutoverLegacySource(t, f, source)
	}
	seedCutoverCanonicalOverlap(t, f, sources[0])
	chatWakeupID, issueWakeupID, originalChatCondition := seedCutoverWakeups(t, f, sources[0])

	preflight, err := f.runner.Preflight(ctx)
	if err != nil {
		t.Fatalf("preflight GitLab cutover: %v", err)
	}
	unsupportedGroupHooks := false
	for _, finding := range preflight.Findings {
		if finding.Code == "group_hooks_unsupported" {
			unsupportedGroupHooks = true
		}
	}
	if !unsupportedGroupHooks {
		t.Fatalf("preflight did not report the confirmed unsupported per-instance group Hook capability: %+v", preflight.Findings)
	}
	if preflight.NoOp || preflight.LegacyConnections != 3 || preflight.LegacyProjects != 3 || preflight.LegacyMergeRequests != 3 ||
		preflight.ActiveIssueWakeups != 1 || preflight.ActiveChatWakeups != 1 || preflight.UnifiedConnections != 1 || preflight.UnifiedMergeRequests != 1 {
		t.Fatalf("unexpected preflight inventory: %+v", preflight)
	}
	if err := f.runner.Begin(ctx); err != nil {
		t.Fatalf("begin GitLab maintenance window: %v", err)
	}
	began, err := f.runner.State(ctx)
	if err != nil || began.Phase != "maintenance" || !began.MaintenanceMode || began.Generation != 1 {
		t.Fatalf("begin state = %+v, err=%v", began, err)
	}
	if err := f.runner.Migrate(ctx); err != nil {
		t.Fatalf("migrate legacy and existing VCS data: %v", err)
	}
	migrated, err := f.runner.State(ctx)
	if err != nil || migrated.Phase != "database_migrated" || !migrated.MaintenanceMode || migrated.Generation != began.Generation {
		t.Fatalf("migration state = %+v, err=%v", migrated, err)
	}
	for _, source := range sources {
		source.unifiedConnectionID = cutoverAcceptanceAlias(t, f.pool, source.workspaceID, "connection", source.legacyConnectionID)
		if source.canonicalConnectionID != "" && source.unifiedConnectionID != source.canonicalConnectionID {
			t.Fatalf("legacy connection alias = %q, want pre-existing ID %q", source.unifiedConnectionID, source.canonicalConnectionID)
		}
	}
	canonicalMR := cutoverAcceptanceAlias(t, f.pool, sources[0].workspaceID, "pull_request", sources[0].legacyMRID)
	if canonicalMR != sources[0].canonicalMRID {
		t.Fatalf("legacy MR alias = %q, want pre-existing ID %q", canonicalMR, sources[0].canonicalMRID)
	}
	assertCutoverMigrationRows(t, f, sources, canonicalMR, "canonical newer title", "canonical existing details")
	var preservedCipher string
	if err := f.pool.QueryRow(ctx, `SELECT webhook_secret_encrypted FROM vcs_connection WHERE id=$1`, sources[0].canonicalConnectionID).Scan(&preservedCipher); err != nil {
		t.Fatal(err)
	}
	if preservedCipher != sources[0].canonicalSecretCipher {
		t.Fatal("migration replaced the pre-existing encrypted canonical Hook secret")
	}
	var rollbackCipher string
	if err := f.pool.QueryRow(ctx, `
		SELECT previous_secret_encrypted FROM vcs_gitlab_migration_hook
		WHERE connection_id=$1 AND scope_type='project' AND scope_id=$2 AND legacy_hook_id=$3
	`, sources[0].unifiedConnectionID, strconv.FormatInt(sources[0].projectID, 10), sources[0].hookID).Scan(&rollbackCipher); err != nil {
		t.Fatal(err)
	}
	if old, openErr := openSecret(f.box, rollbackCipher); openErr != nil || old != sources[0].legacySecret {
		t.Fatalf("rollback Hook secret did not retain the legacy credential (decrypt error: %v)", openErr)
	}

	windowSources := make(map[string]*cutoverAcceptanceSource, len(sources))
	for _, source := range sources {
		windowSources[source.unifiedConnectionID] = source
	}
	type windowDeliveryResult struct {
		deliveryID string
		status     int
		err        error
	}
	windowDeliveries := make(chan windowDeliveryResult, len(sources))
	f.remote.mu.Lock()
	f.remote.onHookUpdate = func(before, after cutoverAcceptanceHook) {
		if before.URL == after.URL {
			return
		}
		parsed, err := url.Parse(after.URL)
		if err != nil {
			windowDeliveries <- windowDeliveryResult{err: err}
			return
		}
		connectionID := strings.TrimPrefix(parsed.Path, "/api/webhooks/vcs/")
		source := windowSources[connectionID]
		if source == nil {
			windowDeliveries <- windowDeliveryResult{err: fmt.Errorf("updated Hook URL has unknown connection %q", connectionID)}
			return
		}
		deliveryID := "cutover-window-" + source.legacyConnectionID
		status, err := cutoverAcceptancePostResult(after.URL, after.Token, deliveryID, "Push Hook",
			cutoverAcceptanceEvent("Push Hook", source.projectID, source.path))
		windowDeliveries <- windowDeliveryResult{deliveryID: deliveryID, status: status, err: err}
	}
	f.remote.mu.Unlock()
	since := time.Now().UTC().Add(-time.Hour)
	if err := f.runner.SwitchHooks(ctx); err != nil {
		t.Fatalf("switch managed project Hooks: %v", err)
	}
	if got := len(windowDeliveries); got != len(sources) {
		t.Fatalf("cutover-window delivery count = %d, want %d", got, len(sources))
	}
	for range len(sources) {
		result := <-windowDeliveries
		if result.err != nil || result.status != http.StatusServiceUnavailable {
			t.Fatalf("cutover-window delivery %q response = %d, err=%v; want durable-receipt 503", result.deliveryID, result.status, result.err)
		}
	}
	var windowPushReceipts int64
	if err := f.pool.QueryRow(ctx, `
		SELECT count(*) FROM vcs_gitlab_migration_delivery
		WHERE delivery_id LIKE 'cutover-window-%' AND event_type='Push Hook'
		  AND payload IS NOT NULL AND reconciled_at IS NULL
	`).Scan(&windowPushReceipts); err != nil {
		t.Fatalf("count cutover-window Push Hook receipts: %v", err)
	}
	if windowPushReceipts != int64(len(sources)) {
		t.Fatalf("cutover-window Push Hook receipts = %d, want %d", windowPushReceipts, len(sources))
	}
	pending, err := f.runner.State(ctx)
	if err != nil || pending.Phase != "verification_pending" || !pending.MaintenanceMode {
		t.Fatalf("Hook switch state = %+v, err=%v", pending, err)
	}
	for _, source := range sources {
		hook, ok := f.remoteHook(source.instance, source.projectID, source.hookID)
		if !ok {
			t.Fatalf("managed remote Hook %d disappeared", source.hookID)
		}
		if hook.Token != source.unifiedSecret {
			t.Fatalf("Hook %d token was not rotated to the workspace VCS secret", source.hookID)
		}
		if !hook.MergeRequestsEvents || !hook.PipelineEvents || !hook.JobEvents || !hook.NoteEvents {
			t.Fatalf("Hook %d did not enable all GitLab event types", source.hookID)
		}
		parsed, parseErr := url.Parse(hook.URL)
		if parseErr != nil || parsed.Path != "/api/webhooks/vcs/"+source.unifiedConnectionID ||
			parsed.Query().Get("scope_type") != "project" || parsed.Query().Get("scope_id") != strconv.FormatInt(source.projectID, 10) ||
			parsed.Query().Get("hook_id") != strconv.FormatInt(source.hookID, 10) {
			t.Fatalf("Hook %d target is not its workspace-scoped VCS route: %q (err=%v)", source.hookID, hook.URL, parseErr)
		}
	}
	firstHook, _ := f.remoteHook(sources[0].instance, sources[0].projectID, sources[0].hookID)
	if got := cutoverAcceptancePost(t, firstHook.URL, sources[0].legacySecret, "old-secret-rejected", "Merge Request Hook", cutoverAcceptanceEvent("Merge Request Hook", sources[0].projectID, sources[0].path)); got != http.StatusUnauthorized {
		t.Fatalf("old Hook secret response = %d, want 401", got)
	}
	otherWorkspaceHook, _ := f.remoteHook(sources[2].instance, sources[2].projectID, sources[2].hookID)
	if got := cutoverAcceptancePost(t, otherWorkspaceHook.URL, sources[0].unifiedSecret, "cross-workspace-secret", "Merge Request Hook",
		cutoverAcceptanceEvent("Merge Request Hook", sources[2].projectID, sources[2].path)); got != http.StatusUnauthorized {
		t.Fatalf("other workspace's Hook accepted this workspace's secret: response=%d", got)
	}
	assertCutoverReceiptCounts(t, f.pool, int64(len(sources)), int64(len(sources)), 0)

	if err := f.runner.VerifyHooks(ctx, since); err == nil {
		t.Fatal("verification accepted an unreconciled delivery")
	}
	if err := f.runner.Replay(ctx, since, f.handler.ReplayGitLabMigrationDelivery); err != nil {
		t.Fatalf("replay cutover-window deliveries: %v", err)
	}
	if err := f.runner.VerifyHooks(ctx, since); err == nil {
		t.Fatal("strict verification accepted missing event deliveries")
	}
	if err := f.runner.VerifyConfiguredHooks(ctx, since); err != nil {
		t.Fatalf("verify configured Hooks without additional deliveries: %v", err)
	}
	events := []string{"Merge Request Hook", "Pipeline Hook", "Job Hook", "Note Hook"}
	for _, source := range sources {
		hook, _ := f.remoteHook(source.instance, source.projectID, source.hookID)
		for _, event := range events {
			deliveryID := fmt.Sprintf("acceptance-%s-%s", source.legacyConnectionID, strings.ReplaceAll(event, " ", "-"))
			body := cutoverAcceptanceEvent(event, source.projectID, source.path)
			for range 2 {
				if got := cutoverAcceptancePost(t, hook.URL, source.unifiedSecret, deliveryID, event, body); got != http.StatusServiceUnavailable {
					t.Fatalf("maintenance delivery %s response = %d, want 503 so GitLab retries", deliveryID, got)
				}
			}
		}
	}
	wantReceipts := int64(len(sources) * (len(events) + 1))
	assertCutoverReceiptCounts(t, f.pool, wantReceipts, wantReceipts, int64(len(sources)))
	if err := f.runner.Replay(ctx, since, f.handler.ReplayGitLabMigrationDelivery); err != nil {
		t.Fatalf("replay persisted real webhook receipts: %v", err)
	}
	assertCutoverReceiptCounts(t, f.pool, wantReceipts, wantReceipts, wantReceipts)
	if err := f.runner.VerifyHooks(ctx, since); err != nil {
		t.Fatalf("verify remote Hooks and real receipts: %v", err)
	}
	ready, err := f.runner.State(ctx)
	if err != nil || ready.Phase != "ready" || !ready.MaintenanceMode {
		t.Fatalf("verified state = %+v, err=%v", ready, err)
	}
	assertCutoverWakeups(t, f.pool, chatWakeupID, issueWakeupID, originalChatCondition, sources[0])
	assertCutoverReceiptCounts(t, f.pool, wantReceipts, wantReceipts, wantReceipts)

	// A delivery arriving after first verification is still persisted with 503,
	// then reconciled under maintenance before the final verification pass.
	lateID := "acceptance-late-after-verify"
	lateEvent := "Merge Request Hook"
	if got := cutoverAcceptancePost(t, firstHook.URL, sources[0].unifiedSecret, lateID, lateEvent, cutoverAcceptanceEvent(lateEvent, sources[0].projectID, sources[0].path)); got != http.StatusServiceUnavailable {
		t.Fatalf("late maintenance delivery response = %d, want 503", got)
	}
	wantReceipts++
	assertCutoverReceiptCounts(t, f.pool, wantReceipts, wantReceipts, wantReceipts-1)
	if err := f.runner.Replay(ctx, since, f.handler.ReplayGitLabMigrationDelivery); err != nil {
		t.Fatalf("replay late ready-phase delivery: %v", err)
	}
	if err := f.runner.VerifyHooks(ctx, since); err != nil {
		t.Fatalf("verify after late delivery: %v", err)
	}
	assertCutoverReceiptCounts(t, f.pool, wantReceipts, wantReceipts, wantReceipts)

	if err := f.runner.Finalize(ctx); err != nil {
		t.Fatalf("finalize verified GitLab cutover: %v", err)
	}
	complete, err := f.runner.State(ctx)
	if err != nil || complete.Phase != "complete" || complete.MaintenanceMode {
		t.Fatalf("final state = %+v, err=%v", complete, err)
	}
	if len(complete.RecoveryDeliveryIDs) != 0 {
		t.Fatalf("final state retained reconciled recovery delivery IDs: %v", complete.RecoveryDeliveryIDs)
	}
	assertCutoverReceiptCounts(t, f.pool, wantReceipts, 0, wantReceipts)
	for _, table := range []string{
		"gitlab_connection", "gitlab_project_binding", "gitlab_merge_request", "gitlab_mr_pipeline", "issue_gitlab_merge_request",
	} {
		var exists bool
		if err := f.pool.QueryRow(ctx, `SELECT to_regclass('public.' || $1) IS NOT NULL`, table).Scan(&exists); err != nil {
			t.Fatal(err)
		}
		if exists {
			t.Fatalf("legacy GitLab table %s survived finalization", table)
		}
	}
	assertCutoverMigrationRows(t, f, sources, canonicalMR, "authoritative GitLab title", "authoritative GitLab description")
	assertRetainedGitLabDetails(t, f, sources[0], canonicalMR)
	assertPostFinalizeGitLabDetailQueries(t, f, sources[0], canonicalMR)
	var stillEncrypted string
	if err := f.pool.QueryRow(ctx, `SELECT webhook_secret_encrypted FROM vcs_connection WHERE id=$1`, sources[0].canonicalConnectionID).Scan(&stillEncrypted); err != nil {
		t.Fatal(err)
	}
	if stillEncrypted != sources[0].canonicalSecretCipher {
		t.Fatal("finalization changed the canonical encrypted Hook secret")
	}
	var clearedRollbackSecrets int
	if err := f.pool.QueryRow(ctx, `SELECT count(*) FROM vcs_gitlab_migration_hook WHERE previous_secret_encrypted=''`).Scan(&clearedRollbackSecrets); err != nil {
		t.Fatal(err)
	}
	if clearedRollbackSecrets != len(sources) {
		t.Fatalf("finalization did not clear every rollback secret: cleared %d of %d", clearedRollbackSecrets, len(sources))
	}

	// The same MR delivery retried after cutover uses the normal VCS route and
	// remains idempotent at the shared MR/link boundary.
	if got := cutoverAcceptancePost(t, firstHook.URL, sources[0].unifiedSecret,
		fmt.Sprintf("acceptance-%s-%s", sources[0].legacyConnectionID, strings.ReplaceAll(lateEvent, " ", "-")),
		lateEvent, cutoverAcceptanceEvent(lateEvent, sources[0].projectID, sources[0].path)); got != http.StatusAccepted {
		t.Fatalf("post-finalize GitLab retry response = %d, want 202", got)
	}
	assertCutoverMigrationRows(t, f, sources, canonicalMR, "authoritative GitLab title", "post-finalize detail")
	assertCutoverWakeups(t, f.pool, chatWakeupID, issueWakeupID, originalChatCondition, sources[0])
}

func assertPostFinalizeGitLabDetailQueries(t *testing.T, f *cutoverAcceptanceFixture, source *cutoverAcceptanceSource, pullRequestID string) {
	t.Helper()
	ctx := context.Background()
	queries := db.New(f.pool)
	workspaceID := cutoverAcceptancePGUUID(t, source.workspaceID)
	issueID := cutoverAcceptancePGUUID(t, source.issueID)
	pr, err := queries.GetVCSGitLabPullRequestForIssue(ctx, db.GetVCSGitLabPullRequestForIssueParams{
		WorkspaceID: workspaceID, IssueID: issueID,
		PullRequestID: cutoverAcceptancePGUUID(t, pullRequestID),
	})
	if err != nil {
		t.Fatalf("read canonical GitLab MR after finalization: %v", err)
	}
	initialDetail, err := queries.GetVCSGitLabMRDetail(ctx, db.GetVCSGitLabMRDetailParams{
		WorkspaceID: pr.WorkspaceID, PullRequestID: pr.ID,
	})
	if err != nil || !initialDetail.Description.Valid || initialDetail.Description.String != "authoritative GitLab description" {
		t.Fatalf("read migrated GitLab MR detail before SQLC update = %+v, err=%v", initialDetail, err)
	}

	detail, err := queries.UpsertVCSGitLabMRDetail(ctx, db.UpsertVCSGitLabMRDetailParams{
		WorkspaceID: pr.WorkspaceID, PullRequestID: pr.ID,
		Description:          pgtype.Text{String: "post-finalize detail", Valid: true},
		WakeupSyncGeneration: pr.WakeupSyncGeneration,
	})
	if err != nil {
		t.Fatalf("write canonical GitLab MR detail after finalization: %v", err)
	}
	if !detail.Description.Valid || detail.Description.String != "post-finalize detail" {
		t.Fatalf("post-finalize detail upsert returned %q", detail.Description.String)
	}
	detail, err = queries.GetVCSGitLabMRDetail(ctx, db.GetVCSGitLabMRDetailParams{
		WorkspaceID: pr.WorkspaceID, PullRequestID: pr.ID,
	})
	if err != nil || !detail.Description.Valid || detail.Description.String != "post-finalize detail" {
		t.Fatalf("read post-finalize GitLab MR detail = %+v, err=%v", detail, err)
	}

	approvalParams := db.UpsertGitLabMRApprovalStateByPullRequestParams{
		WorkspaceID: pr.WorkspaceID, PullRequestID: pr.ID,
		WakeupSyncGeneration: pr.WakeupSyncGeneration, Approved: true,
		ApprovedBy: []byte("[]"), Rules: []byte("[]"), RawState: []byte(`{"approved":true}`),
	}
	if _, err := queries.UpsertGitLabMRApprovalStateByPullRequest(ctx, approvalParams); err != nil {
		t.Fatalf("write canonical GitLab approval after finalization: %v", err)
	}
	approval, err := queries.GetGitLabMRApprovalStateByPullRequest(ctx, db.GetGitLabMRApprovalStateByPullRequestParams{
		WorkspaceID: pr.WorkspaceID, PullRequestID: pr.ID,
	})
	if err != nil || !approval.Approved || approval.MergeRequestID.Valid {
		t.Fatalf("read post-finalize GitLab approval = %+v, err=%v", approval, err)
	}

	discussionID := "post-finalize-" + uuid.NewString()
	discussion, err := queries.UpsertGitLabMRDiscussionByPullRequest(ctx, db.UpsertGitLabMRDiscussionByPullRequestParams{
		WorkspaceID: pr.WorkspaceID, PullRequestID: pr.ID,
		WakeupSyncGeneration: pr.WakeupSyncGeneration,
		GitlabDiscussionID:   discussionID, IndividualNote: true,
		Resolved: pgtype.Bool{Bool: true, Valid: true},
	})
	if err != nil || discussion.MergeRequestID.Valid {
		t.Fatalf("write post-finalize GitLab discussion = %+v, err=%v", discussion, err)
	}
	discussions, err := queries.ListGitLabMRDiscussionsByPullRequest(ctx, db.ListGitLabMRDiscussionsByPullRequestParams{
		WorkspaceID: pr.WorkspaceID, PullRequestID: pr.ID,
	})
	if err != nil {
		t.Fatalf("list post-finalize GitLab discussions: %v", err)
	}
	foundDiscussion := false
	for _, row := range discussions {
		if row.GitlabDiscussionID == discussionID && !row.MergeRequestID.Valid {
			foundDiscussion = true
			break
		}
	}
	if !foundDiscussion {
		t.Fatalf("post-finalize GitLab discussion %q was not listed", discussionID)
	}

	noteID := time.Now().UTC().UnixNano()
	note, err := queries.UpsertGitLabMRNoteByPullRequest(ctx, db.UpsertGitLabMRNoteByPullRequestParams{
		WorkspaceID: pr.WorkspaceID, DiscussionID: discussion.ID, PullRequestID: pr.ID,
		WakeupSyncGeneration: pr.WakeupSyncGeneration, GitlabNoteID: noteID,
		Body: "post-finalize discussion note",
	})
	if err != nil || note.MergeRequestID.Valid {
		t.Fatalf("write post-finalize GitLab note = %+v, err=%v", note, err)
	}
	notes, err := queries.ListGitLabMRNotesByPullRequest(ctx, db.ListGitLabMRNotesByPullRequestParams{
		WorkspaceID: pr.WorkspaceID, PullRequestID: pr.ID,
	})
	if err != nil {
		t.Fatalf("list post-finalize GitLab notes: %v", err)
	}
	foundNote := false
	for _, row := range notes {
		if row.GitlabNoteID == noteID && !row.MergeRequestID.Valid {
			foundNote = true
			break
		}
	}
	if !foundNote {
		t.Fatalf("post-finalize GitLab note %d was not listed", noteID)
	}

	jobID := noteID + 1
	job, err := queries.UpsertGitLabPipelineJobByPullRequest(ctx, db.UpsertGitLabPipelineJobByPullRequestParams{
		WorkspaceID: pr.WorkspaceID, PullRequestID: pr.ID,
		WakeupSyncGeneration: pr.WakeupSyncGeneration,
		PipelineID:           acceptancePipelineID, JobID: jobID,
		Name: "post-finalize-job", Status: "failed",
		Sha:               pgtype.Text{String: pr.HeadSha, Valid: true},
		ArtifactsFileName: pgtype.Text{String: "post-finalize.zip", Valid: true},
		ArtifactsFileSize: pgtype.Int8{Int64: 17, Valid: true},
	})
	if err != nil || job.MergeRequestID.Valid {
		t.Fatalf("write post-finalize GitLab job = %+v, err=%v", job, err)
	}
	if _, err := queries.UpdateGitLabPipelineJobTraceSummaryByPullRequest(ctx, db.UpdateGitLabPipelineJobTraceSummaryByPullRequestParams{
		WorkspaceID: pr.WorkspaceID, PullRequestID: pr.ID, JobID: job.ID,
		WakeupSyncGeneration: pr.WakeupSyncGeneration,
		TraceSummary:         pgtype.Text{String: "post-finalize failure summary", Valid: true},
		TraceTruncated:       true,
	}); err != nil {
		t.Fatalf("write post-finalize GitLab job trace: %v", err)
	}
	readJob, err := queries.GetGitLabVCSJobForIssue(ctx, db.GetGitLabVCSJobForIssueParams{
		WorkspaceID: pr.WorkspaceID, IssueID: issueID, JobID: job.ID,
	})
	if err != nil || readJob.TraceSummary.String != "post-finalize failure summary" ||
		readJob.ArtifactsFileName.String != "post-finalize.zip" {
		t.Fatalf("read post-finalize GitLab job = %+v, err=%v", readJob, err)
	}
	jobRows, err := queries.ListGitLabPipelineJobsByPullRequest(ctx, db.ListGitLabPipelineJobsByPullRequestParams{
		WorkspaceID: pr.WorkspaceID, PullRequestID: pr.ID,
	})
	if err != nil {
		t.Fatalf("list post-finalize GitLab jobs: %v", err)
	}
	foundJob := false
	for _, row := range jobRows {
		if row.JobID == jobID && row.TraceSummary.String == "post-finalize failure summary" &&
			row.ArtifactsFileName.String == "post-finalize.zip" && !row.MergeRequestID.Valid {
			foundJob = true
			break
		}
	}
	if !foundJob {
		t.Fatalf("post-finalize GitLab job %d was not listed with trace and artifact metadata", jobID)
	}
	issueMRs, err := queries.ListGitLabVCSMergeRequestsForIssue(ctx, db.ListGitLabVCSMergeRequestsForIssueParams{
		WorkspaceID: pr.WorkspaceID, IssueID: issueID,
	})
	if err != nil {
		t.Fatalf("list post-finalize GitLab MRs for issue: %v", err)
	}
	foundMR := false
	for _, row := range issueMRs {
		if row.ID == pr.ID {
			foundMR = true
			break
		}
	}
	if !foundMR {
		t.Fatalf("canonical GitLab MR %s was not listed after finalization", pullRequestID)
	}
}

func assertRetainedGitLabDetails(t *testing.T, f *cutoverAcceptanceFixture, source *cutoverAcceptanceSource, pullRequestID string) {
	t.Helper()
	ctx := context.Background()
	tables := []string{"gitlab_mr_approval_state", "gitlab_mr_discussion", "gitlab_mr_note", "gitlab_pipeline_job"}
	for _, table := range tables {
		var exists bool
		if err := f.pool.QueryRow(ctx, `
			SELECT to_regclass(format('%I.%I',current_schema(),$1::text)) IS NOT NULL
		`, table).Scan(&exists); err != nil {
			t.Fatal(err)
		}
		if !exists {
			t.Fatalf("finalization deleted retained GitLab detail table %s", table)
		}
		var canonicalRequired, legacyNullable bool
		if err := f.pool.QueryRow(ctx, `
			SELECT
				COALESCE(bool_or(column_name='vcs_pull_request_id' AND is_nullable='NO'),false),
				COALESCE(bool_or(column_name='merge_request_id' AND is_nullable='YES'),false)
			FROM information_schema.columns
			WHERE table_schema=current_schema() AND table_name=$1
		`, table).Scan(&canonicalRequired, &legacyNullable); err != nil {
			t.Fatal(err)
		}
		if !canonicalRequired || !legacyNullable {
			t.Fatalf("detail table %s does not require canonical identity while retaining a nullable legacy column", table)
		}
		var rows, canonicalRows, legacyReferences int64
		query := fmt.Sprintf(`
			SELECT count(*),
			       count(*) FILTER (WHERE vcs_pull_request_id IS NOT NULL),
			       count(*) FILTER (WHERE merge_request_id IS NOT NULL)
			FROM %s WHERE workspace_id=$1
		`, table)
		if err := f.pool.QueryRow(ctx, query, source.workspaceID).Scan(&rows, &canonicalRows, &legacyReferences); err != nil {
			t.Fatal(err)
		}
		if rows == 0 || canonicalRows != rows || legacyReferences != 0 {
			t.Fatalf("detail table %s identity rows=%d canonical=%d legacy references=%d", table, rows, canonicalRows, legacyReferences)
		}
		var legacyForeignKeys int64
		if err := f.pool.QueryRow(ctx, `
			SELECT count(*) FROM pg_constraint
			WHERE contype='f'
			  AND conrelid=to_regclass(format('%I.%I',current_schema(),$1::text))
			  AND confrelid=to_regclass(format('%I.%I',current_schema(),'gitlab_merge_request'))
		`, table).Scan(&legacyForeignKeys); err != nil {
			t.Fatal(err)
		}
		if legacyForeignKeys != 0 {
			t.Fatalf("detail table %s still depends on the legacy MR table", table)
		}
	}

	var canonicalApprovalPrimaryKey bool
	if err := f.pool.QueryRow(ctx, `
		SELECT EXISTS (
			SELECT 1
			FROM pg_constraint pk
			JOIN pg_attribute a ON a.attrelid=pk.conrelid
			WHERE pk.conrelid=to_regclass(format('%I.%I',current_schema(),'gitlab_mr_approval_state'))
			  AND pk.contype='p'
			  AND a.attname='vcs_pull_request_id'
			  AND pk.conkey=ARRAY[a.attnum]::smallint[]
		)
	`).Scan(&canonicalApprovalPrimaryKey); err != nil {
		t.Fatal(err)
	}
	if !canonicalApprovalPrimaryKey {
		t.Fatal("GitLab approval state does not use the canonical VCS pull-request ID as its primary key")
	}

	var approved bool
	if err := f.pool.QueryRow(ctx, `
		SELECT approved FROM gitlab_mr_approval_state
		WHERE workspace_id=$1 AND vcs_pull_request_id=$2
	`, source.workspaceID, pullRequestID).Scan(&approved); err != nil {
		t.Fatal(err)
	}
	if !approved {
		t.Fatal("legacy GitLab approval detail was not preserved")
	}
	var resolved bool
	if err := f.pool.QueryRow(ctx, `
		SELECT resolved FROM gitlab_mr_discussion
		WHERE workspace_id=$1 AND vcs_pull_request_id=$2 AND gitlab_discussion_id='acceptance-legacy-discussion'
	`, source.workspaceID, pullRequestID).Scan(&resolved); err != nil {
		t.Fatal(err)
	}
	if resolved {
		t.Fatal("legacy GitLab discussion detail changed during cutover")
	}
	var noteBody string
	if err := f.pool.QueryRow(ctx, `
		SELECT body FROM gitlab_mr_note
		WHERE workspace_id=$1 AND vcs_pull_request_id=$2 AND gitlab_note_id=902
	`, source.workspaceID, pullRequestID).Scan(&noteBody); err != nil {
		t.Fatal(err)
	}
	if noteBody != "legacy note body" {
		t.Fatalf("legacy GitLab note body = %q, want preserved value", noteBody)
	}
	var jobName, jobStatus string
	if err := f.pool.QueryRow(ctx, `
		SELECT name,status FROM gitlab_pipeline_job
		WHERE workspace_id=$1 AND vcs_pull_request_id=$2 AND job_id=904
	`, source.workspaceID, pullRequestID).Scan(&jobName, &jobStatus); err != nil {
		t.Fatal(err)
	}
	if jobName != "legacy-job" || jobStatus != "failed" {
		t.Fatalf("legacy GitLab job detail changed: name=%q status=%q", jobName, jobStatus)
	}
}

func cutoverAcceptancePGUUID(t *testing.T, value string) pgtype.UUID {
	t.Helper()
	parsed, err := uuid.Parse(value)
	if err != nil {
		t.Fatalf("parse fixture UUID %q: %v", value, err)
	}
	return pgtype.UUID{Bytes: [16]byte(parsed), Valid: true}
}

func TestGitLabCutoverRestoreAfterRemoteHookFailure(t *testing.T) {
	ctx := context.Background()
	f := newCutoverAcceptanceFixture(t)
	seed := testutil.New(f.pool, "", "")
	workspace := seed.Workspace(t, "Restored workspace", "gitlab-restore-"+uuid.NewString())
	user := seed.User(t, "Restore owner", "gitlab-restore-"+uuid.NewString()+"@example.test")
	otherWorkspace := seed.Workspace(t, "Unaffected workspace", "gitlab-untouched-"+uuid.NewString())
	otherUser := seed.User(t, "Unaffected owner", "gitlab-untouched-"+uuid.NewString()+"@example.test")

	legacyConnectionID, canonicalConnectionID := uuid.NewString(), uuid.NewString()
	token, oldSecret, newSecret := "restore-test-token", "restore-old-hook-secret", "restore-new-hook-secret"
	baseURL := f.remoteHTTP.URL + "/restore-instance"
	f.addInstance("restore-instance", token, "restore-user", map[int64]string{
		77: "group/sub/first", 78: "group/sub/second",
	})
	legacyCipher, err := sealSecret(f.box, token)
	if err != nil {
		t.Fatal(err)
	}
	oldSecretCipher, err := sealSecret(f.box, oldSecret)
	if err != nil {
		t.Fatal(err)
	}
	newTokenCipher, err := sealSecret(f.box, token)
	if err != nil {
		t.Fatal(err)
	}
	newSecretCipher, err := sealSecret(f.box, newSecret)
	if err != nil {
		t.Fatal(err)
	}
	at := f.at
	if _, err := f.pool.Exec(ctx, `
		INSERT INTO gitlab_connection
			(id,workspace_id,base_url,host,connected_by_id,access_token_encrypted,webhook_secret_encrypted,created_at,updated_at)
		VALUES ($1,$2,$3,'gitlab.example.test',$4,$5,$6,$7,$7)
	`, legacyConnectionID, workspace, baseURL, user, legacyCipher, oldSecretCipher, at); err != nil {
		t.Fatalf("insert legacy restore connection: %v", err)
	}
	if _, err := f.pool.Exec(ctx, `
		INSERT INTO vcs_connection
			(id,workspace_id,provider,instance_url,account_login,access_token_encrypted,webhook_secret_encrypted,connected_by_id,created_at,updated_at)
		VALUES ($1,$2,'gitlab',$3,'restore-user',$4,$5,$6,$7,$7)
	`, canonicalConnectionID, workspace, baseURL, newTokenCipher, newSecretCipher, user, at); err != nil {
		t.Fatalf("insert canonical restore connection: %v", err)
	}
	legacyHooks := []cutoverAcceptanceHook{
		{ID: 811, URL: cutoverAcceptanceLegacyHookURL(legacyConnectionID), Token: oldSecret, MergeRequestsEvents: false, PipelineEvents: true, JobEvents: false, NoteEvents: true, EnableSSLVerification: true},
		{ID: 812, URL: cutoverAcceptanceLegacyHookURL(legacyConnectionID), Token: oldSecret, MergeRequestsEvents: true, EnableSSLVerification: true},
	}
	for i, projectID := range []int64{77, 78} {
		bindingID := uuid.NewString()
		hook := legacyHooks[i]
		f.addHook("restore-instance", projectID, hook)
		if _, err := f.pool.Exec(ctx, `
			INSERT INTO gitlab_project_binding (
				id,workspace_id,connection_id,gitlab_project_id,path_with_namespace,web_url,hook_id,hook_enabled,created_at,updated_at
			) VALUES ($1,$2,$3,$4,$5,$6,$7,true,$8,$8)
		`, bindingID, workspace, legacyConnectionID, projectID, []string{"group/sub/first", "group/sub/second"}[i],
			"https://gitlab.example.test/"+[]string{"group/sub/first", "group/sub/second"}[i], hook.ID, at); err != nil {
			t.Fatalf("insert legacy restore project %d: %v", projectID, err)
		}
	}
	unaffectedConnectionID, unaffectedProjectID, unaffectedMRID := uuid.NewString(), uuid.NewString(), uuid.NewString()
	unaffectedTokenCipher, err := sealSecret(f.box, "untouched-workspace-token")
	if err != nil {
		t.Fatal(err)
	}
	unaffectedSecretCipher, err := sealSecret(f.box, "untouched-workspace-secret")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.pool.Exec(ctx, `
		INSERT INTO vcs_connection
			(id,workspace_id,provider,instance_url,account_login,access_token_encrypted,webhook_secret_encrypted,connected_by_id,created_at,updated_at)
		VALUES ($1,$2,'gitlab',$3,'untouched-user',$4,$5,$6,$7,$7)
	`, unaffectedConnectionID, otherWorkspace, f.remoteHTTP.URL+"/untouched-instance", unaffectedTokenCipher, unaffectedSecretCipher, otherUser, at); err != nil {
		t.Fatalf("insert unaffected workspace VCS connection: %v", err)
	}
	if _, err := f.pool.Exec(ctx, `
		INSERT INTO vcs_gitlab_project (
			id,workspace_id,connection_id,gitlab_project_id,path_with_namespace,web_url,hook_enabled,legacy_link_policy,created_at,updated_at
		) VALUES ($1,$2,$3,900,'untouched/project','https://gitlab.example.test/untouched/project',true,false,$4,$4)
	`, unaffectedProjectID, otherWorkspace, unaffectedConnectionID, at); err != nil {
		t.Fatalf("insert unaffected workspace project: %v", err)
	}
	if _, err := f.pool.Exec(ctx, `
		INSERT INTO vcs_pull_request (
			id,workspace_id,connection_id,provider,repo_owner,repo_name,pr_number,title,state,html_url,
			branch,head_sha,author_login,merged_at,closed_at,pr_created_at,pr_updated_at,
			additions,deletions,changed_files,has_conflicts,conflict_head_sha,created_at,updated_at
		) VALUES ($1,$2,$3,'gitlab','untouched','project',9,'keep unchanged','open',
			'https://gitlab.example.test/untouched/project/-/merge_requests/9','keep','untouched-head','owner',
			NULL,NULL,$4,$4,0,0,0,false,'',$4,$4)
	`, unaffectedMRID, otherWorkspace, unaffectedConnectionID, at); err != nil {
		t.Fatalf("insert unaffected workspace MR: %v", err)
	}
	f.failNextHookUpdate("restore-instance", 78, 812)
	preflight, err := f.runner.Preflight(ctx)
	if err != nil {
		t.Fatalf("preflight restore fixture: %v", err)
	}
	if preflight.LegacyConnections != 1 || preflight.LegacyProjects != 2 {
		t.Fatalf("unexpected restore fixture inventory: %+v", preflight)
	}
	if err := f.runner.Begin(ctx); err != nil {
		t.Fatalf("begin restore fixture maintenance: %v", err)
	}
	if err := f.runner.Migrate(ctx); err != nil {
		t.Fatalf("migrate restore fixture: %v", err)
	}
	if err := f.runner.SwitchHooks(ctx); err == nil {
		t.Fatal("switch-hooks succeeded despite an external GitLab Hook update failure")
	}
	failed, err := f.runner.State(ctx)
	if err != nil || !failed.MaintenanceMode || (failed.Phase != "hooks_pending" && failed.Phase != "verification_pending") {
		t.Fatalf("Hook failure did not retain maintenance: %+v, err=%v", failed, err)
	}
	first, ok := f.remoteHook("restore-instance", 77, 811)
	if !ok || first.URL == legacyHooks[0].URL || first.Token != newSecret ||
		!first.MergeRequestsEvents || !first.PipelineEvents || !first.JobEvents || !first.NoteEvents {
		t.Fatalf("first Hook did not enable required events: found=%v hook=%+v", ok, first)
	}
	second, ok := f.remoteHook("restore-instance", 78, 812)
	if !ok || second.URL != legacyHooks[1].URL || second.Token != oldSecret {
		t.Fatalf("failing Hook unexpectedly changed remotely: found=%v url_restored=%v credential_restored=%v", ok, second.URL == legacyHooks[1].URL, second.Token == oldSecret)
	}
	firstURL, err := url.Parse(first.URL)
	if err != nil {
		t.Fatal(err)
	}
	firstConnection := strings.TrimPrefix(firstURL.Path, "/api/webhooks/vcs/")
	if firstConnection != canonicalConnectionID {
		t.Fatalf("partial Hook targets connection %q, want %q", firstConnection, canonicalConnectionID)
	}
	pendingBody := cutoverAcceptanceEvent("Merge Request Hook", 77, "group/sub/first")
	if got := cutoverAcceptancePost(t, first.URL, newSecret, "restore-pending-delivery", "Merge Request Hook", pendingBody); got != http.StatusServiceUnavailable {
		t.Fatalf("failed-cutover ingress response = %d, want durable-receipt 503", got)
	}
	assertCutoverReceiptCounts(t, f.pool, 1, 1, 0)
	var legacyTokenBefore, legacySecretBefore, canonicalTokenBefore, canonicalSecretBefore string
	if err := f.pool.QueryRow(ctx, `SELECT access_token_encrypted,webhook_secret_encrypted FROM gitlab_connection WHERE id=$1`, legacyConnectionID).Scan(&legacyTokenBefore, &legacySecretBefore); err != nil {
		t.Fatal(err)
	}
	if err := f.pool.QueryRow(ctx, `SELECT access_token_encrypted,webhook_secret_encrypted FROM vcs_connection WHERE id=$1`, canonicalConnectionID).Scan(&canonicalTokenBefore, &canonicalSecretBefore); err != nil {
		t.Fatal(err)
	}
	if legacyTokenBefore != legacyCipher || legacySecretBefore != oldSecretCipher || canonicalTokenBefore != newTokenCipher || canonicalSecretBefore != newSecretCipher {
		t.Fatal("migration changed original encrypted connection credentials before restore")
	}

	if err := f.runner.Restore(ctx); err != nil {
		t.Fatalf("restore remote Hooks and database after external failure: %v", err)
	}
	restored, err := f.runner.State(ctx)
	if err != nil || restored.Phase != "restored" || !restored.MaintenanceMode {
		t.Fatalf("restore state = %+v, err=%v", restored, err)
	}
	for i, projectID := range []int64{77, 78} {
		got, ok := f.remoteHook("restore-instance", projectID, legacyHooks[i].ID)
		if !ok || got != legacyHooks[i] {
			t.Fatalf("Hook %d was not restored exactly (found=%v)", legacyHooks[i].ID, ok)
		}
	}
	assertCutoverReceiptCounts(t, f.pool, 1, 1, 0)
	var restoredLegacyToken, restoredLegacySecret, restoredCanonicalToken, restoredCanonicalSecret string
	if err := f.pool.QueryRow(ctx, `SELECT access_token_encrypted,webhook_secret_encrypted FROM gitlab_connection WHERE id=$1`, legacyConnectionID).Scan(&restoredLegacyToken, &restoredLegacySecret); err != nil {
		t.Fatal(err)
	}
	if err := f.pool.QueryRow(ctx, `SELECT access_token_encrypted,webhook_secret_encrypted FROM vcs_connection WHERE id=$1`, canonicalConnectionID).Scan(&restoredCanonicalToken, &restoredCanonicalSecret); err != nil {
		t.Fatal(err)
	}
	if restoredLegacyToken != legacyCipher || restoredLegacySecret != oldSecretCipher || restoredCanonicalToken != newTokenCipher || restoredCanonicalSecret != newSecretCipher {
		t.Fatal("restore failed to preserve the original legacy and canonical encrypted keys")
	}
	var migratedProjects, migrationAliases int
	if err := f.pool.QueryRow(ctx, `SELECT count(*) FROM vcs_gitlab_project WHERE workspace_id=$1`, workspace).Scan(&migratedProjects); err != nil {
		t.Fatal(err)
	}
	if err := f.pool.QueryRow(ctx, `SELECT count(*) FROM vcs_gitlab_migration_id_map WHERE workspace_id=$1`, workspace).Scan(&migrationAliases); err != nil {
		t.Fatal(err)
	}
	if migratedProjects != 0 || migrationAliases != 0 {
		t.Fatalf("restore left migrated workspace rows: projects=%d aliases=%d", migratedProjects, migrationAliases)
	}
	var untouchedTitle, untouchedConnectionURL string
	if err := f.pool.QueryRow(ctx, `SELECT title FROM vcs_pull_request WHERE id=$1`, unaffectedMRID).Scan(&untouchedTitle); err != nil {
		t.Fatal(err)
	}
	if err := f.pool.QueryRow(ctx, `SELECT instance_url FROM vcs_connection WHERE id=$1`, unaffectedConnectionID).Scan(&untouchedConnectionURL); err != nil {
		t.Fatal(err)
	}
	if untouchedTitle != "keep unchanged" || untouchedConnectionURL != f.remoteHTTP.URL+"/untouched-instance" {
		t.Fatalf("restore changed another workspace's canonical rows: title=%q URL=%q", untouchedTitle, untouchedConnectionURL)
	}
	var pendingDeliveryID string
	if err := f.pool.QueryRow(ctx, `
		SELECT delivery_id FROM vcs_gitlab_migration_delivery
		WHERE delivery_id='restore-pending-delivery' AND reconciled_at IS NULL AND payload IS NOT NULL
	`).Scan(&pendingDeliveryID); err != nil || pendingDeliveryID != "restore-pending-delivery" {
		t.Fatalf("restore discarded or falsely reconciled the 503 delivery evidence: id=%q err=%v", pendingDeliveryID, err)
	}
	restored, err = f.runner.State(ctx)
	if err != nil || len(restored.RecoveryDeliveryIDs) != 1 || restored.RecoveryDeliveryIDs[0] != pendingDeliveryID {
		t.Fatalf("restored status did not report its pending delivery ID: IDs=%v err=%v", restored.RecoveryDeliveryIDs, err)
	}
}

func seedCutoverLegacySource(t *testing.T, f *cutoverAcceptanceFixture, source *cutoverAcceptanceSource) {
	t.Helper()
	var err error
	source.legacyTokenCipher, err = sealSecret(f.box, source.token)
	if err != nil {
		t.Fatal(err)
	}
	source.legacySecretCipher, err = sealSecret(f.box, source.legacySecret)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.pool.Exec(context.Background(), `
		INSERT INTO gitlab_connection
			(id,workspace_id,base_url,host,connected_by_id,access_token_encrypted,webhook_secret_encrypted,created_at,updated_at)
		VALUES ($1,$2,$3,'gitlab.example.test',$4,$5,$6,$7,$7)
	`, source.legacyConnectionID, source.workspaceID, source.baseURL, source.userID,
		source.legacyTokenCipher, source.legacySecretCipher, f.at); err != nil {
		t.Fatalf("insert legacy GitLab connection: %v", err)
	}
	if _, err := f.pool.Exec(context.Background(), `
		INSERT INTO gitlab_project_binding (
			id,workspace_id,connection_id,gitlab_project_id,path_with_namespace,web_url,hook_id,hook_enabled,created_at,updated_at
		) VALUES ($1,$2,$3,$4,$5,$6,$7,true,$8,$8)
	`, source.legacyBindingID, source.workspaceID, source.legacyConnectionID, source.projectID, source.path,
		"https://gitlab.example.test/"+source.path, source.hookID, f.at); err != nil {
		t.Fatalf("insert legacy project binding: %v", err)
	}
	mrID := source.legacyMRID
	if _, err := f.pool.Exec(context.Background(), `
		INSERT INTO gitlab_merge_request (
			id,workspace_id,project_binding_id,gitlab_project_id,mr_iid,title,description,state,web_url,
			source_branch,target_branch,author_username,sha,merge_commit_sha,detailed_merge_status,has_conflicts,
			additions,deletions,changed_files,mr_created_at,mr_updated_at,created_at,updated_at,
			reviewers,assignees,labels,last_refreshed_at,wakeup_sync_generation
		) VALUES ($1,$2,$3,$4,$5,'legacy older title','legacy migration detail','merged',
			'https://gitlab.example.test/'||$6||'/-/merge_requests/81','feature','main','legacy-author','head-77',
			'legacy-merge-sha','mergeable',false,5,2,1,$7,$8,$7,$8,
			'["legacy-reviewer"]'::jsonb,'["legacy-assignee"]'::jsonb,'["legacy-label"]'::jsonb,$8,3)
	`, mrID, source.workspaceID, source.legacyBindingID, source.projectID, acceptanceMRIID, source.path,
		f.at.Add(-time.Hour), f.at.Add(-30*time.Minute)); err != nil {
		t.Fatalf("insert legacy GitLab MR: %v", err)
	}
}

func seedCutoverCanonicalOverlap(t *testing.T, f *cutoverAcceptanceFixture, source *cutoverAcceptanceSource) {
	t.Helper()
	var err error
	source.canonicalTokenCipher, err = sealSecret(f.box, source.token)
	if err != nil {
		t.Fatal(err)
	}
	source.canonicalSecretCipher, err = sealSecret(f.box, source.unifiedSecret)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.pool.Exec(context.Background(), `
		INSERT INTO vcs_connection (
			id,workspace_id,provider,instance_url,account_login,access_token_encrypted,webhook_secret_encrypted,connected_by_id,created_at,updated_at
		) VALUES ($1,$2,'gitlab',$3,'canonical-user',$4,$5,$6,$7,$7)
	`, source.canonicalConnectionID, source.workspaceID, source.baseURL, source.canonicalTokenCipher,
		source.canonicalSecretCipher, source.userID, f.at); err != nil {
		t.Fatalf("insert existing canonical GitLab connection: %v", err)
	}
	if _, err := f.pool.Exec(context.Background(), `
		INSERT INTO vcs_gitlab_project (
			id,workspace_id,connection_id,gitlab_project_id,path_with_namespace,web_url,hook_id,hook_enabled,legacy_link_policy,created_at,updated_at
		) VALUES ($1,$2,$3,$4,$5,$6,$7,true,false,$8,$8)
	`, source.canonicalProjectID, source.workspaceID, source.canonicalConnectionID, source.projectID, source.path,
		"https://gitlab.example.test/"+source.path, source.hookID, f.at); err != nil {
		t.Fatalf("insert existing canonical GitLab project: %v", err)
	}
	if _, err := f.pool.Exec(context.Background(), `
		INSERT INTO vcs_pull_request (
			id,workspace_id,connection_id,provider,repo_owner,repo_name,pr_number,title,state,html_url,
			branch,head_sha,author_login,pr_created_at,pr_updated_at,additions,deletions,changed_files,
			has_conflicts,conflict_head_sha,created_at,updated_at
		) VALUES ($1,$2,$3,'gitlab','group/sub','project',$4,'canonical newer title','open',
			'https://gitlab.example.test/group/sub/project/-/merge_requests/81','feature','head-77','canonical-author',
			$5,$6,11,4,2,false,'',$5,$6)
	`, source.canonicalMRID, source.workspaceID, source.canonicalConnectionID, acceptanceMRIID,
		f.at.Add(-time.Hour), f.at.Add(-10*time.Minute)); err != nil {
		t.Fatalf("insert existing canonical GitLab MR: %v", err)
	}
	if _, err := f.pool.Exec(context.Background(), `
		INSERT INTO vcs_gitlab_mr_detail (
			vcs_pull_request_id,description,merge_commit_sha,detailed_merge_status,target_branch,reviewers,assignees,labels,
			last_refreshed_at,last_refresh_error,conflict_sha,pipeline_sync_at,pipeline_sync_error,wakeup_sync_generation,created_at,updated_at
		) VALUES ($1,'canonical existing details','canonical-merge-sha','mergeable','main',
			'["canonical-reviewer"]'::jsonb,'["canonical-assignee"]'::jsonb,'["canonical-label"]'::jsonb,
			$2,'','',$2,'',1,$2,$3)
	`, source.canonicalMRID, f.at.Add(-20*time.Minute), f.at.Add(-10*time.Minute)); err != nil {
		t.Fatalf("insert canonical GitLab MR detail: %v", err)
	}
	if _, err := f.pool.Exec(context.Background(), `
		INSERT INTO issue_vcs_pull_request (
			issue_id,pull_request_id,close_intent,linked_by_type,linked_by_id,linked_at,vcs_linked
		) VALUES ($1,$2,false,'member',$3,$4,true)
	`, source.issueID, source.canonicalMRID, source.userID, f.at); err != nil {
		t.Fatalf("insert pre-existing VCS issue/MR link: %v", err)
	}
	if _, err := f.pool.Exec(context.Background(), `
		INSERT INTO issue_gitlab_merge_request (
			workspace_id,issue_id,merge_request_id,close_intent,linked_by_type,linked_by_id,linked_at
		) VALUES ($1,$2,$3,true,NULL,NULL,$4)
	`, source.workspaceID, source.issueID, source.legacyMRID, f.at.Add(time.Minute)); err != nil {
		t.Fatalf("insert legacy GitLab close-intent link: %v", err)
	}
	if _, err := f.pool.Exec(context.Background(), `
		INSERT INTO gitlab_mr_pipeline (
			merge_request_id,pipeline_id,pipeline_project_id,sha,ref,status,web_url,pipeline_updated_at,created_at,updated_at
		) VALUES ($1,901,$2,'head-77','feature','failed','https://gitlab.example.test/pipelines/901',$3,$3,$3)
	`, source.legacyMRID, source.projectID, f.at.Add(-5*time.Minute)); err != nil {
		t.Fatalf("insert legacy pipeline baseline: %v", err)
	}
	if _, err := f.pool.Exec(context.Background(), `
		INSERT INTO gitlab_mr_approval_state (merge_request_id,workspace_id,approved,approved_by,rules,updated_at)
		VALUES ($1,$2,true,'["legacy-reviewer"]'::jsonb,'[]'::jsonb,$3)
	`, source.legacyMRID, source.workspaceID, f.at); err != nil {
		t.Fatalf("insert legacy approval state: %v", err)
	}
	discussionID, noteID, jobID := uuid.NewString(), uuid.NewString(), uuid.NewString()
	if _, err := f.pool.Exec(context.Background(), `
		INSERT INTO gitlab_mr_discussion (id,workspace_id,merge_request_id,gitlab_discussion_id,resolved)
		VALUES ($1,$2,$3,'acceptance-legacy-discussion',false)
	`, discussionID, source.workspaceID, source.legacyMRID); err != nil {
		t.Fatalf("insert legacy MR discussion: %v", err)
	}
	if _, err := f.pool.Exec(context.Background(), `
		INSERT INTO gitlab_mr_note (id,workspace_id,discussion_id,merge_request_id,gitlab_note_id,body)
		VALUES ($1,$2,$3,$4,902,'legacy note body')
	`, noteID, source.workspaceID, discussionID, source.legacyMRID); err != nil {
		t.Fatalf("insert legacy MR note: %v", err)
	}
	if _, err := f.pool.Exec(context.Background(), `
		INSERT INTO gitlab_pipeline_job (id,workspace_id,merge_request_id,pipeline_id,job_id,name,status)
		VALUES ($1,$2,$3,901,904,'legacy-job','failed')
	`, jobID, source.workspaceID, source.legacyMRID); err != nil {
		t.Fatalf("insert legacy pipeline job: %v", err)
	}
}

func seedCutoverWakeups(t *testing.T, f *cutoverAcceptanceFixture, source *cutoverAcceptanceSource) (string, string, []byte) {
	t.Helper()
	chatID, issueID := uuid.NewString(), uuid.NewString()
	condition := map[string]any{
		"type": "pull_request", "repo": source.path, "number": acceptanceMRIID, "event": "checks_finished",
		"provider": "gitlab", "connection_id": source.legacyConnectionID,
		"gitlab": map[string]any{
			"source": "gitlab_connection", "merge_request_id": source.legacyMRID,
			"activated_at": f.at.Format(time.RFC3339), "baseline_available": true, "baseline_head_sha": "head-77",
			"baseline_pipelines": []any{map[string]any{
				"pipeline_id": "901", "pipeline_project_id": "77", "pipeline_project_path": source.path,
				"ref": "feature", "sha": "head-77", "status": "failed", "updated_at": f.at.Add(-5 * time.Minute).Format(time.RFC3339),
			}},
		},
	}
	original, err := json.Marshal(condition)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.pool.Exec(context.Background(), `
		INSERT INTO chat_wakeup (
			id,workspace_id,chat_session_id,agent_id,requester_id,title,instruction,kind,condition,status,next_eval_at,expires_at
		) VALUES ($1,$2,$3,$4,$5,'Wait for MR','resume after CI','condition',$6::jsonb,'active',$7,$8)
	`, chatID, source.workspaceID, uuid.NewString(), uuid.NewString(), source.userID,
		string(original), f.at, f.at.Add(24*time.Hour)); err != nil {
		t.Fatalf("insert active GitLab chat wakeup: %v", err)
	}
	issueCondition := []byte(`{"type":"pull_request","event":"checks_finished"}`)
	if _, err := f.pool.Exec(context.Background(), `
		INSERT INTO issue_wakeup (
			id,workspace_id,issue_id,agent_id,created_by,instruction,kind,mode,condition,condition_state,enabled
		) VALUES ($1,$2,$3,$4,$5,'wake on linked MR','event','once',$6::jsonb,'legacy-ci-baseline',true)
	`, issueID, source.workspaceID, source.issueID, uuid.NewString(), source.userID, string(issueCondition)); err != nil {
		t.Fatalf("insert active GitLab issue wakeup: %v", err)
	}
	return chatID, issueID, original
}

func assertCutoverMigrationRows(t *testing.T, f *cutoverAcceptanceFixture, sources []*cutoverAcceptanceSource, canonicalMR, wantTitle, wantDescription string) {
	t.Helper()
	var sameIdentity, canonicalDuplicate, projectCount int
	if err := f.pool.QueryRow(context.Background(), `
		SELECT count(*) FROM vcs_pull_request
		WHERE provider='gitlab' AND repo_owner='group/sub' AND repo_name='project' AND pr_number=81
	`).Scan(&sameIdentity); err != nil {
		t.Fatal(err)
	}
	if err := f.pool.QueryRow(context.Background(), `SELECT count(*) FROM vcs_pull_request WHERE id=$1`, canonicalMR).Scan(&canonicalDuplicate); err != nil {
		t.Fatal(err)
	}
	if err := f.pool.QueryRow(context.Background(), `
		SELECT count(*) FROM vcs_gitlab_project WHERE path_with_namespace=$1 AND gitlab_project_id=$2
	`, acceptanceProjectPath, acceptanceProjectID).Scan(&projectCount); err != nil {
		t.Fatal(err)
	}
	if sameIdentity != 3 || canonicalDuplicate != 1 || projectCount != 3 {
		t.Fatalf("same-path MR/project identity was merged or duplicated: MRs=%d canonical=%d projects=%d", sameIdentity, canonicalDuplicate, projectCount)
	}
	for _, source := range sources {
		mappedConnection := cutoverAcceptanceAlias(t, f.pool, source.workspaceID, "connection", source.legacyConnectionID)
		if mappedConnection != source.unifiedConnectionID {
			t.Fatalf("workspace-scoped legacy connection alias = %q, want %q", mappedConnection, source.unifiedConnectionID)
		}
		var encryptedToken, encryptedSecret string
		if err := f.pool.QueryRow(context.Background(), `
			SELECT access_token_encrypted,webhook_secret_encrypted FROM vcs_connection WHERE id=$1 AND workspace_id=$2
		`, source.unifiedConnectionID, source.workspaceID).Scan(&encryptedToken, &encryptedSecret); err != nil {
			t.Fatal(err)
		}
		token, tokenErr := openSecret(f.box, encryptedToken)
		secret, secretErr := openSecret(f.box, encryptedSecret)
		if tokenErr != nil || secretErr != nil || token != source.token || secret != source.unifiedSecret {
			t.Fatalf("workspace-scoped GitLab credentials were not preserved/encrypted: workspace=%s connection=%s", source.workspaceID, source.unifiedConnectionID)
		}
		mappedMR := cutoverAcceptanceAlias(t, f.pool, source.workspaceID, "pull_request", source.legacyMRID)
		if source == sources[0] {
			if mappedMR != canonicalMR {
				t.Fatalf("overlap MR alias = %q, want canonical ID %q", mappedMR, canonicalMR)
			}
		}
		var connectionMRCount int
		if err := f.pool.QueryRow(context.Background(), `
			SELECT count(*) FROM vcs_pull_request
			WHERE workspace_id=$1 AND connection_id=$2 AND provider='gitlab'
			  AND repo_owner='group/sub' AND repo_name='project' AND pr_number=81
		`, source.workspaceID, source.unifiedConnectionID).Scan(&connectionMRCount); err != nil {
			t.Fatal(err)
		}
		if connectionMRCount != 1 {
			t.Fatalf("workspace %s connection %s MR count = %d, want 1", source.workspaceID, source.unifiedConnectionID, connectionMRCount)
		}
	}
	var title, state, description string
	if err := f.pool.QueryRow(context.Background(), `
		SELECT pr.title,pr.state,detail.description
		FROM vcs_pull_request pr JOIN vcs_gitlab_mr_detail detail ON detail.vcs_pull_request_id=pr.id
		WHERE pr.id=$1
	`, canonicalMR).Scan(&title, &state, &description); err != nil {
		t.Fatal(err)
	}
	if title != wantTitle || state != "open" || description != wantDescription {
		t.Fatalf("canonical MR/detail mismatch: title=%q state=%q description=%q want title=%q description=%q", title, state, description, wantTitle, wantDescription)
	}
	var pipelineCount int
	if err := f.pool.QueryRow(context.Background(), `
		SELECT count(*) FROM vcs_gitlab_pipeline WHERE pull_request_id=$1 AND pipeline_id=$2
	`, canonicalMR, acceptancePipelineID).Scan(&pipelineCount); err != nil {
		t.Fatal(err)
	}
	if pipelineCount != 1 {
		t.Fatalf("replayed current pipeline rows = %d, want one deduplicated pipeline", pipelineCount)
	}
	var linkCount int
	var closeIntent, vcsLinked, gitlabCloseIntent bool
	if err := f.pool.QueryRow(context.Background(), `
		SELECT count(*),bool_or(close_intent),bool_or(vcs_linked),bool_or(gitlab_close_intent)
		FROM issue_vcs_pull_request WHERE issue_id=$1 AND pull_request_id=$2
	`, sources[0].issueID, canonicalMR).Scan(&linkCount, &closeIntent, &vcsLinked, &gitlabCloseIntent); err != nil {
		t.Fatal(err)
	}
	if linkCount != 1 || closeIntent || !vcsLinked || !gitlabCloseIntent {
		t.Fatalf("dual-source issue link/close intent was lost or duplicated: rows=%d generic-close=%v vcs=%v gitlab-close=%v", linkCount, closeIntent, vcsLinked, gitlabCloseIntent)
	}
}

func assertCutoverWakeups(t *testing.T, pool *pgxpool.Pool, chatID, issueID string, original []byte, source *cutoverAcceptanceSource) {
	t.Helper()
	var status string
	var raw []byte
	if err := pool.QueryRow(context.Background(), `SELECT status,condition FROM chat_wakeup WHERE id=$1`, chatID).Scan(&status, &raw); err != nil {
		t.Fatal(err)
	}
	var chat map[string]any
	if err := json.Unmarshal(raw, &chat); err != nil {
		t.Fatal(err)
	}
	gitlabState, ok := chat["gitlab"].(map[string]any)
	if !ok || status != "active" || chat["connection_id"] != source.unifiedConnectionID ||
		gitlabState["source"] != "vcs_connection" || gitlabState["merge_request_id"] != source.canonicalMRID ||
		gitlabState["baseline_head_sha"] != "head-77" || gitlabState["baseline_available"] != true {
		t.Fatalf("chat wake target/baseline changed or fired: status=%q condition=%s", status, raw)
	}
	var originalJSON, migratedJSON map[string]any
	if err := json.Unmarshal(original, &originalJSON); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(raw, &migratedJSON); err != nil {
		t.Fatal(err)
	}
	wantPipelines := originalJSON["gitlab"].(map[string]any)["baseline_pipelines"]
	gotPipelines := gitlabState["baseline_pipelines"]
	wantBytes, _ := json.Marshal(wantPipelines)
	gotBytes, _ := json.Marshal(gotPipelines)
	if string(wantBytes) != string(gotBytes) {
		t.Fatalf("chat wake pipeline baseline changed: got %s want %s", gotBytes, wantBytes)
	}
	var enabled bool
	var issueState string
	if err := pool.QueryRow(context.Background(), `SELECT enabled,condition_state FROM issue_wakeup WHERE id=$1`, issueID).Scan(&enabled, &issueState); err != nil {
		t.Fatal(err)
	}
	if !enabled || issueState != "legacy-ci-baseline" {
		t.Fatalf("issue wake was disabled or baseline changed: enabled=%v state=%q", enabled, issueState)
	}
}

func assertCutoverReceiptCounts(t *testing.T, pool *pgxpool.Pool, wantRows, wantPayloads, wantReconciled int64) {
	t.Helper()
	var rows, payloads, reconciled int64
	if err := pool.QueryRow(context.Background(), `
		SELECT count(*),count(*) FILTER (WHERE payload IS NOT NULL),count(*) FILTER (WHERE reconciled_at IS NOT NULL)
		FROM vcs_gitlab_migration_delivery
	`).Scan(&rows, &payloads, &reconciled); err != nil {
		t.Fatal(err)
	}
	if rows != wantRows || payloads != wantPayloads || reconciled != wantReconciled {
		t.Fatalf("GitLab delivery ledger counts rows/payload/reconciled=%d/%d/%d, want %d/%d/%d", rows, payloads, reconciled, wantRows, wantPayloads, wantReconciled)
	}
}

func cutoverAcceptanceAlias(t *testing.T, pool *pgxpool.Pool, workspace, kind, legacyID string) string {
	t.Helper()
	var unified string
	if err := pool.QueryRow(context.Background(), `
		SELECT unified_id::text FROM vcs_gitlab_migration_id_map
		WHERE workspace_id=$1 AND entity_kind=$2 AND legacy_id=$3
	`, workspace, kind, legacyID).Scan(&unified); err != nil {
		t.Fatalf("resolve workspace-scoped %s alias %s: %v", kind, legacyID, err)
	}
	return unified
}

func cutoverAcceptanceLegacyHookURL(connectionID string) string {
	return "https://legacy-gitlab.example.test/api/webhooks/gitlab/" + connectionID
}

func cutoverAcceptanceEvent(event string, projectID int64, projectPath string) []byte {
	project := map[string]any{"id": projectID, "path_with_namespace": projectPath}
	base := map[string]any{"project": project}
	switch event {
	case "Merge Request Hook":
		base["object_kind"] = "merge_request"
		base["user"] = map[string]any{"username": "webhook-user"}
		base["object_attributes"] = map[string]any{
			"iid": acceptanceMRIID, "title": "event title", "description": "event description", "state": "opened", "action": "update",
			"source_branch": "feature", "target_branch": "main", "url": "https://gitlab.example.test/group/sub/project/-/merge_requests/81",
			"created_at": "2026-11-23T09:00:00Z", "updated_at": "2026-11-23T10:00:00Z", "last_commit": map[string]any{"id": "head-77"},
		}
	case "Pipeline Hook":
		base["object_kind"] = "pipeline"
		base["object_attributes"] = map[string]any{
			"id": acceptancePipelineID, "sha": "head-77", "ref": "feature", "status": "failed",
			"url": "https://gitlab.example.test/pipelines/901", "created_at": "2026-11-23T09:55:00Z",
			"updated_at": "2026-11-23T09:58:00Z", "finished_at": "2026-11-23T09:58:00Z",
		}
	case "Job Hook":
		base["object_kind"] = "build"
		base["object_attributes"] = map[string]any{
			"id": acceptanceJobID, "name": "unit-tests", "stage": "test", "status": "failed", "sha": "head-77", "ref": "feature",
			"started_at": "2026-11-23T09:56:00Z", "finished_at": "2026-11-23T09:58:00Z",
		}
		base["pipeline"] = map[string]any{"id": acceptancePipelineID, "sha": "head-77", "ref": "feature"}
	case "Note Hook":
		base["object_kind"] = "note"
		base["merge_request"] = map[string]any{"iid": acceptanceMRIID}
		base["object_attributes"] = map[string]any{
			"id": int64(9901), "note": "new review comment", "created_at": "2026-11-23T09:59:00Z", "updated_at": "2026-11-23T09:59:00Z",
		}
	case "Push Hook":
		base["object_kind"] = "push"
		base["ref"] = "refs/heads/main"
		base["before"] = "previous-head"
		base["after"] = "head-77"
		base["commits"] = []any{}
	}
	encoded, _ := json.Marshal(base)
	return encoded
}

func cutoverAcceptancePost(t *testing.T, targetURL, secret, deliveryID, event string, body []byte) int {
	t.Helper()
	status, err := cutoverAcceptancePostResult(targetURL, secret, deliveryID, event, body)
	if err != nil {
		t.Fatalf("send real GitLab webhook request: %v", err)
	}
	return status
}

func cutoverAcceptancePostResult(targetURL, secret, deliveryID, event string, body []byte) (int, error) {
	request, err := http.NewRequest(http.MethodPost, targetURL, bytes.NewReader(body))
	if err != nil {
		return 0, err
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-Gitlab-Token", secret)
	request.Header.Set("X-Gitlab-Event", event)
	request.Header.Set("X-Gitlab-Webhook-UUID", deliveryID)
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		return 0, err
	}
	_, _ = io.Copy(io.Discard, response.Body)
	_ = response.Body.Close()
	return response.StatusCode, nil
}

func installCodeMlampTLSRoute(t *testing.T, server *httptest.Server) {
	t.Helper()
	original, ok := http.DefaultTransport.(*http.Transport)
	if !ok {
		t.Fatal("default HTTP transport is not an *http.Transport")
	}
	serverTransport, ok := server.Client().Transport.(*http.Transport)
	if !ok || serverTransport.TLSClientConfig == nil {
		t.Fatal("test GitLab TLS transport is unavailable")
	}
	transport := original.Clone()
	transport.TLSClientConfig = &tls.Config{
		RootCAs:    serverTransport.TLSClientConfig.RootCAs,
		ServerName: "127.0.0.1",
	}
	transport.DialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
		if address == "code.mlamp.cn:443" {
			address = server.Listener.Addr().String()
		}
		return (&net.Dialer{}).DialContext(ctx, network, address)
	}
	previous := http.DefaultTransport
	http.DefaultTransport = transport
	t.Cleanup(func() {
		http.DefaultTransport = previous
		transport.CloseIdleConnections()
	})
}

func TestSystemHookInventoryAttestationIsExactAndNarrow(t *testing.T) {
	const origin = "https://gitlab.example.test"
	attestation := &SystemHookInventoryAttestation{InstanceURL: origin}
	tests := []struct {
		name        string
		instance    string
		status      int
		attestation *SystemHookInventoryAttestation
		want        bool
	}{
		{name: "exact origin 403", instance: origin, status: http.StatusForbidden, attestation: attestation, want: true},
		{name: "no confirmation", instance: origin, status: http.StatusForbidden},
		{name: "different instance", instance: "https://gitlab-other.example.test", status: http.StatusForbidden, attestation: attestation},
		{name: "subdomain is a different instance", instance: "https://sub.gitlab.example.test", status: http.StatusForbidden, attestation: attestation},
		{name: "non-default port is a different instance", instance: origin + ":8443", status: http.StatusForbidden, attestation: attestation},
		{name: "explicit default port is distinct", instance: origin + ":443", status: http.StatusForbidden, attestation: attestation},
		{name: "path is not an origin match", instance: origin + "/gitlab", status: http.StatusForbidden, attestation: attestation},
		{name: "401 remains blocked", instance: origin, status: http.StatusUnauthorized, attestation: attestation},
		{name: "503 remains blocked", instance: origin, status: http.StatusServiceUnavailable, attestation: attestation},
		{name: "network failure remains blocked", instance: origin, attestation: attestation},
		{name: "mismatched audit origin remains blocked", instance: origin, status: http.StatusForbidden, attestation: &SystemHookInventoryAttestation{InstanceURL: "https://gitlab-other.example.test"}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var err error
			if test.status == 0 {
				err = fmt.Errorf("simulated network failure")
			} else {
				err = &gitlabAPIError{statusCode: test.status}
			}
			if got := shouldSkipSystemHookInventory(test.instance, err, test.attestation); got != test.want {
				t.Fatalf("shouldSkipSystemHookInventory(%q, %d) = %t, want %t", test.instance, test.status, got, test.want)
			}
		})
	}
}

func TestCanonicalSystemHookInventoryAttestationOrigin(t *testing.T) {
	valid := []string{
		"https://gitlab.example.test",
		"https://gitlab.example.test:8443",
		"https://[2001:db8::1]:8443",
	}
	for _, origin := range valid {
		t.Run(origin, func(t *testing.T) {
			got, err := CanonicalSystemHookInventoryAttestationOrigin(origin)
			if err != nil || got != origin {
				t.Fatalf("CanonicalSystemHookInventoryAttestationOrigin(%q) = %q, %v", origin, got, err)
			}
		})
	}
	for _, invalid := range []string{
		"gitlab.example.test",
		"http://gitlab.example.test",
		"https://gitlab.example.test/",
		"https://gitlab.example.test/group",
		"https://user@gitlab.example.test",
		"https://gitlab.example.test?admin=true",
		"https://gitlab.example.test#fragment",
		"https://gitlab.example.test:",
	} {
		t.Run(invalid, func(t *testing.T) {
			if got, err := CanonicalSystemHookInventoryAttestationOrigin(invalid); err == nil {
				t.Fatalf("CanonicalSystemHookInventoryAttestationOrigin(%q) = %q, want error", invalid, got)
			}
		})
	}
}

func TestSystemHookInventoryAttestationPersistsAcrossCutoverRetries(t *testing.T) {
	ctx := context.Background()
	f := newCutoverAcceptanceFixture(t)
	tlsServer := httptest.NewTLSServer(f.remote)
	t.Cleanup(tlsServer.Close)
	installCodeMlampTLSRoute(t, tlsServer)
	f.remote.mu.Lock()
	f.remote.systemHookInventoryStatus["code.mlamp.cn"] = http.StatusForbidden
	f.remote.mu.Unlock()

	seed := testutil.New(f.pool, "", "")
	workspaceID := seed.Workspace(t, "Code GitLab workspace", "gitlab-attestation-"+uuid.NewString())
	userID := seed.User(t, "Code GitLab owner", "gitlab-attestation-"+uuid.NewString()+"@example.test")
	source := &cutoverAcceptanceSource{
		workspaceID: workspaceID, userID: userID, instance: "code.mlamp.cn",
		legacyConnectionID: uuid.NewString(), legacyBindingID: uuid.NewString(), legacyMRID: uuid.NewString(),
		projectID: acceptanceProjectID, hookID: 401, token: "test-token-code-attestation",
		legacySecret: "legacy-hook-secret-code-attestation",
		baseURL:      "https://code.mlamp.cn", path: acceptanceProjectPath,
	}
	f.addInstance(source.instance, source.token, "code-owner", map[int64]string{source.projectID: source.path})
	oldHookURL := cutoverAcceptanceLegacyHookURL(source.legacyConnectionID)
	f.addHook(source.instance, source.projectID, cutoverAcceptanceHook{
		ID: source.hookID, URL: oldHookURL, Token: source.legacySecret,
		MergeRequestsEvents: true, PipelineEvents: true, JobEvents: true, NoteEvents: true, EnableSSLVerification: true,
	})
	seedCutoverLegacySource(t, f, source)

	blocked, err := f.runner.Preflight(ctx)
	if err == nil {
		t.Fatal("System Hook inventory 403 unexpectedly passed without operator attestation")
	}
	if len(blocked.SystemHookInventoryAttestations) != 0 {
		t.Fatalf("blocked preflight reported an attestation that was not supplied: %+v", blocked.SystemHookInventoryAttestations)
	}
	if err := f.runner.SetSystemHookInventoryAttestation(source.baseURL, "test-operator"); err != nil {
		t.Fatal(err)
	}
	preflight, err := f.runner.Preflight(ctx)
	if err != nil {
		t.Fatalf("preflight with exact operator attestation: %v", err)
	}
	if len(preflight.SystemHookInventoryAttestations) != 1 ||
		preflight.SystemHookInventoryAttestations[0].InstanceURL != source.baseURL ||
		preflight.SystemHookInventoryAttestations[0].Source != "operator_input" {
		t.Fatalf("preflight did not report the exact operator attestation: %+v", preflight.SystemHookInventoryAttestations)
	}
	attestedFinding := false
	for _, finding := range preflight.Findings {
		if finding.Code == "system_hook_inventory_attested" {
			attestedFinding = true
		}
	}
	if !attestedFinding {
		t.Fatalf("preflight did not report the narrowly skipped System Hook inventory: %+v", preflight.Findings)
	}
	if err := f.runner.Begin(ctx); err != nil {
		t.Fatalf("begin cutover with attestation: %v", err)
	}
	state, err := f.runner.State(ctx)
	if err != nil || len(state.SystemHookInventoryAttestations) != 1 ||
		state.SystemHookInventoryAttestations[0].Generation != 1 ||
		state.SystemHookInventoryAttestations[0].Operator != "test-operator" {
		t.Fatalf("attestation audit state = %+v, err=%v", state, err)
	}

	// A new CLI process has no in-memory confirmation; retries use the durable,
	// generation-bound audit record rather than requiring global permission bypass.
	f.runner.systemHookAttestation = nil
	f.remote.mu.Lock()
	f.remote.systemHookInventoryStatus["code.mlamp.cn"] = http.StatusUnauthorized
	f.remote.mu.Unlock()
	if _, err := f.runner.Preflight(ctx); err == nil {
		t.Fatal("System Hook inventory 401 unexpectedly passed with a persisted attestation")
	}
	f.remote.mu.Lock()
	f.remote.systemHookInventoryStatus["code.mlamp.cn"] = http.StatusForbidden
	f.remote.mu.Unlock()
	retryPreflight, err := f.runner.Preflight(ctx)
	if err != nil {
		t.Fatalf("retry preflight did not reuse the persisted exact-instance attestation: %v", err)
	}
	if len(retryPreflight.SystemHookInventoryAttestations) != 1 || retryPreflight.SystemHookInventoryAttestations[0].Source != "persisted" {
		t.Fatalf("retry preflight did not audit the persisted attestation: %+v", retryPreflight.SystemHookInventoryAttestations)
	}

	if err := f.runner.Migrate(ctx); err != nil {
		t.Fatalf("migrate GitLab data: %v", err)
	}
	if err := f.runner.SwitchHooks(ctx); err != nil {
		t.Fatalf("switch project Hook while System Hook inventory is attested: %v", err)
	}
	switched, err := f.runner.State(ctx)
	if err != nil || !switched.MaintenanceMode {
		t.Fatalf("Hook switch state = %+v, err=%v", switched, err)
	}
	remoteHook, ok := f.remoteHook(source.instance, source.projectID, source.hookID)
	if !ok || remoteHook.URL == oldHookURL || !strings.Contains(remoteHook.URL, "/api/webhooks/vcs/") {
		t.Fatalf("managed project Hook was lost or not switched to the workspace VCS route: hook=%+v found=%t", remoteHook, ok)
	}
	if !remoteHook.MergeRequestsEvents || !remoteHook.PipelineEvents || !remoteHook.JobEvents || !remoteHook.NoteEvents {
		t.Fatalf("project Hook event settings were not preserved: %+v", remoteHook)
	}
	connectionID := cutoverAcceptanceAlias(t, f.pool, source.workspaceID, "connection", source.legacyConnectionID)
	var previousURL, rollbackCipher string
	var mergeRequestsEvents, pipelineEvents, jobEvents, noteEvents bool
	if err := f.pool.QueryRow(ctx, `
		SELECT previous_config->>'url',
		       (previous_config->>'merge_requests_events')::boolean,
		       (previous_config->>'pipeline_events')::boolean,
		       (previous_config->>'job_events')::boolean,
		       (previous_config->>'note_events')::boolean,
		       previous_secret_encrypted
		FROM vcs_gitlab_migration_hook
		WHERE connection_id=$1 AND scope_type='project' AND scope_id=$2 AND legacy_hook_id=$3
	`, connectionID, strconv.FormatInt(source.projectID, 10), source.hookID).Scan(
		&previousURL, &mergeRequestsEvents, &pipelineEvents, &jobEvents, &noteEvents, &rollbackCipher,
	); err != nil {
		t.Fatalf("read retained project Hook recovery record: %v", err)
	}
	if previousURL != oldHookURL || !mergeRequestsEvents || !pipelineEvents || !jobEvents || !noteEvents {
		t.Fatalf("attestation bypass lost the old recoverable Hook configuration: url=%q events=%t/%t/%t/%t", previousURL, mergeRequestsEvents, pipelineEvents, jobEvents, noteEvents)
	}
	if secret, err := openSecret(f.box, rollbackCipher); err != nil || secret != source.legacySecret {
		t.Fatalf("attestation bypass lost the old recoverable Hook secret: secret match=%t err=%v", secret == source.legacySecret, err)
	}
}
