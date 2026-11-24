package handler

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/middleware"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

func TestNormalizeGitLabGroupInput(t *testing.T) {
	const instanceURL = "https://gitlab.example/root"
	for _, test := range []struct {
		name  string
		input string
		want  string
		ok    bool
	}{
		{name: "namespace path", input: "engineering/platform", want: "engineering/platform", ok: true},
		{name: "same instance URL with base path", input: "https://gitlab.example/root/engineering/platform", want: "engineering/platform", ok: true},
		{name: "GitLab group projects URL", input: "https://gitlab.example/root/groups/engineering/platform/-/projects", want: "engineering/platform", ok: true},
		{name: "uppercase URL scheme and host", input: "HTTPS://GITLAB.EXAMPLE/root/engineering/platform", want: "engineering/platform", ok: true},
		{name: "empty URL query", input: "https://gitlab.example/root/engineering/platform?"},
		{name: "empty URL fragment", input: "https://gitlab.example/root/engineering/platform#"},
		{name: "wrong instance", input: "https://other.example/root/engineering/platform"},
		{name: "missing connection base path", input: "https://gitlab.example/engineering/platform"},
		{name: "base path prefix boundary", input: "https://gitlab.example/rooted/engineering/platform"},
		{name: "path traversal", input: "engineering/../platform"},
		{name: "query in path", input: "engineering/platform?include_subgroups=false"},
	} {
		t.Run(test.name, func(t *testing.T) {
			got, ok := normalizeGitLabGroupInput(test.input, instanceURL)
			if got != test.want || ok != test.ok {
				t.Fatalf("normalizeGitLabGroupInput(%q) = (%q, %v), want (%q, %v)", test.input, got, ok, test.want, test.ok)
			}
		})
	}
}

func TestListVCSGitLabGroupProjectsRoleAndConnectionBoundaries(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("handler test fixture not initialized (no DB?)")
	}
	ctx := context.Background()
	const slug = "gitlab-group-projects-role-boundaries"
	_, _ = testPool.Exec(ctx, `DELETE FROM workspace WHERE slug = $1`, slug)
	var workspaceID string
	if err := testPool.QueryRow(ctx, `
INSERT INTO workspace (name, slug, description, issue_prefix)
VALUES ($1, $2, $3, $4)
RETURNING id
`, "GitLab Group Projects", slug, "gitlab group projects", "GLGP").Scan(&workspaceID); err != nil {
		t.Fatalf("create workspace: %v", err)
	}

	workspaceIDs := []string{workspaceID}
	userIDs := make([]string, 0, 4)
	connectionIDs := make([]string, 0, 2)
	t.Cleanup(func() {
		for _, id := range connectionIDs {
			_, _ = testPool.Exec(context.Background(), `DELETE FROM vcs_connection WHERE id = $1`, id)
		}
		for _, id := range workspaceIDs {
			_, _ = testPool.Exec(context.Background(), `DELETE FROM workspace WHERE id = $1`, id)
		}
		for _, id := range userIDs {
			_, _ = testPool.Exec(context.Background(), `DELETE FROM "user" WHERE id = $1`, id)
		}
	})

	mkUser := func(label string) string {
		t.Helper()
		var id string
		email := fmt.Sprintf("gitlab-group-projects-%s-%s@multica.ai", slug, label)
		if err := testPool.QueryRow(ctx, `INSERT INTO "user" (name, email) VALUES ($1, $2) RETURNING id`, "GLGP "+label, email).Scan(&id); err != nil {
			t.Fatalf("create %s user: %v", label, err)
		}
		userIDs = append(userIDs, id)
		return id
	}
	ownerID := mkUser("owner")
	adminID := mkUser("admin")
	memberID := mkUser("member")
	outsiderID := mkUser("outsider")
	for _, member := range []struct{ userID, role string }{
		{ownerID, "owner"},
		{adminID, "admin"},
		{memberID, "member"},
	} {
		if _, err := testPool.Exec(ctx, `INSERT INTO member (workspace_id, user_id, role) VALUES ($1, $2, $3)`, workspaceID, member.userID, member.role); err != nil {
			t.Fatalf("insert %s membership: %v", member.role, err)
		}
	}

	var requests atomic.Int32
	gitlabServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		if got, want := r.URL.EscapedPath(), "/gitlab-root/api/v4/groups/engineering%2Fplatform/projects"; got != want {
			t.Errorf("upstream path = %q, want %q", got, want)
		}
		if got := r.Header.Get("PRIVATE-TOKEN"); got != "group-project-token" {
			t.Errorf("upstream token = %q", got)
		}
		if r.URL.Query().Get("include_subgroups") != "true" || r.URL.Query().Get("per_page") != "100" {
			t.Errorf("unexpected upstream query: %s", r.URL.RawQuery)
		}
		switch r.URL.Query().Get("page") {
		case "1":
			w.Header().Set("X-Next-Page", "2")
			_, _ = fmt.Fprint(w, `[{"id":101,"path_with_namespace":"engineering/platform/project-a","web_url":"https://gitlab.example/root/engineering/platform/project-a"}]`)
		case "2":
			_, _ = fmt.Fprint(w, `[{"id":102,"path_with_namespace":"engineering/platform/subgroup/project-b","web_url":"https://gitlab.example/root/engineering/platform/subgroup/project-b"}]`)
		default:
			t.Errorf("unexpected upstream page %q", r.URL.Query().Get("page"))
			http.Error(w, "unexpected page", http.StatusBadRequest)
		}
	}))
	defer gitlabServer.Close()
	t.Setenv("GITLAB_PROXY_URL", "")
	_ = withVCSBox(t)
	createConnection := func(workspaceID, instanceURL string) string {
		t.Helper()
		token, err := testHandler.sealVCSSecret("group-project-token")
		if err != nil {
			t.Fatalf("seal access token: %v", err)
		}
		webhookSecret, err := testHandler.sealVCSSecret("group-project-webhook-secret")
		if err != nil {
			t.Fatalf("seal webhook secret: %v", err)
		}
		connection, err := testHandler.Queries.UpsertVCSConnection(ctx, db.UpsertVCSConnectionParams{
			WorkspaceID:            parseUUID(workspaceID),
			Provider:               "gitlab",
			InstanceUrl:            instanceURL,
			AccountLogin:           "group-project-test",
			AccessTokenEncrypted:   token,
			WebhookSecretEncrypted: webhookSecret,
			ConnectedByID:          pgtype.UUID{},
		})
		if err != nil {
			t.Fatalf("seed GitLab connection: %v", err)
		}
		id := uuidToString(connection.ID)
		connectionIDs = append(connectionIDs, id)
		return id
	}
	connectionID := createConnection(workspaceID, gitlabServer.URL+"/gitlab-root")

	otherSlug := slug + "-other"
	var otherWorkspaceID string
	if err := testPool.QueryRow(ctx, `
INSERT INTO workspace (name, slug, description, issue_prefix)
VALUES ($1, $2, $3, $4)
RETURNING id
`, "GitLab Group Projects Other", otherSlug, "other GitLab group workspace", "GLGO").Scan(&otherWorkspaceID); err != nil {
		t.Fatalf("create other workspace: %v", err)
	}
	workspaceIDs = append(workspaceIDs, otherWorkspaceID)
	otherConnectionID := createConnection(otherWorkspaceID, gitlabServer.URL+"/gitlab-root")

	router := chi.NewRouter()
	router.Route("/api/workspaces/{id}", func(r chi.Router) {
		r.Group(func(r chi.Router) {
			r.Use(middleware.RequireWorkspaceRoleFromURL(testHandler.Queries, "id", "owner", "admin"))
			r.Get("/vcs/gitlab/groups/projects", testHandler.ListVCSGitLabGroupProjects)
		})
	})
	query := url.Values{
		"connection_id": {connectionID},
		"group":         {gitlabServer.URL + "/gitlab-root/engineering/platform"},
	}
	path := "/api/workspaces/" + workspaceID + "/vcs/gitlab/groups/projects?" + query.Encode()
	exercise := func(userID, requestPath string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, requestPath, nil)
		req.Header.Set("X-User-ID", userID)
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)
		return rec
	}

	for _, userID := range []string{ownerID, adminID} {
		rec := exercise(userID, path)
		if rec.Code != http.StatusOK {
			t.Fatalf("authorized GET: want 200, got %d (%s)", rec.Code, rec.Body.String())
		}
		var body struct {
			Projects []gitLabGroupProjectResponse `json:"projects"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatalf("decode response: %v", err)
		}
		if len(body.Projects) != 2 || body.Projects[0].ID != 101 || body.Projects[1].ID != 102 ||
			body.Projects[1].PathWithNamespace != "engineering/platform/subgroup/project-b" {
			t.Fatalf("unexpected projects: %#v", body.Projects)
		}
		if strings.Contains(rec.Body.String(), "group-project-token") || strings.Contains(rec.Body.String(), "group-project-webhook-secret") {
			t.Fatalf("response disclosed GitLab credentials: %s", rec.Body.String())
		}
	}
	if got := exercise(memberID, path).Code; got != http.StatusForbidden {
		t.Fatalf("member GET: want 403, got %d", got)
	}
	if got := exercise(outsiderID, path).Code; got != http.StatusNotFound {
		t.Fatalf("outsider GET: want 404, got %d", got)
	}
	otherConnectionQuery := url.Values{
		"connection_id": {otherConnectionID},
		"group":         {"engineering/platform"},
	}
	otherConnectionPath := "/api/workspaces/" + workspaceID + "/vcs/gitlab/groups/projects?" + otherConnectionQuery.Encode()
	if got := exercise(adminID, otherConnectionPath).Code; got != http.StatusNotFound {
		t.Fatalf("connection from another workspace GET: want 404, got %d", got)
	}
	if got := requests.Load(); got != 4 {
		t.Fatalf("upstream request count = %d, want 4 (two pages for each authorized request)", got)
	}
}
