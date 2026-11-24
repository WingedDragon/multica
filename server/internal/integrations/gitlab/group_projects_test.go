package gitlab

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestListGroupProjectsEncodesNamespaceAndFollowsSubgroupPages(t *testing.T) {
	var requests int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		if got, want := r.URL.EscapedPath(), "/gitlab-root/api/v4/groups/team%2Fplatform%2Fsubgroup/projects"; got != want {
			t.Errorf("request path = %q, want %q", got, want)
		}
		if got := r.Header.Get("PRIVATE-TOKEN"); got != "group-project-token" {
			t.Errorf("PRIVATE-TOKEN = %q", got)
		}
		if got := r.URL.Query().Get("include_subgroups"); got != "true" {
			t.Errorf("include_subgroups = %q, want true", got)
		}
		if got := r.URL.Query().Get("with_shared"); got != "false" {
			t.Errorf("with_shared = %q, want false", got)
		}
		if got := r.URL.Query().Get("per_page"); got != "100" {
			t.Errorf("per_page = %q, want 100", got)
		}

		switch r.URL.Query().Get("page") {
		case "1":
			w.Header().Set("X-Next-Page", "3")
			_, _ = fmt.Fprint(w, `[{"id":1,"path_with_namespace":"team/platform/project-a","web_url":"https://gitlab.invalid/team/platform/project-a"}]`)
		case "3":
			_, _ = fmt.Fprint(w, `[{"id":2,"path_with_namespace":"team/platform/subgroup/project-b","web_url":"https://gitlab.invalid/team/platform/subgroup/project-b"}]`)
		default:
			t.Errorf("unexpected page %q", r.URL.Query().Get("page"))
			http.Error(w, "unexpected page", http.StatusBadRequest)
		}
	}))
	defer server.Close()

	client, err := NewClient(Config{BaseURL: server.URL + "/gitlab-root", Token: "group-project-token"})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	projects, err := client.ListGroupProjects(context.Background(), "team/platform/subgroup")
	if err != nil {
		t.Fatalf("ListGroupProjects: %v", err)
	}
	if requests != 2 {
		t.Fatalf("request count = %d, want 2", requests)
	}
	if len(projects) != 2 {
		t.Fatalf("project count = %d, want 2", len(projects))
	}
	if projects[0].PathWithNamespace != "team/platform/project-a" || projects[1].PathWithNamespace != "team/platform/subgroup/project-b" {
		t.Fatalf("unexpected projects: %#v", projects)
	}
}

func TestListGroupProjectsRejectsInvalidNextPage(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Next-Page", "1")
		_, _ = fmt.Fprint(w, `[]`)
	}))
	defer server.Close()

	client, err := NewClient(Config{BaseURL: server.URL, Token: "token"})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	if _, err := client.ListGroupProjects(context.Background(), "group"); err == nil {
		t.Fatal("expected invalid pagination error")
	}
}
