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
	CreateProjectHook(context.Context, int64, string, string) (gitlab.ProjectHook, error)
	DeleteProjectHook(context.Context, int64, int64) error
}

var newGitLabClient = func(cfg gitlab.Config) (gitLabAPI, error) {
	return gitlab.NewClient(cfg)
}

type GitLabProjectBindingResponse struct {
	ID                string  `json:"id"`
	WorkspaceID       string  `json:"workspace_id"`
	GitLabProjectID   int64   `json:"gitlab_project_id"`
	PathWithNamespace string  `json:"path_with_namespace"`
	WebURL            string  `json:"web_url"`
	HookID            *int64  `json:"hook_id,omitempty"`
	HookEnabled       bool    `json:"hook_enabled"`
	LastSyncError     *string `json:"last_sync_error,omitempty"`
	CreatedAt         string  `json:"created_at"`
}

type GitLabConfigResponse struct {
	Configured       bool                           `json:"configured"`
	CanManage        bool                           `json:"can_manage"`
	BaseURL          string                         `json:"base_url,omitempty"`
	ManualWebhookURL string                         `json:"manual_webhook_url,omitempty"`
	Projects         []GitLabProjectBindingResponse `json:"projects"`
}

type GitLabMergeRequestResponse struct {
	ID                  string  `json:"id"`
	WorkspaceID         string  `json:"workspace_id"`
	ProjectPath         string  `json:"project_path"`
	GitLabProjectID     int64   `json:"gitlab_project_id"`
	IID                 int32   `json:"iid"`
	Title               string  `json:"title"`
	State               string  `json:"state"`
	WebURL              string  `json:"web_url"`
	SourceBranch        *string `json:"source_branch"`
	TargetBranch        *string `json:"target_branch"`
	AuthorUsername      *string `json:"author_username"`
	AuthorAvatarURL     *string `json:"author_avatar_url"`
	SHA                 string  `json:"sha"`
	DetailedMergeStatus *string `json:"detailed_merge_status"`
	HasConflicts        *bool   `json:"has_conflicts"`
	PipelineStatus      *string `json:"pipeline_status"`
	PipelineURL         *string `json:"pipeline_url"`
	Additions           int32   `json:"additions"`
	Deletions           int32   `json:"deletions"`
	ChangedFiles        int32   `json:"changed_files"`
	MergedAt            *string `json:"merged_at"`
	ClosedAt            *string `json:"closed_at"`
	MRCreatedAt         string  `json:"mr_created_at"`
	MRUpdatedAt         string  `json:"mr_updated_at"`
}

type createGitLabProjectRequest struct {
	Project string `json:"project"`
}

func gitLabProjectBindingToResponse(row db.GitlabProjectBinding) GitLabProjectBindingResponse {
	return GitLabProjectBindingResponse{
		ID:                uuidToString(row.ID),
		WorkspaceID:       uuidToString(row.WorkspaceID),
		GitLabProjectID:   row.GitlabProjectID,
		PathWithNamespace: row.PathWithNamespace,
		WebURL:            row.WebUrl,
		HookID:            int64PtrFromPg(row.HookID),
		HookEnabled:       row.HookEnabled,
		LastSyncError:     redactedTextPtr(row.LastSyncError),
		CreatedAt:         timestampToString(row.CreatedAt),
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

func (h *Handler) gitLabWebhookURL() string {
	if h.cfg.PublicURL != "" {
		return strings.TrimRight(h.cfg.PublicURL, "/") + "/api/webhooks/gitlab"
	}
	return "/api/webhooks/gitlab"
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
		Configured:       cfg.Configured(),
		CanManage:        canManage,
		BaseURL:          cfg.BaseURL,
		ManualWebhookURL: h.gitLabWebhookURL(),
		Projects:         projects,
	})
}

func (h *Handler) CreateGitLabProjectBinding(w http.ResponseWriter, r *http.Request) {
	workspaceID := chi.URLParam(r, "id")
	wsUUID, ok := parseUUIDOrBadRequest(w, workspaceID, "workspace id")
	if !ok {
		return
	}
	cfg := gitlab.LoadConfigFromEnv()
	if !cfg.Configured() {
		writeError(w, http.StatusServiceUnavailable, "gitlab integration is not configured")
		return
	}
	var req createGitLabProjectRequest
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	projectPath, ok := normalizeGitLabProjectInput(req.Project, cfg.Host)
	if !ok {
		writeError(w, http.StatusBadRequest, "invalid gitlab project")
		return
	}
	api, err := newGitLabClient(cfg)
	if err != nil {
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}
	project, err := api.GetProject(r.Context(), projectPath)
	if err != nil {
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}

	connectedBy := pgtype.UUID{}
	if member, ok := ctxMember(r.Context()); ok {
		connectedBy = member.UserID
	} else if userID := requestUserID(r); userID != "" {
		if u, err := parseStrictUUID(userID); err == nil {
			connectedBy = u
		}
	}
	conn, err := h.Queries.UpsertGitLabConnection(r.Context(), db.UpsertGitLabConnectionParams{
		WorkspaceID:   wsUUID,
		BaseUrl:       cfg.BaseURL,
		Host:          cfg.Host,
		ConnectedByID: connectedBy,
	})
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to save gitlab connection")
		return
	}

	webhookURL := h.gitLabWebhookURL()
	hookEnabled := false
	hookID := pgtype.Int8{}
	lastSyncError := pgtype.Text{}
	if existing, ok := h.findExistingGitLabProjectBinding(r.Context(), wsUUID, conn.ID, project.ID); ok && existing.HookEnabled && existing.HookID.Valid {
		hookEnabled = true
		hookID = existing.HookID
		lastSyncError = existing.LastSyncError
	} else if h.cfg.PublicURL != "" {
		hook, err := api.CreateProjectHook(r.Context(), project.ID, webhookURL, cfg.WebhookSecret)
		if err != nil {
			lastSyncError = strToText(redact.Text(err.Error()))
		} else {
			hookEnabled = true
			hookID = pgtype.Int8{Int64: hook.ID, Valid: true}
			lastSyncError = pgtype.Text{}
		}
	} else {
		lastSyncError = pgtype.Text{
			String: "MULTICA_PUBLIC_URL is not configured; create the GitLab webhook manually",
			Valid:  true,
		}
	}
	binding, err := h.Queries.UpsertGitLabProjectBinding(r.Context(), db.UpsertGitLabProjectBindingParams{
		WorkspaceID:       wsUUID,
		ConnectionID:      conn.ID,
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
	resp := gitLabProjectBindingToResponse(binding)
	h.publish(protocol.EventGitLabProjectCreated, workspaceID, "system", "", map[string]any{
		"project": resp,
	})
	writeJSON(w, http.StatusOK, map[string]any{
		"project":            resp,
		"manual_webhook_url": webhookURL,
	})
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
	cfg := gitlab.LoadConfigFromEnv()
	if binding.HookID.Valid {
		if cfg.BaseURL == "" || cfg.Token == "" {
			writeError(w, http.StatusServiceUnavailable, "gitlab api is not configured")
			return
		}
		api, err := newGitLabClient(cfg)
		if err != nil {
			writeError(w, http.StatusBadGateway, err.Error())
			return
		}
		if err := api.DeleteProjectHook(r.Context(), binding.GitlabProjectID, binding.HookID.Int64); err != nil {
			if err.Error() != "gitlab project not found" {
				writeError(w, http.StatusBadGateway, err.Error())
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
	cfg := gitlab.LoadConfigFromEnv()
	if cfg.WebhookSecret == "" {
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
		if !h.handleGitLabMergeRequestEvent(r.Context(), cfg, body) {
			writeError(w, http.StatusBadRequest, "invalid merge request payload")
			return
		}
		w.WriteHeader(http.StatusAccepted)
	case "Pipeline Hook":
		if !h.handleGitLabPipelineEvent(r.Context(), cfg, body) {
			writeError(w, http.StatusBadRequest, "invalid pipeline payload")
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
		IID          int32  `json:"iid"`
		Title        string `json:"title"`
		Description  string `json:"description"`
		State        string `json:"state"`
		Draft        bool   `json:"draft"`
		URL          string `json:"url"`
		SourceBranch string `json:"source_branch"`
		TargetBranch string `json:"target_branch"`
		LastCommit   struct {
			ID string `json:"id"`
		} `json:"last_commit"`
		MergeCommitSHA      string `json:"merge_commit_sha"`
		DetailedMergeStatus string `json:"detailed_merge_status"`
		HasConflicts        bool   `json:"has_conflicts"`
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

func (h *Handler) handleGitLabMergeRequestEvent(ctx context.Context, cfg gitlab.Config, body []byte) bool {
	var p gitLabMRWebhookPayload
	if err := json.Unmarshal(body, &p); err != nil {
		return false
	}
	bindings, err := h.Queries.ListGitLabProjectBindingsByHostAndProjectID(ctx, db.ListGitLabProjectBindingsByHostAndProjectIDParams{
		Host:            cfg.Host,
		GitlabProjectID: p.Project.ID,
	})
	if err != nil {
		slog.Warn("gitlab: lookup project bindings failed", "err", err, "project_id", p.Project.ID)
		return true
	}
	if len(bindings) == 0 {
		return true
	}

	for _, binding := range bindings {
		attrs := p.ObjectAttributes
		state := deriveGitLabMRState(attrs.State, attrs.Draft)
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
			HasConflicts:        pgtype.Bool{Bool: attrs.HasConflicts, Valid: true},
			MergedAt:            parseGitLabTime(attrs.MergedAt),
			ClosedAt:            parseGitLabTime(attrs.ClosedAt),
		})
		if err != nil {
			slog.Warn("gitlab: upsert merge request failed", "err", err, "workspace_id", uuidToString(binding.WorkspaceID), "project_id", p.Project.ID, "iid", attrs.IID)
			continue
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
				continue
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
					continue
				}
				if counts.OpenCount == 0 && counts.MergedWithCloseIntentCount > 0 {
					h.advanceIssueToDoneWithSource(ctx, issue, uuidToString(binding.WorkspaceID), "gitlab_mr_merged")
				}
			}
		}
		h.publish(protocol.EventGitLabMergeRequestUpdated, uuidToString(binding.WorkspaceID), "system", "", map[string]any{
			"merge_request_id": uuidToString(mr.ID),
			"linked_issue_ids": linkedIssueIDs,
		})
	}
	return true
}

type gitLabPipelineWebhookPayload struct {
	ObjectKind       string `json:"object_kind"`
	ObjectAttributes struct {
		ID        int64  `json:"id"`
		SHA       string `json:"sha"`
		Ref       string `json:"ref"`
		Status    string `json:"status"`
		URL       string `json:"url"`
		UpdatedAt string `json:"updated_at"`
	} `json:"object_attributes"`
	Project struct {
		ID int64 `json:"id"`
	} `json:"project"`
}

func (h *Handler) handleGitLabPipelineEvent(ctx context.Context, cfg gitlab.Config, body []byte) bool {
	var p gitLabPipelineWebhookPayload
	if err := json.Unmarshal(body, &p); err != nil {
		return false
	}
	bindings, err := h.Queries.ListGitLabProjectBindingsByHostAndProjectID(ctx, db.ListGitLabProjectBindingsByHostAndProjectIDParams{
		Host:            cfg.Host,
		GitlabProjectID: p.Project.ID,
	})
	if err != nil {
		slog.Warn("gitlab: lookup project bindings for pipeline failed", "err", err, "project_id", p.Project.ID)
		return true
	}
	for _, binding := range bindings {
		mrs, err := h.Queries.ListGitLabMergeRequestsByProjectBindingSHA(ctx, db.ListGitLabMergeRequestsByProjectBindingSHAParams{
			WorkspaceID:      binding.WorkspaceID,
			ProjectBindingID: binding.ID,
			Sha:              p.ObjectAttributes.SHA,
		})
		if err != nil {
			slog.Warn("gitlab: lookup merge requests for pipeline failed", "err", err, "workspace_id", uuidToString(binding.WorkspaceID), "sha", p.ObjectAttributes.SHA)
			continue
		}
		linkedIssueSet := map[string]struct{}{}
		for _, mr := range mrs {
			if err := h.Queries.UpsertGitLabMRPipeline(ctx, db.UpsertGitLabMRPipelineParams{
				MergeRequestID:    mr.ID,
				PipelineID:        p.ObjectAttributes.ID,
				Sha:               p.ObjectAttributes.SHA,
				Status:            normalizeGitLabPipelineStatus(p.ObjectAttributes.Status),
				PipelineUpdatedAt: parseGitLabTimeRequired(p.ObjectAttributes.UpdatedAt),
				Ref:               strToText(p.ObjectAttributes.Ref),
				WebUrl:            strToText(p.ObjectAttributes.URL),
			}); err != nil {
				slog.Warn("gitlab: upsert mr pipeline failed", "err", err, "mr_id", uuidToString(mr.ID), "pipeline_id", p.ObjectAttributes.ID)
				continue
			}
			issueIDs, err := h.Queries.ListIssueIDsForGitLabMergeRequest(ctx, db.ListIssueIDsForGitLabMergeRequestParams{
				WorkspaceID:    binding.WorkspaceID,
				MergeRequestID: mr.ID,
			})
			if err != nil {
				slog.Warn("gitlab: list linked issues for pipeline failed", "err", err, "mr_id", uuidToString(mr.ID))
				continue
			}
			for _, id := range issueIDs {
				linkedIssueSet[uuidToString(id)] = struct{}{}
			}
		}
		if len(mrs) > 0 {
			linkedIssueIDs := make([]string, 0, len(linkedIssueSet))
			for id := range linkedIssueSet {
				linkedIssueIDs = append(linkedIssueIDs, id)
			}
			h.publish(protocol.EventGitLabMergeRequestUpdated, uuidToString(binding.WorkspaceID), "system", "", map[string]any{
				"linked_issue_ids": linkedIssueIDs,
			})
		}
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
	case "failed", "canceled", "skipped", "manual":
		return "failed"
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
