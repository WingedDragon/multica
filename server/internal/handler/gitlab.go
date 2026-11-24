package handler

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/integrations/gitlab"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/multica-ai/multica/server/pkg/protocol"
	"github.com/multica-ai/multica/server/pkg/redact"
)

type gitLabAPI interface {
	GetProject(context.Context, string) (gitlab.Project, error)
	ValidateCredentials(context.Context) error
	CreateProjectHook(context.Context, int64, string, string) (gitlab.ProjectHook, error)
	GetProjectHook(context.Context, int64, int64) (gitlab.ProjectHook, error)
	GetCurrentUserName(context.Context) (string, error)
	UpdateProjectHookSecret(context.Context, int64, int64, string, string, string) error
	DeleteProjectHook(context.Context, int64, int64) error
}

var newGitLabClient = func(cfg gitlab.Config) (gitLabAPI, error) {
	return gitlab.NewClient(cfg)
}

type GitLabProjectBindingResponse struct {
	ID                  string  `json:"id"`
	WorkspaceID         string  `json:"workspace_id"`
	ConnectionID        string  `json:"connection_id"`
	GitLabProjectID     int64   `json:"gitlab_project_id"`
	PathWithNamespace   string  `json:"path_with_namespace"`
	WebURL              string  `json:"web_url"`
	HookID              *int64  `json:"hook_id,omitempty"`
	HookEnabled         bool    `json:"hook_enabled"`
	LegacyLinkPolicy    bool    `json:"legacy_link_policy"`
	LastSyncError       *string `json:"last_sync_error,omitempty"`
	LastRefreshAt       *string `json:"last_refresh_at,omitempty"`
	LastRefreshError    *string `json:"last_refresh_error,omitempty"`
	LastEventAt         *string `json:"last_event_at,omitempty"`
	LastEventType       *string `json:"last_event_type,omitempty"`
	RefreshInProgressAt *string `json:"refresh_in_progress_at,omitempty"`
	CreatedAt           string  `json:"created_at"`
}
type GitLabConnectionResponse struct {
	ID               string `json:"id"`
	WorkspaceID      string `json:"workspace_id"`
	BaseURL          string `json:"base_url"`
	Host             string `json:"host"`
	ServerConfigured bool   `json:"server_configured"`
	WebhookURL       string `json:"webhook_url,omitempty"`
	WebhookPath      string `json:"webhook_path,omitempty"`
}

type GitLabConfigResponse struct {
	Configured                  bool                           `json:"configured"`
	CanManage                   bool                           `json:"can_manage"`
	CredentialStorageConfigured bool                           `json:"credential_storage_configured"`
	BaseURL                     string                         `json:"base_url,omitempty"`
	ManualWebhookURL            string                         `json:"manual_webhook_url,omitempty"`
	Connections                 []GitLabConnectionResponse     `json:"connections"`
	Projects                    []GitLabProjectBindingResponse `json:"projects"`
}

type createGitLabConnectionRequest struct {
	BaseURL     string `json:"base_url"`
	AccessToken string `json:"access_token"`
}

type createGitLabProjectRequest struct {
	ConnectionID string `json:"connection_id"`
	Project      string `json:"project"`
}

type GitLabMergeRequestResponse struct {
	ID                  string          `json:"id"`
	WorkspaceID         string          `json:"workspace_id"`
	ProjectPath         string          `json:"project_path"`
	GitLabProjectID     int64           `json:"gitlab_project_id"`
	IID                 int32           `json:"iid"`
	Title               string          `json:"title"`
	State               string          `json:"state"`
	WebURL              string          `json:"web_url"`
	SourceBranch        *string         `json:"source_branch"`
	TargetBranch        *string         `json:"target_branch"`
	AuthorUsername      *string         `json:"author_username"`
	AuthorAvatarURL     *string         `json:"author_avatar_url"`
	SHA                 string          `json:"sha"`
	DetailedMergeStatus *string         `json:"detailed_merge_status"`
	HasConflicts        *bool           `json:"has_conflicts"`
	PipelineStatus      *string         `json:"pipeline_status"`
	PipelineURL         *string         `json:"pipeline_url"`
	Additions           int32           `json:"additions"`
	Deletions           int32           `json:"deletions"`
	ChangedFiles        int32           `json:"changed_files"`
	Reviewers           json.RawMessage `json:"reviewers,omitempty"`
	Assignees           json.RawMessage `json:"assignees,omitempty"`
	Labels              json.RawMessage `json:"labels,omitempty"`
	LastRefreshedAt     *string         `json:"last_refreshed_at,omitempty"`
	LastRefreshError    *string         `json:"last_refresh_error,omitempty"`
	MergedAt            *string         `json:"merged_at"`
	ClosedAt            *string         `json:"closed_at"`
	MRCreatedAt         string          `json:"mr_created_at"`
	MRUpdatedAt         string          `json:"mr_updated_at"`
}

func gitLabProjectBindingToResponse(row db.GitlabProjectBinding) GitLabProjectBindingResponse {
	return GitLabProjectBindingResponse{
		ID:                  uuidToString(row.ID),
		ConnectionID:        uuidToString(row.ConnectionID),
		WorkspaceID:         uuidToString(row.WorkspaceID),
		GitLabProjectID:     row.GitlabProjectID,
		PathWithNamespace:   row.PathWithNamespace,
		WebURL:              row.WebUrl,
		HookID:              int64PtrFromPg(row.HookID),
		HookEnabled:         row.HookEnabled,
		LastSyncError:       redactedTextPtr(row.LastSyncError),
		LastRefreshAt:       timestampToPtr(row.LastRefreshAt),
		LastRefreshError:    redactedTextPtr(row.LastRefreshError),
		LastEventAt:         timestampToPtr(row.LastEventAt),
		LastEventType:       textToPtr(row.LastEventType),
		RefreshInProgressAt: timestampToPtr(row.RefreshInProgressAt),
		CreatedAt:           timestampToString(row.CreatedAt),
	}
}

func redactedTextPtr(t pgtype.Text) *string {
	if !t.Valid {
		return nil
	}
	s := redact.Text(t.String)
	return &s
}

func int64PtrFromPg(v pgtype.Int8) *int64 {
	if !v.Valid {
		return nil
	}
	return &v.Int64
}

func boolPtrFromPg(v pgtype.Bool) *bool {
	if !v.Valid {
		return nil
	}
	return &v.Bool
}

func normalizeGitLabProjectInput(raw string, instanceURL string) (string, bool) {
	s := strings.TrimSpace(raw)
	instanceURL = strings.TrimRight(strings.TrimSpace(instanceURL), "/")
	if s == "" || instanceURL == "" {
		return "", false
	}
	instance, err := url.Parse(instanceURL)
	if err != nil || (instance.Scheme != "http" && instance.Scheme != "https") || instance.Host == "" {
		return "", false
	}
	if strings.Contains(s, "://") {
		projectURL, err := url.Parse(s)
		if err != nil || (projectURL.Scheme != "http" && projectURL.Scheme != "https") ||
			!strings.EqualFold(projectURL.Scheme, instance.Scheme) ||
			!strings.EqualFold(projectURL.Host, instance.Host) ||
			projectURL.User != nil || projectURL.RawQuery != "" || projectURL.Fragment != "" {
			return "", false
		}
		s = projectURL.Path
		instancePath := strings.Trim(instance.Path, "/")
		if instancePath != "" {
			if s == "/"+instancePath {
				return "", false
			}
			prefix := "/" + instancePath + "/"
			if !strings.HasPrefix(s, prefix) {
				return "", false
			}
			s = strings.TrimPrefix(s, prefix)
		}
	}
	s = strings.Trim(s, "/")
	s = strings.TrimSuffix(s, ".git")
	s = strings.Trim(s, "/")
	if s == "" || strings.Contains(s, "://") || strings.ContainsAny(s, "\\?#") {
		return "", false
	}
	for _, segment := range strings.Split(s, "/") {
		if segment == "" || segment == "." || segment == ".." {
			return "", false
		}
	}
	return s, true
}

func normalizeGitLabConnectionBaseURL(raw string) (string, string, bool) {
	baseURL := strings.TrimRight(strings.TrimSpace(raw), "/")
	if baseURL == "" {
		return "", "", false
	}
	u, err := url.Parse(baseURL)
	if err != nil || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return "", "", false
	}
	u.Scheme = strings.ToLower(u.Scheme)
	u.Host = strings.ToLower(u.Host)
	if u.Scheme != "http" && u.Scheme != "https" {
		return "", "", false
	}
	baseURL = strings.TrimRight(u.String(), "/")
	return baseURL, u.Host, true
}

func (h *Handler) GetGitLabConfig(w http.ResponseWriter, r *http.Request) {
	workspaceID := chi.URLParam(r, "id")
	wsUUID, ok := parseUUIDOrBadRequest(w, workspaceID, "workspace id")
	if !ok {
		return
	}
	member, _ := ctxMember(r.Context())
	connections, err := h.Queries.ListLegacyGitLabVCSConnectionsByWorkspace(r.Context(), wsUUID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to list GitLab connections")
		return
	}
	connectionResponses := make([]GitLabConnectionResponse, 0, len(connections))
	legacyConnectionIDs := make(map[string]struct{}, len(connections))
	configured := false
	manualWebhookURL := ""
	for _, connection := range connections {
		connectionID := uuidToString(connection.ID)
		legacyConnectionIDs[connectionID] = struct{}{}
		response := h.vcsGitLabConnectionToResponse(connection, true)
		connectionResponses = append(connectionResponses, response)
		if response.ServerConfigured {
			configured = true
		}
		if manualWebhookURL == "" {
			manualWebhookURL = response.WebhookURL
		}
	}
	rows, err := h.Queries.ListVCSGitLabProjectsByWorkspace(r.Context(), wsUUID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to list GitLab projects")
		return
	}
	projects := make([]GitLabProjectBindingResponse, 0, len(rows))
	for _, row := range rows {
		if _, ok := legacyConnectionIDs[uuidToString(row.ConnectionID)]; !ok {
			continue
		}
		projects = append(projects, vcsGitLabProjectToResponse(row))
	}
	baseURL := ""
	if len(connectionResponses) == 1 {
		baseURL = connectionResponses[0].BaseURL
	}
	writeJSON(w, http.StatusOK, GitLabConfigResponse{
		Configured:                  configured,
		CanManage:                   roleAllowed(member.Role, "owner", "admin"),
		CredentialStorageConfigured: h.VCSSecretBox != nil,
		BaseURL:                     baseURL,
		ManualWebhookURL:            manualWebhookURL,
		Connections:                 connectionResponses,
		Projects:                    projects,
	})
}

func (h *Handler) CreateGitLabProjectBinding(w http.ResponseWriter, r *http.Request) {
	if !h.isVCSAvailable() {
		writeError(w, http.StatusNotFound, "vcs integration is not available on this deployment")
		return
	}
	workspaceID := chi.URLParam(r, "id")
	wsUUID, ok := parseUUIDOrBadRequest(w, workspaceID, "workspace id")
	if !ok {
		return
	}
	var req createGitLabProjectRequest
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	var requestedConnectionID pgtype.UUID
	if strings.TrimSpace(req.ConnectionID) != "" {
		requestedConnectionID, ok = parseUUIDOrBadRequest(w, strings.TrimSpace(req.ConnectionID), "connection id")
		if !ok {
			return
		}
	}
	connection, err := h.resolveLegacyGitLabVCSConnection(r.Context(), wsUUID, requestedConnectionID)
	if err != nil {
		switch {
		case errors.Is(err, errGitLabLegacyConnectionAmbiguous):
			writeError(w, http.StatusConflict, "multiple legacy GitLab connections are available; specify connection_id")
		case errors.Is(err, pgx.ErrNoRows):
			writeError(w, http.StatusNotFound, "legacy GitLab connection not found in this workspace")
		default:
			writeError(w, http.StatusInternalServerError, "failed to resolve legacy GitLab connection")
		}
		return
	}
	req.ConnectionID = uuidToString(connection.ID)
	h.createVCSGitLabProject(w, r, wsUUID, req)
}

func gitLabProjectCloneURL(project gitlab.Project) string {
	for _, candidate := range []string{project.HTTPURLToRepo, project.SSHURLToRepo} {
		candidate = strings.TrimSpace(candidate)
		if candidate != "" {
			return candidate
		}
	}
	return ""
}

func (h *Handler) ensureGitLabProjectWorkspaceRepo(ctx context.Context, workspaceID pgtype.UUID, project gitlab.Project, userID string) error {
	repoURL := gitLabProjectCloneURL(project)
	if repoURL == "" || !isValidGitRepoURL(repoURL) {
		return nil
	}

	ws, err := h.Queries.GetWorkspace(ctx, workspaceID)
	if err != nil {
		return err
	}

	var repos []workspaceRepoRef
	if len(ws.Repos) > 0 {
		if err := json.Unmarshal(ws.Repos, &repos); err != nil {
			return err
		}
	}
	for _, repo := range repos {
		if strings.TrimSpace(repo.URL) == repoURL {
			return nil
		}
	}

	repos = append(repos, workspaceRepoRef{
		URL:         repoURL,
		Description: "GitLab: " + project.PathWithNamespace,
	})
	reposJSON, err := validateAndNormalizeWorkspaceRepos(repos)
	if err != nil {
		return err
	}
	updated, err := h.Queries.UpdateWorkspace(ctx, db.UpdateWorkspaceParams{
		ID:    workspaceID,
		Repos: reposJSON,
	})
	if err != nil {
		return err
	}
	h.publish(protocol.EventWorkspaceUpdated, uuidToString(workspaceID), "member", userID, map[string]any{
		"workspace": h.workspaceToResponse(updated),
	})
	return nil
}

func (h *Handler) DeleteGitLabProjectBinding(w http.ResponseWriter, r *http.Request) {
	h.deleteVCSGitLabProject(w, r, "bindingId")
}

func (h *Handler) ListGitLabMergeRequestsForIssue(w http.ResponseWriter, r *http.Request) {
	issue, ok := h.loadIssueForUser(w, r, chi.URLParam(r, "id"))
	if !ok {
		return
	}
	rows, err := h.Queries.ListGitLabVCSMergeRequestsForIssue(r.Context(), db.ListGitLabVCSMergeRequestsForIssueParams{
		IssueID:     issue.ID,
		WorkspaceID: issue.WorkspaceID,
	})
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to list GitLab merge requests")
		return
	}
	out := make([]GitLabMergeRequestResponse, 0, len(rows))
	for _, row := range rows {
		out = append(out, gitLabMergeRequestRowToResponse(row))
	}
	writeJSON(w, http.StatusOK, map[string]any{"merge_requests": out})
}

func gitLabMergeRequestRowToResponse(row db.ListGitLabVCSMergeRequestsForIssueRow) GitLabMergeRequestResponse {
	return GitLabMergeRequestResponse{
		ID:                  uuidToString(row.ID),
		WorkspaceID:         uuidToString(row.WorkspaceID),
		ProjectPath:         row.PathWithNamespace,
		GitLabProjectID:     row.GitlabProjectID,
		IID:                 row.MrIid,
		Title:               row.Title,
		State:               row.State,
		WebURL:              row.WebUrl,
		SourceBranch:        textToPtr(row.SourceBranch),
		TargetBranch:        textToPtr(row.TargetBranch),
		AuthorUsername:      textToPtr(row.AuthorUsername),
		AuthorAvatarURL:     textToPtr(row.AuthorAvatarUrl),
		SHA:                 row.Sha,
		DetailedMergeStatus: textToPtr(row.DetailedMergeStatus),
		HasConflicts:        boolPtrFromPg(row.HasConflicts),
		PipelineStatus:      strPtrOrNil(row.PipelineStatus),
		PipelineURL:         textToPtr(row.PipelineUrl),
		Additions:           row.Additions,
		Deletions:           row.Deletions,
		ChangedFiles:        row.ChangedFiles,
		Reviewers:           jsonArrayOrEmpty(row.Reviewers),
		Assignees:           jsonArrayOrEmpty(row.Assignees),
		Labels:              jsonArrayOrEmpty(row.Labels),
		LastRefreshedAt:     timestampToPtr(row.LastRefreshedAt),
		LastRefreshError:    redactedTextPtr(row.LastRefreshError),
		MergedAt:            timestampToPtr(row.MergedAt),
		ClosedAt:            timestampToPtr(row.ClosedAt),
		MRCreatedAt:         timestampToString(row.MrCreatedAt),
		MRUpdatedAt:         timestampToString(row.MrUpdatedAt),
	}
}

func deriveGitLabMRState(state string, draft bool) string {
	switch state {
	case "merged":
		return "merged"
	case "closed":
		return "closed"
	}
	if draft {
		return "draft"
	}
	return "open"
}

func normalizeGitLabPipelineStatus(s string) string {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "success", "passed":
		return "passed"
	case "failed", "canceled":
		return "failed"
	case "skipped":
		return "passed"
	case "manual":
		return "pending"
	case "running", "pending", "created", "waiting_for_resource", "preparing", "scheduled":
		return "pending"
	default:
		return "pending"
	}
}

func parseGitLabTime(s string) pgtype.Timestamptz {
	s = strings.TrimSpace(s)
	if s == "" {
		return pgtype.Timestamptz{}
	}
	for _, layout := range []string{time.RFC3339Nano, time.RFC3339, "2006-01-02 15:04:05 UTC"} {
		if t, err := time.Parse(layout, s); err == nil {
			return pgtype.Timestamptz{Time: t, Valid: true}
		}
	}
	return pgtype.Timestamptz{}
}

func parseGitLabTimeRequired(s string) pgtype.Timestamptz {
	t := parseGitLabTime(s)
	if !t.Valid {
		return pgtype.Timestamptz{Time: time.Now().UTC(), Valid: true}
	}
	return t
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}

func (h *Handler) vcsGitLabConnectionToResponse(connection db.VcsConnection, includeWebhook bool) GitLabConnectionResponse {
	baseURL, host, _ := normalizeGitLabConnectionBaseURL(connection.InstanceUrl)
	response := GitLabConnectionResponse{
		ID:               uuidToString(connection.ID),
		WorkspaceID:      uuidToString(connection.WorkspaceID),
		BaseURL:          baseURL,
		Host:             host,
		ServerConfigured: gitLabVCSConnectionServerConfigured(connection),
	}
	if includeWebhook {
		response.WebhookURL = h.vcsWebhookURL(response.ID)
		response.WebhookPath = h.vcsWebhookPath(response.ID)
	}
	return response
}

func vcsGitLabProjectToResponse(row db.VcsGitlabProject) GitLabProjectBindingResponse {
	return GitLabProjectBindingResponse{
		ID:                  uuidToString(row.ID),
		WorkspaceID:         uuidToString(row.WorkspaceID),
		ConnectionID:        uuidToString(row.ConnectionID),
		GitLabProjectID:     row.GitlabProjectID,
		PathWithNamespace:   row.PathWithNamespace,
		WebURL:              row.WebUrl,
		HookID:              int64PtrFromPg(row.HookID),
		HookEnabled:         row.HookEnabled,
		LegacyLinkPolicy:    row.LegacyLinkPolicy,
		LastSyncError:       redactedTextPtr(row.LastSyncError),
		LastRefreshAt:       timestampToPtr(row.LastRefreshAt),
		LastRefreshError:    redactedTextPtr(row.LastRefreshError),
		LastEventAt:         timestampToPtr(row.LastEventAt),
		LastEventType:       textToPtr(row.LastEventType),
		RefreshInProgressAt: timestampToPtr(row.RefreshInProgressAt),
		CreatedAt:           timestampToString(row.CreatedAt),
	}
}

func (h *Handler) ListVCSGitLabProjects(w http.ResponseWriter, r *http.Request) {
	if !h.isVCSAvailable() {
		writeJSON(w, http.StatusOK, map[string]any{"projects": []GitLabProjectBindingResponse{}})
		return
	}
	workspaceID := chi.URLParam(r, "id")
	wsUUID, ok := parseUUIDOrBadRequest(w, workspaceID, "workspace id")
	if !ok {
		return
	}
	rows, err := h.Queries.ListVCSGitLabProjectsByWorkspace(r.Context(), wsUUID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to list GitLab projects")
		return
	}
	projects := make([]GitLabProjectBindingResponse, 0, len(rows))
	for _, row := range rows {
		projects = append(projects, vcsGitLabProjectToResponse(row))
	}
	writeJSON(w, http.StatusOK, map[string]any{"projects": projects})
}

func (h *Handler) CreateVCSGitLabProject(w http.ResponseWriter, r *http.Request) {
	if !h.isVCSAvailable() {
		writeError(w, http.StatusNotFound, "vcs integration is not available on this deployment")
		return
	}
	workspaceID := chi.URLParam(r, "id")
	wsUUID, ok := parseUUIDOrBadRequest(w, workspaceID, "workspace id")
	if !ok {
		return
	}
	var req createGitLabProjectRequest
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	h.createVCSGitLabProject(w, r, wsUUID, req)
}

func (h *Handler) createVCSGitLabProject(w http.ResponseWriter, r *http.Request, workspaceID pgtype.UUID, req createGitLabProjectRequest) {
	if req.ConnectionID == "" {
		writeError(w, http.StatusBadRequest, "connection_id is required")
		return
	}
	connectionID, ok := parseUUIDOrBadRequest(w, req.ConnectionID, "connection id")
	if !ok {
		return
	}
	connection, err := h.resolveGitLabVCSConnection(r.Context(), workspaceID, connectionID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			writeError(w, http.StatusNotFound, "GitLab connection not found")
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
	projectPath, ok := normalizeGitLabProjectInput(req.Project, cfg.BaseURL)
	if !ok {
		writeError(w, http.StatusBadRequest, "invalid GitLab project")
		return
	}
	api, err := newGitLabClient(cfg)
	if err != nil {
		writeError(w, http.StatusBadGateway, redact.Text(err.Error()))
		return
	}
	project, err := api.GetProject(r.Context(), projectPath)
	if err != nil {
		writeError(w, http.StatusBadGateway, redact.Text(err.Error()))
		return
	}
	if project.ID <= 0 || strings.TrimSpace(project.PathWithNamespace) == "" {
		writeError(w, http.StatusBadGateway, "GitLab returned an invalid project")
		return
	}

	var existing db.VcsGitlabProject
	existingFound := false
	existing, err = h.Queries.GetVCSGitLabProjectByRemoteID(r.Context(), db.GetVCSGitLabProjectByRemoteIDParams{
		WorkspaceID:     workspaceID,
		ConnectionID:    connection.ID,
		GitlabProjectID: project.ID,
	})
	if err == nil {
		existingFound = true
	} else if !errors.Is(err, pgx.ErrNoRows) {
		writeError(w, http.StatusInternalServerError, "failed to check existing GitLab project")
		return
	}

	webhookURL := h.vcsWebhookURL(uuidToString(connection.ID))
	hookEnabled := existingFound && existing.HookEnabled && existing.HookID.Valid
	hookID := pgtype.Int8{}
	if existingFound {
		hookID = existing.HookID
	}
	lastSyncError := pgtype.Text{}
	if hookEnabled {
		hookID = existing.HookID
	} else if h.cfg.PublicURL != "" && cfg.WebhookSecret != "" {
		hook, err := api.CreateProjectHook(r.Context(), project.ID, webhookURL, cfg.WebhookSecret)
		if err != nil {
			lastSyncError = strToText(redact.Text(err.Error()))
		} else if hook.ID <= 0 {
			lastSyncError = strToText("GitLab did not return a webhook ID; create the webhook manually")
		} else {
			hookEnabled = true
			hookID = pgtype.Int8{Int64: hook.ID, Valid: true}
		}
	} else if h.cfg.PublicURL == "" {
		lastSyncError = strToText("MULTICA_PUBLIC_URL is not configured; create the GitLab webhook manually")
	} else {
		lastSyncError = strToText("GitLab webhook secret is unavailable; create the webhook manually")
	}

	saved, err := h.Queries.UpsertVCSGitLabProject(r.Context(), db.UpsertVCSGitLabProjectParams{
		WorkspaceID:       workspaceID,
		ConnectionID:      connection.ID,
		GitlabProjectID:   project.ID,
		PathWithNamespace: project.PathWithNamespace,
		WebUrl:            project.WebURL,
		HookID:            hookID,
		HookEnabled:       hookEnabled,
		LastSyncError:     lastSyncError,
	})
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to save GitLab project")
		return
	}
	if err := h.ensureGitLabProjectWorkspaceRepo(r.Context(), workspaceID, project, requestUserID(r)); err != nil {
		slog.Warn("gitlab: add project to workspace repos failed", "err", err, "workspace_id", uuidToString(workspaceID), "project_id", project.ID)
	}
	response := vcsGitLabProjectToResponse(saved)
	h.publish(protocol.EventGitLabProjectCreated, uuidToString(workspaceID), "system", "", map[string]any{
		"project": response,
	})
	writeJSON(w, http.StatusOK, map[string]any{
		"project":            response,
		"manual_webhook_url": webhookURL,
	})
}

func (h *Handler) DeleteVCSGitLabProject(w http.ResponseWriter, r *http.Request) {
	h.deleteVCSGitLabProject(w, r, "projectId")
}

func (h *Handler) deleteVCSGitLabProject(w http.ResponseWriter, r *http.Request, projectParam string) {
	if !h.isVCSAvailable() {
		writeError(w, http.StatusNotFound, "vcs integration is not available on this deployment")
		return
	}
	workspaceID := chi.URLParam(r, "id")
	wsUUID, ok := parseUUIDOrBadRequest(w, workspaceID, "workspace id")
	if !ok {
		return
	}
	projectID, ok := parseUUIDOrBadRequest(w, chi.URLParam(r, projectParam), "project id")
	if !ok {
		return
	}
	project, err := h.Queries.GetVCSGitLabProjectByIDAndWorkspace(r.Context(), db.GetVCSGitLabProjectByIDAndWorkspaceParams{
		ID:          projectID,
		WorkspaceID: wsUUID,
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			writeError(w, http.StatusNotFound, "GitLab project not found")
		} else {
			writeError(w, http.StatusInternalServerError, "failed to load GitLab project")
		}
		return
	}
	if project.HookID.Valid {
		connection, err := h.Queries.GetVCSGitLabConnectionByIDAndWorkspace(r.Context(), db.GetVCSGitLabConnectionByIDAndWorkspaceParams{
			ID:          project.ConnectionID,
			WorkspaceID: wsUUID,
		})
		if err != nil {
			writeError(w, http.StatusNotFound, "GitLab connection not found")
			return
		}
		cfg, err := h.gitLabConfigForVCSConnectionRow(connection)
		if err != nil {
			writeError(w, http.StatusServiceUnavailable, "GitLab credentials are unavailable")
			return
		}
		api, err := newGitLabClient(cfg)
		if err != nil {
			writeError(w, http.StatusBadGateway, redact.Text(err.Error()))
			return
		}
		if err := api.DeleteProjectHook(r.Context(), project.GitlabProjectID, project.HookID.Int64); err != nil && err.Error() != "gitlab project not found" {
			writeError(w, http.StatusBadGateway, redact.Text(err.Error()))
			return
		}
	}
	if h.TxStarter == nil {
		writeError(w, http.StatusInternalServerError, "database transactions are unavailable")
		return
	}
	tx, err := h.TxStarter.Begin(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to begin GitLab project cleanup")
		return
	}
	defer tx.Rollback(r.Context())
	q := h.Queries.WithTx(tx)
	if err := q.DeleteVCSGitLabProjectOrphanedPullRequests(r.Context(), db.DeleteVCSGitLabProjectOrphanedPullRequestsParams{
		ProjectID:   project.ID,
		WorkspaceID: wsUUID,
	}); err != nil {
		writeError(w, http.StatusInternalServerError, "failed to clean GitLab project dependencies")
		return
	}
	deleted, err := q.DeleteVCSGitLabProjectByID(r.Context(), db.DeleteVCSGitLabProjectByIDParams{
		ID:           project.ID,
		WorkspaceID:  wsUUID,
		ConnectionID: project.ConnectionID,
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			writeError(w, http.StatusNotFound, "GitLab project not found")
		} else {
			writeError(w, http.StatusInternalServerError, "failed to remove GitLab project")
		}
		return
	}
	if err := tx.Commit(r.Context()); err != nil {
		writeError(w, http.StatusInternalServerError, "failed to commit GitLab project cleanup")
		return
	}
	h.publish(protocol.EventGitLabProjectDeleted, workspaceID, "system", "", map[string]any{
		"id": uuidToString(deleted.ID),
	})
	w.WriteHeader(http.StatusNoContent)
}
