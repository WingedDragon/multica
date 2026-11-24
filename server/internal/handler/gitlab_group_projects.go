package handler

import (
	"errors"
	"net/http"
	"net/url"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
	"github.com/multica-ai/multica/server/internal/integrations/gitlab"
	"github.com/multica-ai/multica/server/pkg/redact"
)

type gitLabGroupProjectResponse struct {
	ID                int64  `json:"id"`
	PathWithNamespace string `json:"path_with_namespace"`
	WebURL            string `json:"web_url"`
}

// normalizeGitLabGroupInput accepts either a GitLab namespace path or a group
// URL on the configured instance. URLs must include the connection's base path.
func normalizeGitLabGroupInput(raw, instanceURL string) (string, bool) {
	group := strings.TrimSpace(raw)
	instanceURL = strings.TrimRight(strings.TrimSpace(instanceURL), "/")
	if group == "" || instanceURL == "" {
		return "", false
	}

	instance, err := url.Parse(instanceURL)
	if err != nil || (!strings.EqualFold(instance.Scheme, "http") && !strings.EqualFold(instance.Scheme, "https")) || instance.Host == "" ||
		instance.User != nil || instance.RawQuery != "" || instance.ForceQuery || instance.Fragment != "" || strings.Contains(instanceURL, "#") {
		return "", false
	}

	if strings.Contains(group, "://") {
		groupURL, err := url.Parse(group)
		if err != nil || (!strings.EqualFold(groupURL.Scheme, "http") && !strings.EqualFold(groupURL.Scheme, "https")) ||
			!strings.EqualFold(groupURL.Scheme, instance.Scheme) ||
			!strings.EqualFold(groupURL.Host, instance.Host) ||
			groupURL.User != nil || groupURL.RawQuery != "" || groupURL.ForceQuery || groupURL.Fragment != "" || strings.Contains(group, "#") {
			return "", false
		}

		groupPath := groupURL.Path
		instancePath := strings.TrimRight(instance.Path, "/")
		if instancePath != "" {
			prefix := instancePath + "/"
			if !strings.HasPrefix(groupPath, prefix) {
				return "", false
			}
			groupPath = strings.TrimPrefix(groupPath, prefix)
		}
		group = strings.TrimPrefix(groupPath, "/")
		if strings.HasPrefix(group, "groups/") {
			group = strings.TrimPrefix(group, "groups/")
			group, _, _ = strings.Cut(group, "/-/")
		}
	} else if strings.HasPrefix(group, "//") {
		return "", false
	}

	group = strings.Trim(group, "/")
	if group == "" || strings.Contains(group, "://") || strings.ContainsAny(group, "\\?#\r\n\t ") {
		return "", false
	}
	for _, segment := range strings.Split(group, "/") {
		if segment == "" || segment == "." || segment == ".." {
			return "", false
		}
	}
	return group, true
}

func (h *Handler) ListVCSGitLabGroupProjects(w http.ResponseWriter, r *http.Request) {
	if !h.isVCSAvailable() {
		writeError(w, http.StatusNotFound, "vcs integration is not available on this deployment")
		return
	}

	workspaceID, ok := parseUUIDOrBadRequest(w, chi.URLParam(r, "id"), "workspace id")
	if !ok {
		return
	}
	connectionIDValue := strings.TrimSpace(r.URL.Query().Get("connection_id"))
	if connectionIDValue == "" {
		writeError(w, http.StatusBadRequest, "connection_id is required")
		return
	}
	connectionID, ok := parseUUIDOrBadRequest(w, connectionIDValue, "connection id")
	if !ok {
		return
	}
	groupInput := r.URL.Query().Get("group")
	if strings.TrimSpace(groupInput) == "" {
		writeError(w, http.StatusBadRequest, "group is required")
		return
	}

	connection, err := h.resolveGitLabVCSConnection(r.Context(), workspaceID, connectionID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			writeError(w, http.StatusNotFound, "GitLab connection not found in this workspace")
		} else {
			writeError(w, http.StatusInternalServerError, "failed to load GitLab connection")
		}
		return
	}
	cfg, err := h.gitLabConfigForVCSConnectionRow(connection)
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, "GitLab connection credentials are unavailable")
		return
	}
	groupPath, ok := normalizeGitLabGroupInput(groupInput, cfg.BaseURL)
	if !ok {
		writeError(w, http.StatusBadRequest, "invalid GitLab group path or URL")
		return
	}

	client, err := gitlab.NewClient(cfg)
	if err != nil {
		writeError(w, http.StatusBadGateway, "failed to create GitLab client: "+gitLabGroupProjectsError(err, cfg.Token))
		return
	}
	projects, err := client.ListGroupProjects(r.Context(), groupPath)
	if err != nil {
		writeError(w, http.StatusBadGateway, "failed to list GitLab group projects: "+gitLabGroupProjectsError(err, cfg.Token))
		return
	}

	response := make([]gitLabGroupProjectResponse, 0, len(projects))
	for _, project := range projects {
		response = append(response, gitLabGroupProjectResponse{
			ID:                project.ID,
			PathWithNamespace: project.PathWithNamespace,
			WebURL:            project.WebURL,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"projects": response})
}

func gitLabGroupProjectsError(err error, token string) string {
	message := err.Error()
	if token != "" {
		message = strings.ReplaceAll(message, token, "[REDACTED]")
	}
	return redact.Text(message)
}
