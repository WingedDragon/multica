package handler

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
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

func normalizeGitLabProjectInput(raw string, baseHost string) (string, bool) {
	s := strings.TrimSpace(raw)
	if s == "" || strings.TrimSpace(baseHost) == "" {
		return "", false
	}
	baseHost = strings.ToLower(strings.TrimSpace(baseHost))
	if strings.Contains(s, "://") {
		u, err := url.Parse(s)
		if err != nil || u.Scheme == "" || u.Host == "" {
			return "", false
		}
		if u.Scheme != "http" && u.Scheme != "https" {
			return "", false
		}
		if strings.ToLower(u.Host) != baseHost {
			return "", false
		}
		s = u.Path
	}
	s = strings.TrimSpace(s)
	s = strings.Trim(s, "/")
	s = strings.TrimSuffix(s, ".git")
	s = strings.Trim(s, "/")
	if s == "" || strings.Contains(s, "://") || strings.Contains(s, "\\") || strings.Contains(s, "?") || strings.Contains(s, "#") {
		return "", false
	}
	return s, true
}

func (h *Handler) gitLabWebhookPath(connectionID string) string {
	path := "/api/webhooks/gitlab"
	if connectionID != "" {
		path += "/" + connectionID
	}
	return path
}

func (h *Handler) gitLabWebhookURL(connectionID ...string) string {
	id := ""
	if len(connectionID) > 0 {
		id = connectionID[0]
	}
	path := h.gitLabWebhookPath(id)
	if h.cfg.PublicURL == "" {
		return path
	}
	return strings.TrimRight(h.cfg.PublicURL, "/") + path
}

func normalizeGitLabConnectionBaseURL(raw string) (string, string, bool) {
	baseURL := strings.TrimRight(strings.TrimSpace(raw), "/")
	if baseURL == "" {
		return "", "", false
	}
	u, err := url.Parse(baseURL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" ||
		u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return "", "", false
	}
	return baseURL, strings.ToLower(u.Host), true
}

func (h *Handler) gitLabConnectionToResponse(row db.GitlabConnection, includeWebhook bool) GitLabConnectionResponse {
	response := GitLabConnectionResponse{
		ID:               uuidToString(row.ID),
		WorkspaceID:      uuidToString(row.WorkspaceID),
		BaseURL:          row.BaseUrl,
		Host:             row.Host,
		ServerConfigured: gitLabConnectionServerConfigured(row),
	}
	if includeWebhook {
		if row.WebhookSecretEncrypted != "" {
			response.WebhookURL = h.gitLabWebhookURL(response.ID)
			response.WebhookPath = h.gitLabWebhookPath(response.ID)
		} else {
			response.WebhookURL = h.gitLabWebhookURL()
			response.WebhookPath = h.gitLabWebhookPath("")
		}
	}
	return response
}

func (h *Handler) GetGitLabConfig(w http.ResponseWriter, r *http.Request) {
	workspaceID := chi.URLParam(r, "id")
	wsUUID, ok := parseUUIDOrBadRequest(w, workspaceID, "workspace id")
	if !ok {
		return
	}
	cfg := gitlab.LoadConfigFromEnv()
	member, _ := ctxMember(r.Context())
	canManage := roleAllowed(member.Role, "owner", "admin")

	connections, err := h.Queries.ListGitLabConnectionsByWorkspace(r.Context(), wsUUID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to list gitlab connections")
		return
	}
	configured := cfg.Configured()
	connectionResponses := make([]GitLabConnectionResponse, 0, len(connections))
	for _, row := range connections {
		if row.AccessTokenEncrypted != "" {
			configured = true
		}
		connectionResponses = append(connectionResponses, h.gitLabConnectionToResponse(row, true))
	}
	rows, err := h.Queries.ListGitLabProjectBindingsByWorkspace(r.Context(), wsUUID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to list gitlab projects")
		return
	}
	projects := make([]GitLabProjectBindingResponse, 0, len(rows))
	for _, row := range rows {
		projects = append(projects, gitLabProjectBindingToResponse(row))
	}
	writeJSON(w, http.StatusOK, GitLabConfigResponse{
		Configured:                  configured,
		CanManage:                   canManage,
		CredentialStorageConfigured: h.VCSSecretBox != nil,
		BaseURL:                     cfg.BaseURL,
		ManualWebhookURL:            h.gitLabWebhookURL(),
		Connections:                 connectionResponses,
		Projects:                    projects,
	})
}

func (h *Handler) CreateGitLabProjectBinding(w http.ResponseWriter, r *http.Request) {
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

	cfg := gitlab.LoadConfigFromEnv()
	var connection db.GitlabConnection
	connectionRequested := req.ConnectionID != ""
	useScopedWebhook := false
	var err error
	if connectionRequested {
		connectionID, ok := parseUUIDOrBadRequest(w, req.ConnectionID, "connection id")
		if !ok {
			return
		}
		connection, err = h.Queries.GetGitLabConnectionByID(r.Context(), db.GetGitLabConnectionByIDParams{
			ID:          connectionID,
			WorkspaceID: wsUUID,
		})
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				writeError(w, http.StatusNotFound, "gitlab connection not found")
				return
			}
			writeError(w, http.StatusInternalServerError, "failed to load gitlab connection")
			return
		}
		cfg, err = h.gitLabConfigForConnectionRow(connection)
		if err != nil {
			writeError(w, http.StatusServiceUnavailable, "gitlab connection credentials are unavailable")
			return
		}
		useScopedWebhook = connection.WebhookSecretEncrypted != ""
	} else if !cfg.Configured() {
		writeError(w, http.StatusServiceUnavailable, "gitlab integration is not configured")
		return
	}
	projectPath, ok := normalizeGitLabProjectInput(req.Project, cfg.Host)
	if !ok {
		writeError(w, http.StatusBadRequest, "invalid gitlab project")
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

	if !connection.ID.Valid {
		connectedBy := pgtype.UUID{}
		if member, ok := ctxMember(r.Context()); ok {
			connectedBy = member.UserID
		} else if userID := requestUserID(r); userID != "" {
			if u, err := parseStrictUUID(userID); err == nil {
				connectedBy = u
			}
		}
		connection, err = h.Queries.UpsertGitLabConnection(r.Context(), db.UpsertGitLabConnectionParams{
			WorkspaceID:   wsUUID,
			BaseUrl:       cfg.BaseURL,
			Host:          cfg.Host,
			ConnectedByID: connectedBy,
		})
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				writeError(w, http.StatusConflict, "a GitLab connection already exists for this host with a different base URL")
			} else {
				writeError(w, http.StatusInternalServerError, "failed to save gitlab connection")
			}
			return
		}
	}

	webhookURL := h.gitLabWebhookURL()
	if useScopedWebhook {
		webhookURL = h.gitLabWebhookURL(uuidToString(connection.ID))
	}
	hookEnabled := false
	hookID := pgtype.Int8{}
	lastSyncError := pgtype.Text{}
	if existing, ok := h.findExistingGitLabProjectBinding(r.Context(), wsUUID, connection.ID, project.ID); ok && existing.HookEnabled && existing.HookID.Valid {
		hookEnabled = true
		hookID = existing.HookID
		lastSyncError = existing.LastSyncError
	} else if h.cfg.PublicURL != "" && cfg.WebhookSecret != "" {
		hook, err := api.CreateProjectHook(r.Context(), project.ID, webhookURL, cfg.WebhookSecret)
		if err != nil {
			lastSyncError = strToText(redact.Text(err.Error()))
		} else {
			hookEnabled = true
			hookID = pgtype.Int8{Int64: hook.ID, Valid: true}
			lastSyncError = pgtype.Text{}
		}
	} else {
		message := "MULTICA_PUBLIC_URL is not configured; create the GitLab webhook manually"
		if cfg.WebhookSecret == "" {
			message = "GitLab webhook secret is unavailable; create the GitLab webhook manually"
		}
		lastSyncError = pgtype.Text{String: message, Valid: true}
	}
	binding, err := h.Queries.UpsertGitLabProjectBinding(r.Context(), db.UpsertGitLabProjectBindingParams{
		WorkspaceID:       wsUUID,
		ConnectionID:      connection.ID,
		GitlabProjectID:   project.ID,
		PathWithNamespace: project.PathWithNamespace,
		WebUrl:            project.WebURL,
		HookID:            hookID,
		HookEnabled:       hookEnabled,
		LastSyncError:     lastSyncError,
	})
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to save gitlab project")
		return
	}
	if err := h.ensureGitLabProjectWorkspaceRepo(r.Context(), wsUUID, project, requestUserID(r)); err != nil {
		slog.Warn("gitlab: add project to workspace repos failed", "err", err, "workspace_id", workspaceID, "project_id", project.ID)
	}
	resp := gitLabProjectBindingToResponse(binding)
	h.publish(protocol.EventGitLabProjectCreated, workspaceID, "system", "", map[string]any{
		"project": resp,
	})
	writeJSON(w, http.StatusOK, map[string]any{
		"project":            resp,
		"manual_webhook_url": webhookURL,
	})
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

func (h *Handler) findExistingGitLabProjectBinding(ctx context.Context, workspaceID, connectionID pgtype.UUID, projectID int64) (db.GitlabProjectBinding, bool) {
	rows, err := h.Queries.ListGitLabProjectBindingsByWorkspace(ctx, workspaceID)
	if err != nil {
		slog.Warn("gitlab: list existing project bindings failed", "err", err, "workspace_id", uuidToString(workspaceID))
		return db.GitlabProjectBinding{}, false
	}
	for _, row := range rows {
		if row.ConnectionID == connectionID && row.GitlabProjectID == projectID {
			return row, true
		}
	}
	return db.GitlabProjectBinding{}, false
}

func (h *Handler) DeleteGitLabProjectBinding(w http.ResponseWriter, r *http.Request) {
	workspaceID := chi.URLParam(r, "id")
	wsUUID, ok := parseUUIDOrBadRequest(w, workspaceID, "workspace id")
	if !ok {
		return
	}
	bindingID := chi.URLParam(r, "bindingId")
	bindingUUID, ok := parseUUIDOrBadRequest(w, bindingID, "binding id")
	if !ok {
		return
	}
	binding, err := h.Queries.GetGitLabProjectBinding(r.Context(), db.GetGitLabProjectBindingParams{
		ID:          bindingUUID,
		WorkspaceID: wsUUID,
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			writeError(w, http.StatusNotFound, "gitlab project not found")
			return
		}
		writeError(w, http.StatusInternalServerError, "failed to remove gitlab project")
		return
	}
	if binding.HookID.Valid {
		cfg, err := h.gitLabConfigForConnection(r.Context(), wsUUID, binding.ConnectionID)
		if err != nil {
			writeError(w, http.StatusServiceUnavailable, "gitlab credentials are unavailable")
			return
		}
		api, err := newGitLabClient(cfg)
		if err != nil {
			writeError(w, http.StatusBadGateway, redact.Text(err.Error()))
			return
		}
		if err := api.DeleteProjectHook(r.Context(), binding.GitlabProjectID, binding.HookID.Int64); err != nil {
			if err.Error() != "gitlab project not found" {
				writeError(w, http.StatusBadGateway, redact.Text(err.Error()))
				return
			}
		}
	}
	deleted, err := h.Queries.DeleteGitLabProjectBinding(r.Context(), db.DeleteGitLabProjectBindingParams{
		ID:          bindingUUID,
		WorkspaceID: wsUUID,
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			writeError(w, http.StatusNotFound, "gitlab project not found")
			return
		}
		writeError(w, http.StatusInternalServerError, "failed to remove gitlab project")
		return
	}
	h.publish(protocol.EventGitLabProjectDeleted, workspaceID, "system", "", map[string]any{
		"id": uuidToString(deleted.ID),
	})
	w.WriteHeader(http.StatusNoContent)
}

func (h *Handler) HandleGitLabWebhook(w http.ResponseWriter, r *http.Request) {
	connectionID := pgtype.UUID{}
	cfg := gitlab.LoadConfigFromEnv()
	if rawConnectionID := chi.URLParam(r, "connectionId"); rawConnectionID != "" {
		var ok bool
		connectionID, ok = parseUUIDOrBadRequest(w, rawConnectionID, "connection id")
		if !ok {
			return
		}
		_, webhookConfig, err := h.gitLabWebhookConnectionConfig(r.Context(), connectionID)
		if err != nil {
			if gitLabConnectionNotFound(err) || h.VCSSecretBox == nil {
				writeError(w, http.StatusNotFound, "unknown GitLab connection")
			} else {
				writeError(w, http.StatusInternalServerError, "GitLab webhook credentials are unavailable")
			}
			return
		}
		cfg = webhookConfig
	} else if cfg.WebhookSecret == "" {
		writeError(w, http.StatusServiceUnavailable, "gitlab webhooks not configured")
		return
	}
	token := r.Header.Get("X-Gitlab-Token")
	if subtle.ConstantTimeCompare([]byte(token), []byte(cfg.WebhookSecret)) != 1 {
		writeError(w, http.StatusUnauthorized, "invalid gitlab token")
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 10<<20))
	if err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			writeError(w, http.StatusRequestEntityTooLarge, "gitlab payload too large")
			return
		}
		writeError(w, http.StatusBadRequest, "read body failed")
		return
	}
	switch r.Header.Get("X-Gitlab-Event") {
	case "Merge Request Hook":
		valid, err := h.handleGitLabMergeRequestEvent(r.Context(), cfg, body, connectionID)
		if err != nil {
			slog.Warn("gitlab: merge request webhook persistence failed", "reason", gitLabWakeupFailureMessage(err))
			writeError(w, http.StatusServiceUnavailable, "merge request sync failed")
			return
		}
		if !valid {
			writeError(w, http.StatusBadRequest, "invalid merge request payload")
			return
		}
		w.WriteHeader(http.StatusAccepted)
	case "Pipeline Hook":
		valid, err := h.handleGitLabPipelineEvent(r.Context(), cfg, body, connectionID)
		if err != nil {
			slog.Warn("gitlab: pipeline webhook refresh failed", "reason", gitLabWakeupFailureMessage(err))
			writeError(w, http.StatusServiceUnavailable, "pipeline sync failed")
			return
		}
		if !valid {
			writeError(w, http.StatusBadRequest, "invalid pipeline payload")
			return
		}
		w.WriteHeader(http.StatusAccepted)
	case "System Hook":
		var event struct {
			ObjectKind string `json:"object_kind"`
		}
		if err := json.Unmarshal(body, &event); err != nil {
			writeError(w, http.StatusBadRequest, "invalid system hook payload")
			return
		}
		switch event.ObjectKind {
		case "merge_request":
			valid, err := h.handleGitLabMergeRequestEvent(r.Context(), cfg, body, connectionID)
			if err != nil {
				slog.Warn("gitlab: merge request system hook persistence failed", "reason", gitLabWakeupFailureMessage(err))
				writeError(w, http.StatusServiceUnavailable, "merge request sync failed")
				return
			}
			if !valid {
				writeError(w, http.StatusBadRequest, "invalid merge request payload")
				return
			}
			w.WriteHeader(http.StatusAccepted)
		case "pipeline":
			valid, err := h.handleGitLabPipelineEvent(r.Context(), cfg, body, connectionID)
			if err != nil {
				slog.Warn("gitlab: pipeline system hook refresh failed", "reason", gitLabWakeupFailureMessage(err))
				writeError(w, http.StatusServiceUnavailable, "pipeline sync failed")
				return
			}
			if !valid {
				writeError(w, http.StatusBadRequest, "invalid pipeline payload")
				return
			}
			w.WriteHeader(http.StatusAccepted)
		default:
			w.WriteHeader(http.StatusNoContent)
		}
	case "Job Hook":
		if !h.handleGitLabJobEvent(r.Context(), cfg, body, connectionID) {
			writeError(w, http.StatusBadRequest, "invalid job payload")
			return
		}
		w.WriteHeader(http.StatusAccepted)
	case "Note Hook":
		if !h.handleGitLabNoteEvent(r.Context(), cfg, body, connectionID) {
			writeError(w, http.StatusBadRequest, "invalid note payload")
			return
		}
		w.WriteHeader(http.StatusAccepted)
	default:
		w.WriteHeader(http.StatusNoContent)
	}
}

type gitLabMRWebhookPayload struct {
	ObjectKind string `json:"object_kind"`
	User       struct {
		Username  string `json:"username"`
		AvatarURL string `json:"avatar_url"`
	} `json:"user"`
	Project struct {
		ID                int64  `json:"id"`
		PathWithNamespace string `json:"path_with_namespace"`
		WebURL            string `json:"web_url"`
	} `json:"project"`
	ObjectAttributes struct {
		IID             int32  `json:"iid"`
		Title           string `json:"title"`
		Description     string `json:"description"`
		State           string `json:"state"`
		Draft           bool   `json:"draft"`
		URL             string `json:"url"`
		SourceBranch    string `json:"source_branch"`
		SourceProjectID int64  `json:"source_project_id"`
		SourceProject   struct {
			ID                int64  `json:"id"`
			PathWithNamespace string `json:"path_with_namespace"`
		} `json:"source"`
		TargetBranch string `json:"target_branch"`
		LastCommit   struct {
			ID string `json:"id"`
		} `json:"last_commit"`
		MergeCommitSHA      string `json:"merge_commit_sha"`
		DetailedMergeStatus string `json:"detailed_merge_status"`
		HasConflicts        *bool  `json:"has_conflicts"`
		CreatedAt           string `json:"created_at"`
		UpdatedAt           string `json:"updated_at"`
		MergedAt            string `json:"merged_at"`
		ClosedAt            string `json:"closed_at"`
		Action              string `json:"action"`
	} `json:"object_attributes"`
	Changes struct {
		Total struct {
			Previous string `json:"previous"`
			Current  string `json:"current"`
		} `json:"total"`
	} `json:"changes"`
}

func (h *Handler) gitLabProjectBindingsForEvent(ctx context.Context, cfg gitlab.Config, connectionID pgtype.UUID, projectID int64) ([]db.GitlabProjectBinding, error) {
	if connectionID.Valid {
		return h.Queries.ListGitLabProjectBindingsByConnectionAndProjectID(ctx, db.ListGitLabProjectBindingsByConnectionAndProjectIDParams{
			ConnectionID:    connectionID,
			GitlabProjectID: projectID,
		})
	}
	return h.Queries.ListGitLabProjectBindingsByBaseURLAndProjectID(ctx, db.ListGitLabProjectBindingsByBaseURLAndProjectIDParams{
		BaseUrl:         cfg.BaseURL,
		GitlabProjectID: projectID,
	})
}

func (h *Handler) handleGitLabMergeRequestEvent(ctx context.Context, cfg gitlab.Config, body []byte, connectionID pgtype.UUID) (bool, error) {
	var p gitLabMRWebhookPayload
	if err := json.Unmarshal(body, &p); err != nil || p.Project.ID <= 0 || p.ObjectAttributes.IID <= 0 {
		return false, nil
	}
	bindings, err := h.gitLabProjectBindingsForEvent(ctx, cfg, connectionID, p.Project.ID)
	if err != nil {
		slog.Warn("gitlab: lookup project bindings failed", "err", err, "project_id", p.Project.ID)
		return false, err
	}
	if len(bindings) == 0 {
		return true, nil
	}

	for _, binding := range bindings {
		if err := h.Queries.MarkGitLabProjectBindingEvent(ctx, db.MarkGitLabProjectBindingEventParams{
			ID:            binding.ID,
			WorkspaceID:   binding.WorkspaceID,
			LastEventType: strToText("merge_request"),
		}); err != nil {
			return false, err
		}
		attrs := p.ObjectAttributes
		state := deriveGitLabMRState(attrs.State, attrs.Draft)

		sourceProjectID := attrs.SourceProjectID
		if sourceProjectID == 0 {
			sourceProjectID = attrs.SourceProject.ID
		}
		sourceProjectPath := attrs.SourceProject.PathWithNamespace
		conflictState := gitlab.MergeRequestConflictState(attrs.HasConflicts, attrs.DetailedMergeStatus)
		conflictSHA := ""
		if conflictState != nil && attrs.LastCommit.ID != "" {
			conflictSHA = attrs.LastCommit.ID
		} else {
			conflictState = nil
		}
		sourceProjectIDValue := pgtype.Int8{}
		if sourceProjectID > 0 {
			sourceProjectIDValue = pgtype.Int8{Int64: sourceProjectID, Valid: true}
		}
		webURL := attrs.URL
		if webURL == "" && p.Project.WebURL != "" && attrs.IID != 0 {
			webURL = strings.TrimRight(p.Project.WebURL, "/") + "/-/merge_requests/" + strconv.FormatInt(int64(attrs.IID), 10)
		}
		mr, err := h.Queries.UpsertGitLabMergeRequest(ctx, db.UpsertGitLabMergeRequestParams{
			WorkspaceID:         binding.WorkspaceID,
			ProjectBindingID:    binding.ID,
			GitlabProjectID:     p.Project.ID,
			MrIid:               attrs.IID,
			Title:               attrs.Title,
			State:               state,
			WebUrl:              webURL,
			Sha:                 attrs.LastCommit.ID,
			Additions:           0,
			Deletions:           0,
			ChangedFiles:        0,
			MrCreatedAt:         parseGitLabTimeRequired(attrs.CreatedAt),
			MrUpdatedAt:         parseGitLabTimeRequired(attrs.UpdatedAt),
			Description:         strToText(attrs.Description),
			SourceBranch:        strToText(attrs.SourceBranch),
			TargetBranch:        strToText(attrs.TargetBranch),
			AuthorUsername:      strToText(p.User.Username),
			AuthorAvatarUrl:     strToText(p.User.AvatarURL),
			MergeCommitSha:      strToText(attrs.MergeCommitSHA),
			DetailedMergeStatus: strToText(attrs.DetailedMergeStatus),
			HasConflicts:        boolToPg(conflictState),
			ConflictSha:         strToText(conflictSHA),
			SourceProjectID:     sourceProjectIDValue,
			SourceProjectPath:   strToText(sourceProjectPath),
			MergedAt:            parseGitLabTime(attrs.MergedAt),
			ClosedAt:            parseGitLabTime(attrs.ClosedAt),
		})
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				continue // stale delivery; keep the newer mirror and links intact
			}
			slog.Warn("gitlab: upsert merge request failed", "err", err, "workspace_id", uuidToString(binding.WorkspaceID), "project_id", p.Project.ID, "iid", attrs.IID)
			return false, err
		}

		linkedIssueIDs := make([]string, 0)
		reevalIssues := make([]db.Issue, 0)
		idents := extractIdentifiers(attrs.Title, attrs.Description, attrs.SourceBranch)
		closingIdents := map[string]struct{}{}
		for _, c := range extractClosingIdentifiers(attrs.Title, attrs.Description) {
			closingIdents[c] = struct{}{}
		}
		preserveCloseIntent := attrs.Action != "merge" && (state == "merged" || state == "closed")
		prefix := h.getIssuePrefix(ctx, binding.WorkspaceID)
		for _, ident := range idents {
			issue, ok := h.lookupIssueByIdentifier(ctx, binding.WorkspaceID, prefix, ident)
			if !ok {
				continue
			}
			_, declared := closingIdents[ident]
			if err := h.Queries.LinkIssueToGitLabMergeRequest(ctx, db.LinkIssueToGitLabMergeRequestParams{
				WorkspaceID:         binding.WorkspaceID,
				IssueID:             issue.ID,
				MergeRequestID:      mr.ID,
				CloseIntent:         declared && !preserveCloseIntent,
				LinkedByType:        strToText("system"),
				LinkedByID:          pgtype.UUID{},
				PreserveCloseIntent: preserveCloseIntent,
			}); err != nil {
				slog.Warn("gitlab: link merge request failed", "err", err, "issue_id", uuidToString(issue.ID), "mr_id", uuidToString(mr.ID))
				return false, err
			}
			linkedIssueIDs = append(linkedIssueIDs, uuidToString(issue.ID))
			reevalIssues = append(reevalIssues, issue)
		}
		if state == "merged" || state == "closed" {
			for _, issue := range reevalIssues {
				if issue.Status == "done" || issue.Status == "cancelled" {
					continue
				}
				counts, err := h.Queries.GetIssueGitLabMRCloseAggregate(ctx, db.GetIssueGitLabMRCloseAggregateParams{
					WorkspaceID: binding.WorkspaceID,
					IssueID:     issue.ID,
				})
				if err != nil {
					slog.Warn("gitlab: count linked mr states failed", "err", err, "issue_id", uuidToString(issue.ID))
					return false, err
				}
				if counts.OpenCount == 0 && counts.MergedWithCloseIntentCount > 0 {
					h.advanceIssueToDoneWithSource(ctx, issue, uuidToString(binding.WorkspaceID), "gitlab_mr_merged")
				}
			}
		}
		if err := h.refreshGitLabMergeRequestFromWebhook(ctx, cfg, mr); err != nil {
			if isGitLabRefreshPersistenceError(err) {
				return false, err
			}
			slog.Warn("gitlab: merge request refresh failed", "reason", gitLabWakeupFailureMessage(err), "mr_id", uuidToString(mr.ID))
		}
		h.publish(protocol.EventGitLabMergeRequestUpdated, uuidToString(binding.WorkspaceID), "system", "", map[string]any{
			"merge_request_id": uuidToString(mr.ID),
			"linked_issue_ids": linkedIssueIDs,
		})
	}
	return true, nil
}

type gitLabPipelineWebhookPayload struct {
	ObjectKind       string `json:"object_kind"`
	ObjectAttributes struct {
		ID         int64  `json:"id"`
		SHA        string `json:"sha"`
		Ref        string `json:"ref"`
		Status     string `json:"status"`
		URL        string `json:"url"`
		UpdatedAt  string `json:"updated_at"`
		FinishedAt string `json:"finished_at"`
		CreatedAt  string `json:"created_at"`
	} `json:"object_attributes"`
	Project struct {
		ID int64 `json:"id"`
	} `json:"project"`
}

func (h *Handler) handleGitLabPipelineEvent(ctx context.Context, cfg gitlab.Config, body []byte, connectionID pgtype.UUID) (bool, error) {
	var p gitLabPipelineWebhookPayload
	if err := json.Unmarshal(body, &p); err != nil || p.Project.ID <= 0 || p.ObjectAttributes.ID <= 0 || p.ObjectAttributes.SHA == "" {
		return false, nil
	}
	var pipelineUpdatedAt pgtype.Timestamptz
	for _, timestamp := range []string{p.ObjectAttributes.UpdatedAt, p.ObjectAttributes.FinishedAt, p.ObjectAttributes.CreatedAt} {
		pipelineUpdatedAt = parseGitLabTime(timestamp)
		if pipelineUpdatedAt.Valid {
			break
		}
	}
	if !pipelineUpdatedAt.Valid {
		return false, nil
	}
	bindings, err := h.gitLabProjectBindingsForEvent(ctx, cfg, connectionID, p.Project.ID)
	if err != nil {
		slog.Warn("gitlab: lookup project bindings for pipeline failed", "err", err, "project_id", p.Project.ID)
		return false, err
	}
	candidates := make(map[string]db.GitlabMergeRequest)
	addCandidate := func(mr db.GitlabMergeRequest) {
		if gitLabPipelineRefMayMatchMR(p.ObjectAttributes.Ref, mr) {
			candidates[uuidToString(mr.ID)] = mr
		}
	}
	for _, binding := range bindings {
		if err := h.Queries.MarkGitLabProjectBindingEvent(ctx, db.MarkGitLabProjectBindingEventParams{
			ID:            binding.ID,
			WorkspaceID:   binding.WorkspaceID,
			LastEventType: strToText("pipeline"),
		}); err != nil {
			return false, err
		}
		mrs, err := h.Queries.ListGitLabMergeRequestsByProjectBindingSHA(ctx, db.ListGitLabMergeRequestsByProjectBindingSHAParams{
			WorkspaceID:      binding.WorkspaceID,
			ProjectBindingID: binding.ID,
			Sha:              p.ObjectAttributes.SHA,
		})
		if err != nil {
			return false, err
		}
		for _, mr := range mrs {
			addCandidate(mr)
		}
	}
	var sourceMRs []db.GitlabMergeRequest
	if connectionID.Valid {
		sourceMRs, err = h.Queries.ListGitLabMergeRequestsBySourceProjectHeadForConnection(ctx, db.ListGitLabMergeRequestsBySourceProjectHeadForConnectionParams{
			ConnectionID:    connectionID,
			SourceProjectID: pgtype.Int8{Int64: p.Project.ID, Valid: p.Project.ID > 0},
			Sha:             p.ObjectAttributes.SHA,
		})
	} else {
		sourceMRs, err = h.Queries.ListGitLabMergeRequestsBySourceProjectHeadForBaseURL(ctx, db.ListGitLabMergeRequestsBySourceProjectHeadForBaseURLParams{
			BaseUrl:         cfg.BaseURL,
			SourceProjectID: pgtype.Int8{Int64: p.Project.ID, Valid: p.Project.ID > 0},
			Sha:             p.ObjectAttributes.SHA,
		})
	}
	if err != nil {
		return false, err
	}
	for _, mr := range sourceMRs {
		addCandidate(mr)
	}
	var persistenceErr error
	for _, mr := range candidates {
		refreshErr := h.refreshGitLabMergeRequestFromWebhook(ctx, cfg, mr)
		if refreshErr != nil {
			slog.Warn("gitlab: pipeline merge request refresh failed", "reason", gitLabWakeupFailureMessage(refreshErr), "mr_id", uuidToString(mr.ID))
		}
		if isGitLabRefreshPersistenceError(refreshErr) {
			persistenceErr = errors.Join(persistenceErr, refreshErr)
		}
		currentMR, err := h.Queries.GetGitLabMergeRequestByID(ctx, db.GetGitLabMergeRequestByIDParams{
			ID:          mr.ID,
			WorkspaceID: mr.WorkspaceID,
		})
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				continue
			}
			return false, err
		}
		if currentMR.Sha != p.ObjectAttributes.SHA ||
			(p.Project.ID != currentMR.GitlabProjectID && (!currentMR.SourceProjectID.Valid || p.Project.ID != currentMR.SourceProjectID.Int64)) ||
			!gitLabPipelineRefMayMatchMR(p.ObjectAttributes.Ref, currentMR) {
			continue
		}
		if err := h.Queries.UpsertGitLabMRPipeline(ctx, db.UpsertGitLabMRPipelineParams{
			MergeRequestID:       currentMR.ID,
			WorkspaceID:          currentMR.WorkspaceID,
			WakeupSyncGeneration: currentMR.WakeupSyncGeneration,
			PipelineID:           p.ObjectAttributes.ID,
			PipelineProjectID:    p.Project.ID,
			Sha:                  p.ObjectAttributes.SHA,
			Ref:                  strToText(p.ObjectAttributes.Ref),
			Status:               normalizeGitLabPipelineStatus(p.ObjectAttributes.Status),
			WebUrl:               strToText(p.ObjectAttributes.URL),
			PipelineUpdatedAt:    pipelineUpdatedAt,
		}); err != nil {
			return false, wrapGitLabRefreshPersistenceError(err)
		}
		issueIDs, err := h.Queries.ListIssueIDsForGitLabMergeRequest(ctx, db.ListIssueIDsForGitLabMergeRequestParams{
			WorkspaceID:    currentMR.WorkspaceID,
			MergeRequestID: currentMR.ID,
		})
		if err != nil {
			return false, err
		}
		linkedIssueIDs := make([]string, 0, len(issueIDs))
		for _, id := range issueIDs {
			linkedIssueIDs = append(linkedIssueIDs, uuidToString(id))
		}
		h.publish(protocol.EventGitLabMergeRequestUpdated, uuidToString(currentMR.WorkspaceID), "system", "", map[string]any{
			"merge_request_id": uuidToString(currentMR.ID),
			"linked_issue_ids": linkedIssueIDs,
		})
	}
	if persistenceErr != nil {
		return false, persistenceErr
	}
	return true, nil
}

func gitLabPipelineRefMayMatchMR(ref string, mr db.GitlabMergeRequest) bool {
	if ref == "" || (mr.SourceBranch.Valid && ref == mr.SourceBranch.String) {
		return true
	}
	parts := strings.Split(ref, "/")
	if len(parts) >= 4 && parts[0] == "refs" && parts[1] == "merge-requests" {
		iid, err := strconv.ParseInt(parts[2], 10, 32)
		return err == nil && int32(iid) == mr.MrIid
	}
	return true
}

type gitLabJobWebhookPayload struct {
	ObjectKind          string                            `json:"object_kind"`
	ProjectID           int64                             `json:"project_id"`
	PipelineID          int64                             `json:"pipeline_id"`
	BuildID             int64                             `json:"build_id"`
	BuildName           string                            `json:"build_name"`
	BuildStage          string                            `json:"build_stage"`
	BuildStatus         string                            `json:"build_status"`
	BuildRef            string                            `json:"build_ref"`
	BuildSHA            string                            `json:"build_sha"`
	BuildWebURL         string                            `json:"build_web_url"`
	BuildStartedAt      string                            `json:"build_started_at"`
	BuildFinishedAt     string                            `json:"build_finished_at"`
	BuildDuration       *float64                          `json:"build_duration"`
	BuildQueuedDuration *float64                          `json:"build_queued_duration"`
	BuildFailureReason  string                            `json:"build_failure_reason"`
	BuildAllowFailure   bool                              `json:"build_allow_failure"`
	ArtifactsFile       *gitLabWebhookArtifactFilePayload `json:"artifacts_file"`
	ArtifactsExpireAt   string                            `json:"artifacts_expire_at"`
	Project             struct {
		ID int64 `json:"id"`
	} `json:"project"`
}

type gitLabWebhookArtifactFilePayload struct {
	Filename string `json:"filename"`
	Size     int64  `json:"size"`
}

func (h *Handler) handleGitLabJobEvent(ctx context.Context, cfg gitlab.Config, body []byte, connectionID pgtype.UUID) bool {
	var p gitLabJobWebhookPayload
	if err := json.Unmarshal(body, &p); err != nil {
		return false
	}
	projectID := p.ProjectID
	if projectID == 0 {
		projectID = p.Project.ID
	}
	if projectID == 0 || p.BuildID == 0 || p.BuildSHA == "" {
		return false
	}
	bindings, err := h.gitLabProjectBindingsForEvent(ctx, cfg, connectionID, projectID)
	if err != nil {
		slog.Warn("gitlab: lookup project bindings for job failed", "err", err, "project_id", projectID)
		return true
	}
	for _, binding := range bindings {
		_ = h.Queries.MarkGitLabProjectBindingEvent(ctx, db.MarkGitLabProjectBindingEventParams{
			ID:            binding.ID,
			WorkspaceID:   binding.WorkspaceID,
			LastEventType: strToText("job"),
		})
		mrs, err := h.Queries.ListGitLabMergeRequestsByProjectBindingSHA(ctx, db.ListGitLabMergeRequestsByProjectBindingSHAParams{
			WorkspaceID:      binding.WorkspaceID,
			ProjectBindingID: binding.ID,
			Sha:              p.BuildSHA,
		})
		if err != nil {
			slog.Warn("gitlab: lookup merge requests for job failed", "err", err, "workspace_id", uuidToString(binding.WorkspaceID), "sha", p.BuildSHA)
			continue
		}
		linkedIssueSet := map[string]struct{}{}
		for _, mr := range mrs {
			if p.PipelineID != 0 {
				if err := h.Queries.UpsertGitLabMRPipeline(ctx, db.UpsertGitLabMRPipelineParams{
					MergeRequestID:       mr.ID,
					WorkspaceID:          mr.WorkspaceID,
					WakeupSyncGeneration: mr.WakeupSyncGeneration,
					PipelineID:           p.PipelineID,
					PipelineProjectID:    projectID,
					Sha:                  p.BuildSHA,
					Status:               normalizeGitLabPipelineStatus(p.BuildStatus),
					PipelineUpdatedAt:    parseGitLabTimeRequired(firstNonEmpty(p.BuildFinishedAt, p.BuildStartedAt)),
					Ref:                  strToText(p.BuildRef),
					WebUrl:               pgtype.Text{},
				}); err != nil {
					slog.Warn("gitlab: upsert job pipeline failed", "err", err, "mr_id", uuidToString(mr.ID), "pipeline_id", p.PipelineID)
				}
			}
			if _, err := h.Queries.UpsertGitLabPipelineJob(ctx, gitLabPipelineJobParams(mr, p.PipelineID, gitlab.Job{
				ID:                p.BuildID,
				Name:              p.BuildName,
				Stage:             p.BuildStage,
				Status:            p.BuildStatus,
				Ref:               p.BuildRef,
				SHA:               p.BuildSHA,
				WebURL:            p.BuildWebURL,
				StartedAt:         p.BuildStartedAt,
				FinishedAt:        p.BuildFinishedAt,
				Duration:          p.BuildDuration,
				QueuedDuration:    p.BuildQueuedDuration,
				FailureReason:     p.BuildFailureReason,
				AllowFailure:      p.BuildAllowFailure,
				ArtifactsFile:     gitLabWebhookArtifactFile(p.ArtifactsFile),
				ArtifactsExpireAt: p.ArtifactsExpireAt,
			})); err != nil {
				slog.Warn("gitlab: upsert pipeline job failed", "err", err, "mr_id", uuidToString(mr.ID), "job_id", p.BuildID)
				continue
			}
			issueIDs, err := h.Queries.ListIssueIDsForGitLabMergeRequest(ctx, db.ListIssueIDsForGitLabMergeRequestParams{
				WorkspaceID:    binding.WorkspaceID,
				MergeRequestID: mr.ID,
			})
			if err != nil {
				slog.Warn("gitlab: list linked issues for job failed", "err", err, "mr_id", uuidToString(mr.ID))
				continue
			}
			for _, id := range issueIDs {
				linkedIssueSet[uuidToString(id)] = struct{}{}
			}
		}
		if len(mrs) > 0 {
			h.publish(protocol.EventGitLabMergeRequestUpdated, uuidToString(binding.WorkspaceID), "system", "", map[string]any{
				"linked_issue_ids": stringSetKeys(linkedIssueSet),
			})
		}
	}
	return true
}

type gitLabNoteWebhookPayload struct {
	ObjectKind string `json:"object_kind"`
	Project    struct {
		ID int64 `json:"id"`
	} `json:"project"`
	MergeRequest struct {
		IID int32 `json:"iid"`
	} `json:"merge_request"`
	ObjectAttributes struct {
		NoteableType string `json:"noteable_type"`
		NoteableIID  int32  `json:"noteable_iid"`
	} `json:"object_attributes"`
}

func (h *Handler) handleGitLabNoteEvent(ctx context.Context, cfg gitlab.Config, body []byte, connectionID pgtype.UUID) bool {
	var p gitLabNoteWebhookPayload
	if err := json.Unmarshal(body, &p); err != nil {
		return false
	}
	if !strings.EqualFold(p.ObjectAttributes.NoteableType, "MergeRequest") {
		return true
	}
	iid := p.MergeRequest.IID
	if iid == 0 {
		iid = p.ObjectAttributes.NoteableIID
	}
	if p.Project.ID == 0 || iid == 0 {
		return false
	}
	api, err := newGitLabClient(cfg)
	if err != nil {
		slog.Warn("gitlab: create client for note refresh failed", "err", err)
		return true
	}
	detailAPI, err := detailGitLabAPI(api)
	if err != nil {
		slog.Warn("gitlab: detail client for note refresh unavailable", "err", err)
		return true
	}
	bindings, err := h.gitLabProjectBindingsForEvent(ctx, cfg, connectionID, p.Project.ID)
	if err != nil {
		slog.Warn("gitlab: lookup project bindings for note failed", "err", err, "project_id", p.Project.ID)
		return true
	}
	for _, binding := range bindings {
		_ = h.Queries.MarkGitLabProjectBindingEvent(ctx, db.MarkGitLabProjectBindingEventParams{
			ID:            binding.ID,
			WorkspaceID:   binding.WorkspaceID,
			LastEventType: strToText("note"),
		})
		mr, err := h.Queries.GetGitLabMergeRequestByBinding(ctx, db.GetGitLabMergeRequestByBindingParams{
			WorkspaceID:      binding.WorkspaceID,
			ProjectBindingID: binding.ID,
			MrIid:            iid,
		})
		if err != nil {
			continue
		}
		if _, err := h.refreshGitLabDiscussions(ctx, detailAPI, mr); err != nil {
			h.markGitLabMergeRequestRefreshError(ctx, mr, err)
			slog.Warn("gitlab: note discussion refresh failed", "reason", redact.Text(err.Error()), "mr_id", uuidToString(mr.ID))
			continue
		}
		issueIDs, err := h.Queries.ListIssueIDsForGitLabMergeRequest(ctx, db.ListIssueIDsForGitLabMergeRequestParams{
			WorkspaceID:    binding.WorkspaceID,
			MergeRequestID: mr.ID,
		})
		if err != nil {
			continue
		}
		linkedIssueIDs := make([]string, 0, len(issueIDs))
		for _, id := range issueIDs {
			linkedIssueIDs = append(linkedIssueIDs, uuidToString(id))
		}
		h.publish(protocol.EventGitLabMergeRequestUpdated, uuidToString(binding.WorkspaceID), "system", "", map[string]any{
			"merge_request_id": uuidToString(mr.ID),
			"linked_issue_ids": linkedIssueIDs,
		})
	}
	return true
}

func (h *Handler) ListGitLabMergeRequestsForIssue(w http.ResponseWriter, r *http.Request) {
	issue, ok := h.loadIssueForUser(w, r, chi.URLParam(r, "id"))
	if !ok {
		return
	}
	rows, err := h.Queries.ListGitLabMergeRequestsByIssue(r.Context(), db.ListGitLabMergeRequestsByIssueParams{
		WorkspaceID: issue.WorkspaceID,
		IssueID:     issue.ID,
	})
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to list gitlab merge requests")
		return
	}
	out := make([]GitLabMergeRequestResponse, 0, len(rows))
	for _, row := range rows {
		out = append(out, gitLabMergeRequestRowToResponse(row))
	}
	writeJSON(w, http.StatusOK, map[string]any{"merge_requests": out})
}

func gitLabMergeRequestRowToResponse(row db.ListGitLabMergeRequestsByIssueRow) GitLabMergeRequestResponse {
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
		PipelineStatus:      textToPtr(row.PipelineStatus),
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

func stringSetKeys(set map[string]struct{}) []string {
	out := make([]string, 0, len(set))
	for key := range set {
		out = append(out, key)
	}
	return out
}

func gitLabWebhookArtifactFile(file *gitLabWebhookArtifactFilePayload) *gitlab.JobArtifactFile {
	if file == nil || file.Filename == "" {
		return nil
	}
	return &gitlab.JobArtifactFile{Filename: file.Filename, Size: file.Size}
}
