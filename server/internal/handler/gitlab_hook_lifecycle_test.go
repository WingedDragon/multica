package handler

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/multica-ai/multica/server/internal/integrations/gitlab"
)

type lifecycleGitLabAPI struct {
	fakeGitLabAPI
	urls       map[int64]string
	names      map[int64]string
	secrets    map[int64]string
	failUpdate int64
	failDelete int64
	deleted    []int64
}

func (f *lifecycleGitLabAPI) GetProjectHook(_ context.Context, _, hookID int64) (gitlab.ProjectHook, error) {
	return gitlab.ProjectHook{ID: hookID, URL: f.urls[hookID], Name: f.names[hookID]}, nil
}
func (f *lifecycleGitLabAPI) GetCurrentUserName(context.Context) (string, error) {
	return "Test User", nil
}

func (f *lifecycleGitLabAPI) UpdateProjectHookSecret(_ context.Context, _, hookID int64, url, secret, name string) error {
	if hookID == f.failUpdate {
		return errors.New("permission denied")
	}
	if url != f.urls[hookID] {
		return errors.New("hook URL changed")
	}
	f.secrets[hookID] = secret
	f.names[hookID] = name
	return nil
}

func (f *lifecycleGitLabAPI) DeleteProjectHook(_ context.Context, _, hookID int64) error {
	if hookID == f.failDelete {
		return errors.New("permission denied")
	}
	f.deleted = append(f.deleted, hookID)
	return nil
}

func TestGitLabHookLifecycle(t *testing.T) {
	for _, test := range []struct {
		name         string
		rotate, fail bool
	}{
		{"rotate", true, false}, {"rotate_failure", true, true},
		{"disconnect", false, false}, {"disconnect_failure", false, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			box := withVCSBox(t)
			withPublicURLForGitLabTest(t, "https://multica.example")
			connID, _ := seedGitLabProjectTestConnection(t, box, "gitlab", "https://gitlab-lifecycle.example")
			first, second := int64(101), int64(102)
			seedCanonicalGitLabProject(t, testWorkspaceID, connID, 1, "group/one", &first, true)
			seedCanonicalGitLabProject(t, testWorkspaceID, connID, 2, "group/two", &second, true)
			otherID, _ := seedGitLabProjectTestConnection(t, box, "gitlab", "https://gitlab-other.example")
			otherHook := int64(103)
			seedCanonicalGitLabProject(t, testWorkspaceID, otherID, 3, "group/other", &otherHook, true)
			target := "https://multica.example/api/webhooks/vcs/" + connID
			fake := &lifecycleGitLabAPI{urls: map[int64]string{101: target, 102: target + "?scope=project", 103: "unrelated"}, secrets: map[int64]string{101: vcsTestSecret, 102: vcsTestSecret}, names: map[int64]string{101: "old one", 102: "old two"}}
			if test.fail && test.rotate {
				fake.failUpdate = 102
			}
			if test.fail && !test.rotate {
				fake.failDelete = 102
			}
			installCanonicalGitLabProjectAPI(t, fake)
			rec := httptest.NewRecorder()
			if test.rotate {
				testHandler.RotateVCSConnectionWebhook(rec, directGitLabProjectRequest(http.MethodPost, "/", testWorkspaceID, nil, "connectionId", connID))
				if test.fail {
					if rec.Code < 400 || !strings.Contains(rec.Body.String(), "group/two") {
						t.Fatalf("rotate failure = %d %s", rec.Code, rec.Body.String())
					}
					if fake.secrets[101] != vcsTestSecret || fake.secrets[102] != vcsTestSecret {
						t.Fatal("old hooks not restored after failed rotation")
					}
					if fake.names[101] != "old one" {
						t.Fatal("old hook name not restored after failed rotation")
					}
				} else {
					if rec.Code != http.StatusOK {
						t.Fatalf("rotate = %d %s", rec.Code, rec.Body.String())
					}
					var response struct {
						Secret string `json:"webhook_secret"`
					}
					if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
						t.Fatal(err)
					}
					if response.Secret == "" || response.Secret == vcsTestSecret || fake.secrets[101] != response.Secret || fake.secrets[102] != response.Secret {
						t.Fatal("remote hooks do not use rotated secret")
					}
					if fake.names[101] != "Test User Multica Hook" || fake.names[102] != "Test User Multica Hook" {
						t.Fatalf("hook names = %v", fake.names)
					}
				}
			} else {
				testHandler.DeleteVCSConnection(rec, directGitLabProjectRequest(http.MethodDelete, "/", testWorkspaceID, nil, "connectionId", connID))
				if test.fail {
					if rec.Code < 400 || !strings.Contains(rec.Body.String(), "group/two") {
						t.Fatalf("disconnect failure = %d %s", rec.Code, rec.Body.String())
					}
				} else if rec.Code != http.StatusNoContent {
					t.Fatalf("disconnect = %d %s", rec.Code, rec.Body.String())
				}
				if fmt.Sprint(fake.deleted) != "[101]" && test.fail {
					t.Fatalf("deleted hooks = %v", fake.deleted)
				}
				if test.fail {
					fake.failDelete = 0
					retry := httptest.NewRecorder()
					testHandler.DeleteVCSConnection(retry, directGitLabProjectRequest(http.MethodDelete, "/", testWorkspaceID, nil, "connectionId", connID))
					if retry.Code != http.StatusNoContent || fmt.Sprint(fake.deleted) != "[101 102]" {
						t.Fatalf("disconnect retry = %d %s, deleted hooks = %v", retry.Code, retry.Body.String(), fake.deleted)
					}
				}
				if fmt.Sprint(fake.deleted) != "[101 102]" && !test.fail {
					t.Fatalf("deleted hooks = %v", fake.deleted)
				}
			}
		})
	}
}
