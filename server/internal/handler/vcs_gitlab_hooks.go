package handler

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strings"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/integrations/gitlab"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/multica-ai/multica/server/pkg/redact"
)

type managedGitLabHook struct {
	project db.VcsGitlabProject
	name    string
	url     string
}

func (h *Handler) managedGitLabHooks(ctx context.Context, conn db.VcsConnection) (gitLabAPI, []managedGitLabHook, error) {
	projects, err := h.Queries.ListVCSGitLabProjectsByWorkspace(ctx, conn.WorkspaceID)
	if err != nil {
		return nil, nil, err
	}
	var managed []managedGitLabHook
	for _, project := range projects {
		if project.ConnectionID == conn.ID && project.HookID.Valid {
			managed = append(managed, managedGitLabHook{project: project})
		}
	}
	if len(managed) == 0 {
		return nil, managed, nil
	}
	cfg, err := h.gitLabConfigForVCSConnectionRow(conn)
	if err != nil {
		return nil, nil, err
	}
	api, err := newGitLabClient(cfg)
	if err != nil {
		return nil, nil, err
	}
	expected := h.vcsWebhookURL(uuidToString(conn.ID))
	var failures []error
	for i := range managed {
		project := managed[i].project
		remote, err := api.GetProjectHook(ctx, project.GitlabProjectID, project.HookID.Int64)
		if err == nil {
			var parsed *url.URL
			parsed, err = url.Parse(remote.URL)
			if err == nil {
				parsed.RawQuery = ""
				parsed.Fragment = ""
				if parsed.String() != expected {
					err = errors.New("webhook no longer targets this connection")
				}
			}
		}
		if err != nil {
			failures = append(failures, fmt.Errorf("%s: %s", project.PathWithNamespace, redact.Text(err.Error())))
			continue
		}
		managed[i].name = remote.Name
		managed[i].url = remote.URL
	}
	if len(failures) != 0 {
		return nil, nil, errors.Join(failures...)
	}
	return api, managed, nil
}

func (h *Handler) rotateManagedGitLabHooks(ctx context.Context, conn db.VcsConnection, newSecret string) (func() error, error) {
	api, hooks, err := h.managedGitLabHooks(ctx, conn)
	if err != nil {
		return nil, err
	}
	if len(hooks) == 0 {
		return nil, nil
	}
	oldSecret, err := h.openVCSSecret(conn.WebhookSecretEncrypted)
	if err != nil {
		return nil, err
	}
	userName, err := api.GetCurrentUserName(ctx)
	if err != nil {
		return nil, err
	}
	changed := make([]managedGitLabHook, 0, len(hooks))
	rollback := func() error {
		var failures []error
		for _, hook := range changed {
			if err := api.UpdateProjectHookSecret(ctx, hook.project.GitlabProjectID, hook.project.HookID.Int64, hook.url, oldSecret, hook.name); err != nil {
				failures = append(failures, fmt.Errorf("%s: rollback failed: %s", hook.project.PathWithNamespace, redact.Text(err.Error())))
			}
		}
		return errors.Join(failures...)
	}
	var failures []error
	for _, hook := range hooks {
		changed = append(changed, hook)
		if err := api.UpdateProjectHookSecret(ctx, hook.project.GitlabProjectID, hook.project.HookID.Int64, hook.url, newSecret, userName+gitlab.ProjectHookSuffix); err != nil {
			failures = append(failures, fmt.Errorf("%s: %s", hook.project.PathWithNamespace, redact.Text(err.Error())))
			continue
		}
	}
	if len(failures) != 0 {
		return nil, errors.Join(errors.Join(failures...), rollback())
	}
	return rollback, nil
}

func (h *Handler) deleteManagedGitLabHooks(ctx context.Context, conn db.VcsConnection) error {
	api, hooks, err := h.managedGitLabHooks(ctx, conn)
	if err != nil {
		return err
	}
	var failures []error
	for _, hook := range hooks {
		project := hook.project
		if err := api.DeleteProjectHook(ctx, project.GitlabProjectID, project.HookID.Int64); err != nil {
			failures = append(failures, fmt.Errorf("%s: %s", project.PathWithNamespace, redact.Text(err.Error())))
			continue
		}
		_, err := h.Queries.UpsertVCSGitLabProject(ctx, db.UpsertVCSGitLabProjectParams{
			WorkspaceID: project.WorkspaceID, ConnectionID: project.ConnectionID,
			GitlabProjectID: project.GitlabProjectID, PathWithNamespace: project.PathWithNamespace,
			WebUrl: project.WebUrl, HookID: pgtype.Int8{}, HookEnabled: false, LastSyncError: project.LastSyncError,
		})
		if err != nil {
			failures = append(failures, fmt.Errorf("%s: could not record deleted webhook", project.PathWithNamespace))
		}
	}
	return errors.Join(failures...)
}

func gitLabHookFailureMessage(action string, err error) string {
	return fmt.Sprintf("GitLab webhook %s failed: %s", action, strings.ReplaceAll(err.Error(), "\n", "; "))
}
