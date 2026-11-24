package handler

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/integrations/gitlab"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/multica-ai/multica/server/pkg/protocol"
	"github.com/multica-ai/multica/server/pkg/redact"
)

type GitLabMergeRequestDetailsResponse struct {
	MergeRequest GitLabMergeRequestResponse   `json:"merge_request"`
	Approval     *GitLabMRApprovalResponse    `json:"approval"`
	Discussions  []GitLabMRDiscussionResponse `json:"discussions"`
	Jobs         []GitLabPipelineJobResponse  `json:"jobs"`
}

type GitLabMRApprovalResponse struct {
	Approved          bool            `json:"approved"`
	ApprovalsRequired *int32          `json:"approvals_required"`
	ApprovalsLeft     *int32          `json:"approvals_left"`
	ApprovedBy        json.RawMessage `json:"approved_by"`
	Rules             json.RawMessage `json:"rules"`
	FetchedAt         string          `json:"fetched_at"`
}

type GitLabMRDiscussionResponse struct {
	ID                 string                 `json:"id"`
	GitLabDiscussionID string                 `json:"gitlab_discussion_id"`
	IndividualNote     bool                   `json:"individual_note"`
	Resolved           *bool                  `json:"resolved"`
	CreatedAt          *string                `json:"created_at"`
	UpdatedAt          *string                `json:"updated_at"`
	Notes              []GitLabMRNoteResponse `json:"notes"`
}

type GitLabMRNoteResponse struct {
	ID              string  `json:"id"`
	GitLabNoteID    int64   `json:"gitlab_note_id"`
	AuthorUsername  *string `json:"author_username"`
	AuthorAvatarURL *string `json:"author_avatar_url"`
	Body            string  `json:"body"`
	System          bool    `json:"system"`
	Resolved        *bool   `json:"resolved"`
	Resolvable      *bool   `json:"resolvable"`
	CreatedAt       *string `json:"created_at"`
	UpdatedAt       *string `json:"updated_at"`
}

type GitLabPipelineJobResponse struct {
	ID                    string   `json:"id"`
	PipelineID            int64    `json:"pipeline_id"`
	JobID                 int64    `json:"job_id"`
	Name                  string   `json:"name"`
	Stage                 *string  `json:"stage"`
	Status                string   `json:"status"`
	Ref                   *string  `json:"ref"`
	SHA                   *string  `json:"sha"`
	WebURL                *string  `json:"web_url"`
	StartedAt             *string  `json:"started_at"`
	FinishedAt            *string  `json:"finished_at"`
	DurationSeconds       *float64 `json:"duration_seconds"`
	QueuedDurationSeconds *float64 `json:"queued_duration_seconds"`
	FailureReason         *string  `json:"failure_reason"`
	AllowFailure          bool     `json:"allow_failure"`
	ArtifactsFileName     *string  `json:"artifacts_file_name"`
	ArtifactsFileSize     *int64   `json:"artifacts_file_size"`
	ArtifactsExpireAt     *string  `json:"artifacts_expire_at"`
	TraceSummary          *string  `json:"trace_summary"`
	TraceTruncated        bool     `json:"trace_truncated"`
	TraceFetchedAt        *string  `json:"trace_fetched_at"`
}

func (h *Handler) GetVCSGitLabMergeRequestDetails(w http.ResponseWriter, r *http.Request) {
	issue, ok := h.loadIssueForUser(w, r, chi.URLParam(r, "id"))
	if !ok {
		return
	}
	prID, ok := parseUUIDOrBadRequest(w, chi.URLParam(r, "prId"), "pull request id")
	if !ok {
		return
	}
	pr, project, detail, ok := h.loadVCSGitLabMergeRequestForIssue(w, r, issue, prID)
	if !ok {
		return
	}
	h.writeVCSGitLabMergeRequestDetails(w, r, issue, pr, project, detail)
}

func (h *Handler) GetGitLabMergeRequestDetails(w http.ResponseWriter, r *http.Request) {
	issue, ok := h.loadIssueForUser(w, r, chi.URLParam(r, "id"))
	if !ok {
		return
	}
	legacyID, ok := parseUUIDOrBadRequest(w, chi.URLParam(r, "mrId"), "merge request id")
	if !ok {
		return
	}
	prID, err := h.resolveLegacyGitLabPullRequestID(r.Context(), issue.WorkspaceID, legacyID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to resolve gitlab merge request")
		return
	}
	pr, project, detail, ok := h.loadVCSGitLabMergeRequestForIssue(w, r, issue, prID)
	if !ok {
		return
	}
	h.writeVCSGitLabMergeRequestDetails(w, r, issue, pr, project, detail)
}

func (h *Handler) resolveLegacyGitLabPullRequestID(ctx context.Context, workspaceID, legacyID pgtype.UUID) (pgtype.UUID, error) {
	unifiedID, err := h.Queries.ResolveGitLabVCSMigrationPullRequestID(ctx, db.ResolveGitLabVCSMigrationPullRequestIDParams{
		WorkspaceID: workspaceID,
		LegacyID:    legacyID,
	})
	if err == nil {
		return unifiedID, nil
	}
	if errors.Is(err, pgx.ErrNoRows) {
		// Some migrations preserve the UUID. The canonical issue-link query
		// below still applies the same workspace and provider checks.
		return legacyID, nil
	}
	return pgtype.UUID{}, err
}

func (h *Handler) loadVCSGitLabMergeRequestForIssue(w http.ResponseWriter, r *http.Request, issue db.Issue, prID pgtype.UUID) (db.VcsPullRequest, db.VcsGitlabProject, db.VcsGitlabMrDetail, bool) {
	pr, err := h.Queries.GetVCSGitLabPullRequestForIssue(r.Context(), db.GetVCSGitLabPullRequestForIssueParams{
		IssueID:       issue.ID,
		WorkspaceID:   issue.WorkspaceID,
		PullRequestID: prID,
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			writeError(w, http.StatusNotFound, "gitlab merge request not found")
			return db.VcsPullRequest{}, db.VcsGitlabProject{}, db.VcsGitlabMrDetail{}, false
		}
		writeError(w, http.StatusInternalServerError, "failed to load gitlab merge request")
		return db.VcsPullRequest{}, db.VcsGitlabProject{}, db.VcsGitlabMrDetail{}, false
	}
	project, status, err := h.resolveVCSGitLabProjectForPullRequest(r.Context(), pr)
	if err != nil {
		writeGitLabProjectResolutionError(w, status)
		return db.VcsPullRequest{}, db.VcsGitlabProject{}, db.VcsGitlabMrDetail{}, false
	}
	detail, err := h.Queries.GetVCSGitLabMRDetail(r.Context(), db.GetVCSGitLabMRDetailParams{
		WorkspaceID:   issue.WorkspaceID,
		PullRequestID: pr.ID,
	})
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		writeError(w, http.StatusInternalServerError, "failed to load gitlab merge request details")
		return db.VcsPullRequest{}, db.VcsGitlabProject{}, db.VcsGitlabMrDetail{}, false
	}
	return pr, project, detail, true
}

func (h *Handler) resolveVCSGitLabProjectForPullRequest(ctx context.Context, pr db.VcsPullRequest) (db.VcsGitlabProject, int, error) {
	project, err := h.Queries.GetVCSGitLabProjectForPullRequest(ctx, db.GetVCSGitLabProjectForPullRequestParams{
		WorkspaceID: pr.WorkspaceID, ConnectionID: pr.ConnectionID,
		RepoOwner: pr.RepoOwner, RepoName: pr.RepoName,
	})
	if err == nil {
		return project, 0, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return db.VcsGitlabProject{}, http.StatusInternalServerError, err
	}

	connection, err := h.Queries.GetVCSGitLabConnectionByIDAndWorkspace(ctx, db.GetVCSGitLabConnectionByIDAndWorkspaceParams{
		ID: pr.ConnectionID, WorkspaceID: pr.WorkspaceID,
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return db.VcsGitlabProject{}, http.StatusNotFound, err
		}
		return db.VcsGitlabProject{}, http.StatusInternalServerError, err
	}
	if connection.Provider != "gitlab" || connection.WorkspaceID != pr.WorkspaceID {
		return db.VcsGitlabProject{}, http.StatusNotFound, errors.New("GitLab connection is outside the pull request workspace")
	}
	path := pr.RepoOwner + "/" + pr.RepoName
	owner, name, ok := splitGitLabProjectPath(path)
	if !ok {
		return db.VcsGitlabProject{}, http.StatusNotFound, errors.New("GitLab pull request has no valid repository path")
	}
	path = owner + "/" + name
	config, err := h.gitLabConfigForVCSConnectionRow(connection)
	if err != nil {
		return db.VcsGitlabProject{}, http.StatusServiceUnavailable, err
	}
	client, err := newGitLabClient(config)
	if err != nil {
		return db.VcsGitlabProject{}, http.StatusBadGateway, err
	}
	api, err := detailGitLabAPI(client)
	if err != nil {
		return db.VcsGitlabProject{}, http.StatusBadGateway, err
	}
	remote, err := api.GetProject(ctx, path)
	if err != nil {
		return db.VcsGitlabProject{}, http.StatusBadGateway, err
	}
	remoteOwner, remoteName, validRemotePath := splitGitLabProjectPath(remote.PathWithNamespace)
	if remote.ID <= 0 || !validRemotePath || !strings.EqualFold(remoteOwner+"/"+remoteName, path) {
		return db.VcsGitlabProject{}, http.StatusBadGateway, errors.New("GitLab returned an inconsistent project identity")
	}
	return db.VcsGitlabProject{
		WorkspaceID: pr.WorkspaceID, ConnectionID: pr.ConnectionID,
		GitlabProjectID: remote.ID, PathWithNamespace: remote.PathWithNamespace, WebUrl: remote.WebURL,
	}, 0, nil
}

func writeGitLabProjectResolutionError(w http.ResponseWriter, status int) {
	switch status {
	case http.StatusNotFound:
		writeError(w, status, "gitlab project not found")
	case http.StatusServiceUnavailable:
		writeError(w, status, "gitlab connection credentials are unavailable")
	case http.StatusBadGateway:
		writeError(w, status, "failed to resolve gitlab project")
	default:
		writeError(w, http.StatusInternalServerError, "failed to load gitlab project")
	}
}

func (h *Handler) writeVCSGitLabMergeRequestDetails(w http.ResponseWriter, r *http.Request, issue db.Issue, pr db.VcsPullRequest, project db.VcsGitlabProject, detail db.VcsGitlabMrDetail) {
	approval, approvalErr := h.Queries.GetGitLabMRApprovalStateByPullRequest(r.Context(), db.GetGitLabMRApprovalStateByPullRequestParams{
		WorkspaceID:   issue.WorkspaceID,
		PullRequestID: pr.ID,
	})
	if approvalErr != nil && !errors.Is(approvalErr, pgx.ErrNoRows) {
		writeError(w, http.StatusInternalServerError, "failed to load gitlab approvals")
		return
	}
	discussions, err := h.Queries.ListGitLabMRDiscussionsByPullRequest(r.Context(), db.ListGitLabMRDiscussionsByPullRequestParams{
		WorkspaceID:   issue.WorkspaceID,
		PullRequestID: pr.ID,
	})
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to load gitlab discussions")
		return
	}
	notes, err := h.Queries.ListGitLabMRNotesByPullRequest(r.Context(), db.ListGitLabMRNotesByPullRequestParams{
		WorkspaceID:   issue.WorkspaceID,
		PullRequestID: pr.ID,
	})
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to load gitlab notes")
		return
	}
	jobs, err := h.Queries.ListGitLabPipelineJobsByPullRequest(r.Context(), db.ListGitLabPipelineJobsByPullRequestParams{
		WorkspaceID:   issue.WorkspaceID,
		PullRequestID: pr.ID,
	})
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to load gitlab jobs")
		return
	}
	pipelines, err := h.Queries.ListVCSGitLabPipelinesByMR(r.Context(), db.ListVCSGitLabPipelinesByMRParams{
		WorkspaceID:   issue.WorkspaceID,
		PullRequestID: pr.ID,
	})
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to load gitlab pipelines")
		return
	}
	var approvalResp *GitLabMRApprovalResponse
	if approvalErr == nil {
		approvalResp = gitLabApprovalToResponse(approval)
	}
	writeJSON(w, http.StatusOK, GitLabMergeRequestDetailsResponse{
		MergeRequest: gitLabVCSMergeRequestToResponse(pr, project, detail, pipelines),
		Approval:     approvalResp,
		Discussions:  gitLabDiscussionsToResponse(discussions, notes),
		Jobs:         gitLabJobsToResponse(jobs),
	})
}

func (h *Handler) RefreshVCSGitLabMergeRequestForIssue(w http.ResponseWriter, r *http.Request) {
	issue, ok := h.loadIssueForUser(w, r, chi.URLParam(r, "id"))
	if !ok {
		return
	}
	prID, ok := parseUUIDOrBadRequest(w, chi.URLParam(r, "prId"), "pull request id")
	if !ok {
		return
	}
	pr, project, _, ok := h.loadVCSGitLabMergeRequestForIssue(w, r, issue, prID)
	if !ok {
		return
	}
	h.refreshVCSGitLabMergeRequestForIssue(w, r, issue, pr, project)
}

func (h *Handler) RefreshGitLabMergeRequestForIssue(w http.ResponseWriter, r *http.Request) {
	issue, ok := h.loadIssueForUser(w, r, chi.URLParam(r, "id"))
	if !ok {
		return
	}
	legacyID, ok := parseUUIDOrBadRequest(w, chi.URLParam(r, "mrId"), "merge request id")
	if !ok {
		return
	}
	prID, err := h.resolveLegacyGitLabPullRequestID(r.Context(), issue.WorkspaceID, legacyID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to resolve gitlab merge request")
		return
	}
	pr, project, _, ok := h.loadVCSGitLabMergeRequestForIssue(w, r, issue, prID)
	if !ok {
		return
	}
	h.refreshVCSGitLabMergeRequestForIssue(w, r, issue, pr, project)
}

func (h *Handler) refreshVCSGitLabMergeRequestForIssue(w http.ResponseWriter, r *http.Request, issue db.Issue, pr db.VcsPullRequest, project db.VcsGitlabProject) {
	cfg, err := h.gitLabConfigForVCSProject(r.Context(), project)
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, "gitlab connection credentials are unavailable")
		return
	}
	client, err := newGitLabClient(cfg)
	if err != nil {
		writeError(w, http.StatusBadGateway, redact.Text(err.Error()))
		return
	}
	api, err := detailGitLabAPI(client)
	if err != nil {
		writeError(w, http.StatusBadGateway, redact.Text(err.Error()))
		return
	}
	refreshed, _, err := h.refreshVCSGitLabMergeRequest(r.Context(), api, project, pr)
	if err != nil {
		status := http.StatusBadGateway
		if isGitLabRefreshPersistenceError(err) {
			status = http.StatusInternalServerError
		}
		writeError(w, status, redact.Text(err.Error()))
		return
	}
	h.publish(protocol.EventGitLabMergeRequestUpdated, uuidToString(issue.WorkspaceID), "system", "", map[string]any{
		"merge_request_id": uuidToString(refreshed.ID),
		"linked_issue_ids": []string{uuidToString(issue.ID)},
	})
	fresh, freshProject, detail, ok := h.loadVCSGitLabMergeRequestForIssue(w, r, issue, refreshed.ID)
	if !ok {
		return
	}
	h.writeVCSGitLabMergeRequestDetails(w, r, issue, fresh, freshProject, detail)
}

func (h *Handler) MergeVCSGitLabMergeRequestForIssue(w http.ResponseWriter, r *http.Request) {
	issue, ok := h.loadIssueForUser(w, r, chi.URLParam(r, "id"))
	if !ok {
		return
	}
	prID, ok := parseUUIDOrBadRequest(w, chi.URLParam(r, "prId"), "pull request id")
	if !ok {
		return
	}
	pr, project, _, ok := h.loadVCSGitLabMergeRequestForIssue(w, r, issue, prID)
	if !ok {
		return
	}
	h.mergeVCSGitLabMergeRequestForIssue(w, r, issue, pr, project)
}

func (h *Handler) MergeGitLabMergeRequestForIssue(w http.ResponseWriter, r *http.Request) {
	issue, ok := h.loadIssueForUser(w, r, chi.URLParam(r, "id"))
	if !ok {
		return
	}
	legacyID, ok := parseUUIDOrBadRequest(w, chi.URLParam(r, "mrId"), "merge request id")
	if !ok {
		return
	}
	prID, err := h.resolveLegacyGitLabPullRequestID(r.Context(), issue.WorkspaceID, legacyID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to resolve gitlab merge request")
		return
	}
	pr, project, _, ok := h.loadVCSGitLabMergeRequestForIssue(w, r, issue, prID)
	if !ok {
		return
	}
	h.mergeVCSGitLabMergeRequestForIssue(w, r, issue, pr, project)
}

func (h *Handler) mergeVCSGitLabMergeRequestForIssue(w http.ResponseWriter, r *http.Request, issue db.Issue, pr db.VcsPullRequest, project db.VcsGitlabProject) {
	cfg, err := h.gitLabConfigForVCSProject(r.Context(), project)
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, "gitlab connection credentials are unavailable")
		return
	}
	client, err := newGitLabClient(cfg)
	if err != nil {
		writeError(w, http.StatusBadGateway, redact.Text(err.Error()))
		return
	}
	api, err := detailGitLabAPI(client)
	if err != nil {
		writeError(w, http.StatusBadGateway, redact.Text(err.Error()))
		return
	}
	if _, err := api.MergeMergeRequest(r.Context(), project.GitlabProjectID, pr.PrNumber, gitlab.MergeRequestMergeOptions{
		ShouldRemoveSourceBranch: true,
	}); err != nil {
		_ = h.markVCSGitLabMergeRequestRefreshErrorForGeneration(r.Context(), pr, pr.WakeupSyncGeneration, err)
		writeError(w, http.StatusBadGateway, redact.Text(err.Error()))
		return
	}
	refreshed, _, err := h.refreshVCSGitLabMergeRequest(r.Context(), api, project, pr)
	if err != nil {
		status := http.StatusBadGateway
		if isGitLabRefreshPersistenceError(err) {
			status = http.StatusInternalServerError
		}
		writeError(w, status, redact.Text(err.Error()))
		return
	}
	h.publish(protocol.EventGitLabMergeRequestUpdated, uuidToString(issue.WorkspaceID), "system", "", map[string]any{
		"merge_request_id": uuidToString(refreshed.ID),
		"linked_issue_ids": []string{uuidToString(issue.ID)},
	})
	fresh, freshProject, detail, ok := h.loadVCSGitLabMergeRequestForIssue(w, r, issue, refreshed.ID)
	if !ok {
		return
	}
	h.writeVCSGitLabMergeRequestDetails(w, r, issue, fresh, freshProject, detail)
}

func (h *Handler) RefreshGitLabProjectBinding(w http.ResponseWriter, r *http.Request) {
	h.refreshVCSGitLabProjectRoute(w, r, "bindingId")
}

func (h *Handler) RefreshVCSGitLabProject(w http.ResponseWriter, r *http.Request) {
	h.refreshVCSGitLabProjectRoute(w, r, "projectId")
}

func (h *Handler) refreshVCSGitLabProjectRoute(w http.ResponseWriter, r *http.Request, idParam string) {
	workspaceID, ok := parseUUIDOrBadRequest(w, chi.URLParam(r, "id"), "workspace id")
	if !ok {
		return
	}
	projectID, ok := parseUUIDOrBadRequest(w, chi.URLParam(r, idParam), "project id")
	if !ok {
		return
	}
	project, err := h.Queries.GetVCSGitLabProjectByIDAndWorkspace(r.Context(), db.GetVCSGitLabProjectByIDAndWorkspaceParams{
		ID:          projectID,
		WorkspaceID: workspaceID,
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			writeError(w, http.StatusNotFound, "gitlab project not found")
			return
		}
		writeError(w, http.StatusInternalServerError, "failed to load gitlab project")
		return
	}
	cfg, err := h.gitLabConfigForVCSProject(r.Context(), project)
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, "gitlab connection credentials are unavailable")
		return
	}
	client, err := newGitLabClient(cfg)
	if err != nil {
		writeError(w, http.StatusBadGateway, redact.Text(err.Error()))
		return
	}
	api, err := detailGitLabAPI(client)
	if err != nil {
		writeError(w, http.StatusBadGateway, redact.Text(err.Error()))
		return
	}
	if err := h.Queries.MarkVCSGitLabProjectRefreshStarted(r.Context(), db.MarkVCSGitLabProjectRefreshStartedParams{
		ID: project.ID, WorkspaceID: project.WorkspaceID, ConnectionID: project.ConnectionID,
	}); err != nil {
		writeError(w, http.StatusInternalServerError, "failed to start gitlab project refresh")
		return
	}
	owner, name, validPath := splitGitLabProjectPath(project.PathWithNamespace)
	if !validPath {
		refreshErr := errors.New("invalid GitLab project path")
		slog.Warn("invalid GitLab project path in stored project binding",
			"error", refreshErr, "workspace_id", workspaceID, "project_id", projectID)
		_ = h.Queries.MarkVCSGitLabProjectRefreshFinished(r.Context(), db.MarkVCSGitLabProjectRefreshFinishedParams{
			ID: project.ID, WorkspaceID: project.WorkspaceID, ConnectionID: project.ConnectionID,
			LastRefreshError: strToText(refreshErr.Error()),
		})
		writeError(w, http.StatusInternalServerError, "failed to refresh gitlab project")
		return
	}
	pullRequests, err := h.Queries.ListVCSGitLabPullRequestsByProject(r.Context(), db.ListVCSGitLabPullRequestsByProjectParams{
		WorkspaceID:  project.WorkspaceID,
		ConnectionID: project.ConnectionID,
		RepoOwner:    owner,
		RepoName:     name,
	})
	if err != nil {
		refreshErr := redact.Text(err.Error())
		_ = h.Queries.MarkVCSGitLabProjectRefreshFinished(r.Context(), db.MarkVCSGitLabProjectRefreshFinishedParams{
			ID: project.ID, WorkspaceID: project.WorkspaceID, ConnectionID: project.ConnectionID,
			LastRefreshError: strToText(refreshErr),
		})
		writeError(w, http.StatusInternalServerError, "failed to list gitlab merge requests")
		return
	}
	var summary gitLabRefreshSummary
	for _, pr := range pullRequests {
		_, itemSummary, refreshErr := h.refreshVCSGitLabMergeRequest(r.Context(), api, project, pr)
		summary.UpdatedMRs += itemSummary.UpdatedMRs
		summary.UpdatedJobs += itemSummary.UpdatedJobs
		summary.UpdatedDiscussions += itemSummary.UpdatedDiscussions
		summary.Errors = append(summary.Errors, itemSummary.Errors...)
		if refreshErr != nil {
			summary.addError(refreshErr)
			if isGitLabRefreshPersistenceError(refreshErr) {
				break
			}
		}
	}
	var lastRefreshError pgtype.Text
	if len(summary.Errors) > 0 {
		lastRefreshError = strToText(strings.Join(summary.Errors, "; "))
	}
	finishErr := h.Queries.MarkVCSGitLabProjectRefreshFinished(r.Context(), db.MarkVCSGitLabProjectRefreshFinishedParams{
		ID: project.ID, WorkspaceID: project.WorkspaceID, ConnectionID: project.ConnectionID,
		LastRefreshError: lastRefreshError,
	})
	if finishErr != nil {
		writeError(w, http.StatusInternalServerError, "failed to finish gitlab project refresh")
		return
	}
	refreshedProject, err := h.Queries.GetVCSGitLabProjectByIDAndWorkspace(r.Context(), db.GetVCSGitLabProjectByIDAndWorkspaceParams{
		ID: project.ID, WorkspaceID: project.WorkspaceID,
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			writeError(w, http.StatusNotFound, "gitlab project not found")
		} else {
			writeError(w, http.StatusInternalServerError, "failed to reload gitlab project")
		}
		return
	}
	project = refreshedProject
	h.publish(protocol.EventGitLabProjectCreated, uuidToString(project.WorkspaceID), "system", "", map[string]any{
		"project": map[string]any{
			"id": uuidToString(project.ID), "workspace_id": uuidToString(project.WorkspaceID),
			"connection_id": uuidToString(project.ConnectionID), "gitlab_project_id": project.GitlabProjectID,
			"path_with_namespace": project.PathWithNamespace, "web_url": project.WebUrl,
			"hook_id": int64PtrFromPg(project.HookID), "hook_enabled": project.HookEnabled,
			"last_refresh_at":    timestampToPtr(project.LastRefreshAt),
			"last_refresh_error": redactedTextPtr(project.LastRefreshError),
		},
	})
	writeJSON(w, http.StatusOK, summary)
}

func (h *Handler) GetGitLabJobTrace(w http.ResponseWriter, r *http.Request) {
	h.getVCSGitLabJobTrace(w, r, nil)
}

func (h *Handler) GetVCSGitLabJobTrace(w http.ResponseWriter, r *http.Request) {
	prID, ok := parseUUIDOrBadRequest(w, chi.URLParam(r, "prId"), "pull request id")
	if !ok {
		return
	}
	h.getVCSGitLabJobTrace(w, r, &prID)
}

func (h *Handler) getVCSGitLabJobTrace(w http.ResponseWriter, r *http.Request, prID *pgtype.UUID) {
	issue, job, pr, project, pipeline, ok := h.loadVCSGitLabJobForIssue(w, r, prID)
	if !ok {
		return
	}
	cfg, err := h.gitLabConfigForVCSProject(r.Context(), project)
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, "gitlab connection credentials are unavailable")
		return
	}
	client, err := newGitLabClient(cfg)
	if err != nil {
		writeError(w, http.StatusBadGateway, redact.Text(err.Error()))
		return
	}
	api, err := detailGitLabAPI(client)
	if err != nil {
		writeError(w, http.StatusBadGateway, redact.Text(err.Error()))
		return
	}
	trace, err := api.GetJobTrace(r.Context(), pipeline.PipelineProjectID, job.JobID, gitLabFailedJobTraceLimit)
	if err != nil {
		writeError(w, http.StatusBadGateway, redact.Text(err.Error()))
		return
	}
	summary := redact.Text(trace.Text)
	_, err = h.Queries.UpdateGitLabPipelineJobTraceSummaryByPullRequest(r.Context(), db.UpdateGitLabPipelineJobTraceSummaryByPullRequestParams{
		JobID: job.ID, WorkspaceID: issue.WorkspaceID, PullRequestID: pr.ID,
		WakeupSyncGeneration: pr.WakeupSyncGeneration,
		TraceSummary:         strToText(summary), TraceTruncated: trace.Truncated,
	})
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to store gitlab job trace summary")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"trace_summary":   summary,
		"trace_truncated": trace.Truncated,
	})
}

func (h *Handler) OpenGitLabJobArtifacts(w http.ResponseWriter, r *http.Request) {
	h.openVCSGitLabJobArtifacts(w, r, nil)
}

func (h *Handler) OpenVCSGitLabJobArtifacts(w http.ResponseWriter, r *http.Request) {
	prID, ok := parseUUIDOrBadRequest(w, chi.URLParam(r, "prId"), "pull request id")
	if !ok {
		return
	}
	h.openVCSGitLabJobArtifacts(w, r, &prID)
}

func (h *Handler) openVCSGitLabJobArtifacts(w http.ResponseWriter, r *http.Request, prID *pgtype.UUID) {
	_, job, _, project, pipeline, ok := h.loadVCSGitLabJobForIssue(w, r, prID)
	if !ok {
		return
	}
	if !job.ArtifactsFileName.Valid {
		writeError(w, http.StatusNotFound, "gitlab artifact not found")
		return
	}
	cfg, err := h.gitLabConfigForVCSProject(r.Context(), project)
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, "gitlab connection credentials are unavailable")
		return
	}
	client, err := newGitLabClient(cfg)
	if err != nil {
		writeError(w, http.StatusBadGateway, redact.Text(err.Error()))
		return
	}
	api, err := detailGitLabAPI(client)
	if err != nil {
		writeError(w, http.StatusBadGateway, redact.Text(err.Error()))
		return
	}
	resp, err := api.DownloadJobArtifacts(r.Context(), pipeline.PipelineProjectID, job.JobID)
	if err != nil {
		writeError(w, http.StatusBadGateway, redact.Text(err.Error()))
		return
	}
	if resp == nil || resp.Body == nil {
		writeError(w, http.StatusBadGateway, "gitlab artifact response is empty")
		return
	}
	defer resp.Body.Close()
	for _, key := range []string{"Content-Type", "Content-Length"} {
		if value := resp.Header.Get(key); value != "" {
			w.Header().Set(key, value)
		}
	}
	if w.Header().Get("Content-Type") == "" {
		w.Header().Set("Content-Type", "application/octet-stream")
	}
	w.Header().Set("Content-Disposition", "attachment; filename="+strconv.Quote(job.ArtifactsFileName.String))
	w.WriteHeader(resp.StatusCode)
	_, _ = io.Copy(w, resp.Body)
}

func (h *Handler) loadVCSGitLabJobForIssue(w http.ResponseWriter, r *http.Request, prID *pgtype.UUID) (db.Issue, db.GitlabPipelineJob, db.VcsPullRequest, db.VcsGitlabProject, db.VcsGitlabPipeline, bool) {
	issue, ok := h.loadIssueForUser(w, r, chi.URLParam(r, "id"))
	if !ok {
		return db.Issue{}, db.GitlabPipelineJob{}, db.VcsPullRequest{}, db.VcsGitlabProject{}, db.VcsGitlabPipeline{}, false
	}
	jobID, ok := parseUUIDOrBadRequest(w, chi.URLParam(r, "jobId"), "job id")
	if !ok {
		return db.Issue{}, db.GitlabPipelineJob{}, db.VcsPullRequest{}, db.VcsGitlabProject{}, db.VcsGitlabPipeline{}, false
	}
	var job db.GitlabPipelineJob
	var err error
	if prID == nil {
		job, err = h.Queries.GetGitLabVCSJobForIssue(r.Context(), db.GetGitLabVCSJobForIssueParams{
			IssueID: issue.ID, WorkspaceID: issue.WorkspaceID, JobID: jobID,
		})
	} else {
		job, err = h.Queries.GetGitLabVCSJobForIssueByPullRequest(r.Context(), db.GetGitLabVCSJobForIssueByPullRequestParams{
			IssueID: issue.ID, WorkspaceID: issue.WorkspaceID, PullRequestID: *prID, JobID: jobID,
		})
	}
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			writeError(w, http.StatusNotFound, "gitlab job not found")
			return db.Issue{}, db.GitlabPipelineJob{}, db.VcsPullRequest{}, db.VcsGitlabProject{}, db.VcsGitlabPipeline{}, false
		}
		writeError(w, http.StatusInternalServerError, "failed to load gitlab job")
		return db.Issue{}, db.GitlabPipelineJob{}, db.VcsPullRequest{}, db.VcsGitlabProject{}, db.VcsGitlabPipeline{}, false
	}
	canonicalPRID := job.VcsPullRequestID
	pr, err := h.Queries.GetVCSGitLabPullRequestForIssue(r.Context(), db.GetVCSGitLabPullRequestForIssueParams{
		IssueID: issue.ID, WorkspaceID: issue.WorkspaceID, PullRequestID: canonicalPRID,
	})
	if err != nil {
		writeError(w, http.StatusNotFound, "gitlab merge request not found")
		return db.Issue{}, db.GitlabPipelineJob{}, db.VcsPullRequest{}, db.VcsGitlabProject{}, db.VcsGitlabPipeline{}, false
	}
	project, status, err := h.resolveVCSGitLabProjectForPullRequest(r.Context(), pr)
	if err != nil {
		writeGitLabProjectResolutionError(w, status)
		return db.Issue{}, db.GitlabPipelineJob{}, db.VcsPullRequest{}, db.VcsGitlabProject{}, db.VcsGitlabPipeline{}, false
	}
	pipeline, err := h.Queries.GetVCSGitLabPipelineForJob(r.Context(), db.GetVCSGitLabPipelineForJobParams{
		WorkspaceID: issue.WorkspaceID, PullRequestID: pr.ID, PipelineID: job.PipelineID,
	})
	if err != nil {
		writeError(w, http.StatusNotFound, "gitlab pipeline not found")
		return db.Issue{}, db.GitlabPipelineJob{}, db.VcsPullRequest{}, db.VcsGitlabProject{}, db.VcsGitlabPipeline{}, false
	}
	return issue, job, pr, project, pipeline, true
}

func (h *Handler) gitLabConfigForVCSProject(ctx context.Context, project db.VcsGitlabProject) (gitlab.Config, error) {
	connection, err := h.Queries.GetVCSGitLabConnectionByIDAndWorkspace(ctx, db.GetVCSGitLabConnectionByIDAndWorkspaceParams{
		ID: project.ConnectionID, WorkspaceID: project.WorkspaceID,
	})
	if err != nil {
		return gitlab.Config{}, err
	}
	if connection.Provider != "gitlab" || connection.WorkspaceID != project.WorkspaceID {
		return gitlab.Config{}, errors.New("GitLab connection does not belong to this workspace")
	}
	baseURL := strings.TrimRight(strings.TrimSpace(connection.InstanceUrl), "/")
	parsed, err := url.Parse(baseURL)
	if err != nil || parsed.Host == "" || (parsed.Scheme != "https" && parsed.Scheme != "http") {
		return gitlab.Config{}, errors.New("GitLab connection URL is invalid")
	}
	token, err := h.openVCSSecret(connection.AccessTokenEncrypted)
	if err != nil {
		return gitlab.Config{}, errors.New("cannot decrypt GitLab connection credentials")
	}
	env := gitlab.LoadConfigFromEnv()
	if token == "" && strings.TrimRight(env.BaseURL, "/") == baseURL {
		token = env.Token
	}
	if token == "" {
		return gitlab.Config{}, errors.New("GitLab connection credentials are unavailable")
	}
	return gitlab.Config{
		BaseURL: baseURL, Host: parsed.Host, Token: token, ProxyURL: env.ProxyURL,
	}, nil
}

func gitLabVCSMergeRequestToResponse(pr db.VcsPullRequest, project db.VcsGitlabProject, detail db.VcsGitlabMrDetail, pipelines []db.VcsGitlabPipeline) GitLabMergeRequestResponse {
	response := GitLabMergeRequestResponse{
		ID:                  uuidToString(pr.ID),
		WorkspaceID:         uuidToString(pr.WorkspaceID),
		ProjectPath:         project.PathWithNamespace,
		GitLabProjectID:     project.GitlabProjectID,
		IID:                 pr.PrNumber,
		Title:               pr.Title,
		State:               pr.State,
		WebURL:              pr.HtmlUrl,
		SourceBranch:        textToPtr(pr.Branch),
		TargetBranch:        textToPtr(detail.TargetBranch),
		AuthorUsername:      textToPtr(pr.AuthorLogin),
		AuthorAvatarURL:     textToPtr(pr.AuthorAvatarUrl),
		SHA:                 pr.HeadSha,
		DetailedMergeStatus: textToPtr(detail.DetailedMergeStatus),
		HasConflicts:        boolPtrFromPg(pr.HasConflicts),
		Additions:           pr.Additions,
		Deletions:           pr.Deletions,
		ChangedFiles:        pr.ChangedFiles,
		Reviewers:           jsonArrayOrEmpty(detail.Reviewers),
		Assignees:           jsonArrayOrEmpty(detail.Assignees),
		Labels:              jsonArrayOrEmpty(detail.Labels),
		LastRefreshedAt:     timestampToPtr(detail.LastRefreshedAt),
		LastRefreshError:    redactedTextPtr(detail.LastRefreshError),
		MergedAt:            timestampToPtr(pr.MergedAt),
		ClosedAt:            timestampToPtr(pr.ClosedAt),
		MRCreatedAt:         timestampToString(pr.PrCreatedAt),
		MRUpdatedAt:         timestampToString(pr.PrUpdatedAt),
	}
	if pr.HeadSha != "" {
		for _, pipeline := range pipelines {
			if pipeline.Sha != pr.HeadSha {
				continue
			}
			status := pipeline.Status
			response.PipelineStatus = &status
			response.PipelineURL = textToPtr(pipeline.WebUrl)
			break
		}
	}
	return response
}

func gitLabApprovalToResponse(row db.GitlabMrApprovalState) *GitLabMRApprovalResponse {
	return &GitLabMRApprovalResponse{
		Approved:          row.Approved,
		ApprovalsRequired: int4ToPtr(row.ApprovalsRequired),
		ApprovalsLeft:     int4ToPtr(row.ApprovalsLeft),
		ApprovedBy:        jsonArrayOrEmpty(row.ApprovedBy),
		Rules:             jsonArrayOrEmpty(row.Rules),
		FetchedAt:         timestampToString(row.FetchedAt),
	}
}

func gitLabDiscussionsToResponse(discussions []db.GitlabMrDiscussion, notes []db.GitlabMrNote) []GitLabMRDiscussionResponse {
	notesByDiscussion := make(map[string][]GitLabMRNoteResponse, len(discussions))
	for _, note := range notes {
		key := uuidToString(note.DiscussionID)
		notesByDiscussion[key] = append(notesByDiscussion[key], gitLabNoteToResponse(note))
	}
	out := make([]GitLabMRDiscussionResponse, 0, len(discussions))
	for _, discussion := range discussions {
		key := uuidToString(discussion.ID)
		discussionNotes := notesByDiscussion[key]
		if discussionNotes == nil {
			discussionNotes = make([]GitLabMRNoteResponse, 0)
		}
		out = append(out, GitLabMRDiscussionResponse{
			ID:                 key,
			GitLabDiscussionID: discussion.GitlabDiscussionID,
			IndividualNote:     discussion.IndividualNote,
			Resolved:           boolPtrFromPg(discussion.Resolved),
			CreatedAt:          timestampToPtr(discussion.DiscussionCreatedAt),
			UpdatedAt:          timestampToPtr(discussion.DiscussionUpdatedAt),
			Notes:              discussionNotes,
		})
	}
	return out
}

func gitLabNoteToResponse(row db.GitlabMrNote) GitLabMRNoteResponse {
	return GitLabMRNoteResponse{
		ID:              uuidToString(row.ID),
		GitLabNoteID:    row.GitlabNoteID,
		AuthorUsername:  textToPtr(row.AuthorUsername),
		AuthorAvatarURL: textToPtr(row.AuthorAvatarUrl),
		Body:            row.Body,
		System:          row.System,
		Resolved:        boolPtrFromPg(row.Resolved),
		Resolvable:      boolPtrFromPg(row.Resolvable),
		CreatedAt:       timestampToPtr(row.NoteCreatedAt),
		UpdatedAt:       timestampToPtr(row.NoteUpdatedAt),
	}
}

func gitLabJobsToResponse(rows []db.GitlabPipelineJob) []GitLabPipelineJobResponse {
	out := make([]GitLabPipelineJobResponse, 0, len(rows))
	for _, row := range rows {
		out = append(out, GitLabPipelineJobResponse{
			ID:                    uuidToString(row.ID),
			PipelineID:            row.PipelineID,
			JobID:                 row.JobID,
			Name:                  row.Name,
			Stage:                 textToPtr(row.Stage),
			Status:                row.Status,
			Ref:                   textToPtr(row.Ref),
			SHA:                   textToPtr(row.Sha),
			WebURL:                textToPtr(row.WebUrl),
			StartedAt:             timestampToPtr(row.StartedAt),
			FinishedAt:            timestampToPtr(row.FinishedAt),
			DurationSeconds:       float8ToPtr(row.DurationSeconds),
			QueuedDurationSeconds: float8ToPtr(row.QueuedDurationSeconds),
			FailureReason:         textToPtr(row.FailureReason),
			AllowFailure:          row.AllowFailure,
			ArtifactsFileName:     textToPtr(row.ArtifactsFileName),
			ArtifactsFileSize:     int8ToPtr(row.ArtifactsFileSize),
			ArtifactsExpireAt:     timestampToPtr(row.ArtifactsExpireAt),
			TraceSummary:          textToPtr(row.TraceSummary),
			TraceTruncated:        row.TraceTruncated,
			TraceFetchedAt:        timestampToPtr(row.TraceFetchedAt),
		})
	}
	return out
}

func jsonArrayOrEmpty(raw []byte) json.RawMessage {
	if len(strings.TrimSpace(string(raw))) == 0 {
		return json.RawMessage("[]")
	}
	return json.RawMessage(raw)
}

func float8ToPtr(v pgtype.Float8) *float64 {
	if !v.Valid {
		return nil
	}
	return &v.Float64
}
