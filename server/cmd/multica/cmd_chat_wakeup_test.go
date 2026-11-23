package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"github.com/multica-ai/multica/server/internal/cli"
	"github.com/multica-ai/multica/server/internal/handler"
)

func chatWakeupConditionForFlags(t *testing.T, untilPR, connection string) (map[string]any, error) {
	t.Helper()
	wake := newChatWakeupCommand()
	var create *cobra.Command
	for _, command := range wake.Commands() {
		if command.Name() == "create" {
			create = command
			break
		}
	}
	if create == nil {
		t.Fatal("chat wakeup create command missing")
	}
	if untilPR != "" {
		if err := create.Flags().Set("until-pr", untilPR); err != nil {
			return nil, err
		}
	}
	if connection != "" {
		if err := create.Flags().Set("gitlab-connection", connection); err != nil {
			return nil, err
		}
	}
	return chatWakeupConditionFromFlags(context.Background(), create, nil)
}

func TestChatWakeupCLIGitHubPRConditionKeepsLegacyShape(t *testing.T) {
	got, err := chatWakeupConditionForFlags(t, "octo/widgets#17:checks", "")
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]any{"type": "pull_request", "repo": "octo/widgets", "number": 17, "event": "checks_finished"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("GitHub condition = %#v, want %#v", got, want)
	}
}

func TestChatWakeupCLIGitLabPRConditionUsesExactConnectionAndFullPath(t *testing.T) {
	const connection = "44b522ca-5fe6-4ea4-b686-263946524f14"
	got, err := chatWakeupConditionForFlags(t, "group/subgroup/project#17:closed", connection)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]any{
		"type": "pull_request", "provider": "gitlab", "connection_id": connection,
		"repo": "group/subgroup/project", "number": 17, "event": "closed",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("GitLab condition = %#v, want %#v", got, want)
	}
}

func TestChatWakeupCLIUntilRunFlagRemainsRegistered(t *testing.T) {
	for _, name := range []string{"create", "update"} {
		t.Run(name, func(t *testing.T) {
			wakeup := newChatWakeupCommand()
			var command *cobra.Command
			for _, candidate := range wakeup.Commands() {
				if candidate.Name() == name {
					command = candidate
					break
				}
			}
			if command == nil {
				t.Fatalf("chat wakeup %s command missing", name)
			}
			const runID = "44b522ca-5fe6-4ea4-b686-263946524f14"
			if err := command.Flags().Set("until-run", runID); err != nil {
				t.Fatalf("--until-run flag is not registered: %v", err)
			}
			got, err := chatWakeupConditionFromFlags(context.Background(), command, nil)
			if err != nil {
				t.Fatal(err)
			}
			want := map[string]any{"type": "run", "task_id": runID}
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("--until-run condition = %#v, want %#v", got, want)
			}
		})
	}
}

func TestChatWakeupCLIGitLabConnectionRequiresGitLabTarget(t *testing.T) {
	const connection = "44b522ca-5fe6-4ea4-b686-263946524f14"
	for _, tc := range []struct {
		name, untilPR, connection string
	}{
		{name: "connection without target", connection: connection},
		{name: "GitLab project path needs namespace", untilPR: "project#17", connection: connection},
		{name: "GitLab path needs connection", untilPR: "group/subgroup/project#17"},
		{name: "invalid connection UUID", untilPR: "group/subgroup/project#17", connection: "not-a-uuid"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := chatWakeupConditionForFlags(t, tc.untilPR, tc.connection); err == nil {
				t.Fatal("invalid GitLab target accepted")
			}
		})
	}
}
func TestChatWakeupCLIGitLabConnectionDiscoveryIsWorkspaceScoped(t *testing.T) {
	const (
		legacyID        = "44b522ca-5fe6-4ea4-b686-263946524f14"
		foreignLegacyID = "c92386c7-7abe-41ca-9f57-3772ae567cc4"
		vcsID           = "a1d4da51-e0f4-436f-8d6c-8ac6a44e0a48"
		foreignVCSID    = "89a49f14-f32b-43d6-bfb1-e6f330234907"
	)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/workspaces/ws/gitlab/config":
			_ = json.NewEncoder(w).Encode(handler.GitLabConfigResponse{
				Configured:       true,
				CanManage:        false,
				BaseURL:          "https://legacy.example",
				ManualWebhookURL: "https://api.example/gitlab/webhook",
				Connections: []handler.GitLabConnectionResponse{
					{ID: legacyID, WorkspaceID: "ws", BaseURL: "https://legacy.example", Host: "legacy.example"},
					{ID: foreignLegacyID, WorkspaceID: "other-workspace", BaseURL: "https://foreign.example", Host: "foreign.example"},
				},
				Projects: []handler.GitLabProjectBindingResponse{
					{WorkspaceID: "ws", PathWithNamespace: "private/local-project"},
					{WorkspaceID: "other-workspace", PathWithNamespace: "private/foreign-project"},
				},
			})
		case "/api/workspaces/ws/vcs/connections":
			_ = json.NewEncoder(w).Encode(struct {
				Connections []handler.VCSConnectionResponse `json:"connections"`
			}{Connections: []handler.VCSConnectionResponse{
				{ID: vcsID, WorkspaceID: "ws", Provider: "gitlab", InstanceURL: "https://vcs.example"},
				{ID: "forgejo-id", WorkspaceID: "ws", Provider: "forgejo", InstanceURL: "https://forgejo.example"},
				{ID: foreignVCSID, WorkspaceID: "other-workspace", Provider: "gitlab", InstanceURL: "https://foreign-vcs.example"},
			}})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	rows, err := listChatWakeupGitLabConnections(context.Background(), &cli.APIClient{
		BaseURL: server.URL, WorkspaceID: "ws", HTTPClient: server.Client(),
	})
	if err != nil {
		t.Fatal(err)
	}
	want := []map[string]any{
		{
			"id": legacyID, "provider": "gitlab", "source": "gitlab_connection",
			"instance_url": "https://legacy.example", "host": "legacy.example",
		},
		{
			"id": vcsID, "provider": "gitlab", "source": "vcs_connection",
			"instance_url": "https://vcs.example",
		},
	}
	if !reflect.DeepEqual(rows, want) {
		t.Fatalf("GitLab connections = %#v, want workspace-scoped, secret-free IDs %#v", rows, want)
	}
	for _, row := range rows {
		if _, ok := row["projects"]; ok {
			t.Fatalf("connection discovery exposed project paths: %#v", row)
		}
	}
	wakeup := newChatWakeupCommand()
	if !strings.Contains(wakeup.Long, "multica chat wakeup connections") ||
		!strings.Contains(wakeup.Long, "group/subgroup/project#N") {
		t.Fatalf("CLI help does not explain GitLab #N syntax and connection discovery:\n%s", wakeup.Long)
	}
	connections, _, err := wakeup.Find([]string{"connections"})
	if err != nil || connections.Use != "connections" {
		t.Fatalf("GitLab connection discovery is not wired into `multica chat wakeup`: command=%v error=%v", connections, err)
	}
}

func TestChatWakeupCLITableTargetIdentifiesGitLabConnection(t *testing.T) {
	got := chatWakeupTarget(map[string]any{
		"condition": map[string]any{
			"type": "pull_request", "provider": "gitlab", "connection_id": "connection-uuid",
			"repo": "group/subgroup/project", "number": float64(17), "event": "closed",
		},
	})
	if want := "GitLab group/subgroup/project#17 (closed, connection connection-uuid)"; got != want {
		t.Fatalf("GitLab target = %q, want %q", got, want)
	}
}
