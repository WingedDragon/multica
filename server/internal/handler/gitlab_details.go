package handler

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
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

func (h *Handler) GetGitLabMergeRequestDetails(w http.ResponseWriter, r *http.Request) {
	issue, ok := h.loadIssueForUser(w, r, chi.URLParam(r, "id"))
	if !ok {
		return
	}
	mrID, ok := parseUUIDOrBadRequest(w, chi.URLParam(r, "mrId"), "merge request id")
	if !ok {
		return
	}
	mr, err := h.Queries.GetGitLabMergeRequestForIssue(r.Context(), db.GetGitLabMergeRequestForIssueParams{
		IssueID:     issue.ID,
		ID:          mrID,
		WorkspaceID: issue.WorkspaceID,
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			writeError(w, http.StatusNotFound, "gitlab merge request not found")
			return
		}
		writeError(w, http.StatusInternalServerError, "failed to load gitlab merge request")
		return
	}

	binding, err := h.Queries.GetGitLabProjectBinding(r.Context(), db.GetGitLabProjectBindingParams{
		ID:          mr.ProjectBindingID,
		WorkspaceID: issue.WorkspaceID,
	})
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to load gitlab project")
		return
	}

	approval, err := h.Queries.GetGitLabMRApprovalState(r.Context(), db.GetGitLabMRApprovalStateParams{
		WorkspaceID:    issue.WorkspaceID,
		MergeRequestID: mr.ID,
	})
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		writeError(w, http.StatusInternalServerError, "failed to load gitlab approvals")
		return
	}
	discussions, err := h.Queries.ListGitLabMRDiscussions(r.Context(), db.ListGitLabMRDiscussionsParams{
		WorkspaceID:    issue.WorkspaceID,
		MergeRequestID: mr.ID,
	})
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to load gitlab discussions")
		return
	}
	notes, err := h.Queries.ListGitLabMRNotes(r.Context(), db.ListGitLabMRNotesParams{
		WorkspaceID:    issue.WorkspaceID,
		MergeRequestID: mr.ID,
	})
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to load gitlab notes")
		return
	}
	jobs, err := h.Queries.ListGitLabPipelineJobsByMR(r.Context(), db.ListGitLabPipelineJobsByMRParams{
		WorkspaceID:    issue.WorkspaceID,
		MergeRequestID: mr.ID,
	})
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to load gitlab jobs")
		return
	}

	var approvalResp *GitLabMRApprovalResponse
	if err == nil && approval.MergeRequestID.Valid {
		approvalResp = gitLabApprovalToResponse(approval)
	}
	writeJSON(w, http.StatusOK, GitLabMergeRequestDetailsResponse{
		MergeRequest: gitLabMergeRequestToResponse(mr, binding.PathWithNamespace),
		Approval:     approvalResp,
		Discussions:  gitLabDiscussionsToResponse(discussions, notes),
		Jobs:         gitLabJobsToResponse(jobs),
	})
}

func (h *Handler) RefreshGitLabMergeRequestForIssue(w http.ResponseWriter, r *http.Request) {
	issue, ok := h.loadIssueForUser(w, r, chi.URLParam(r, "id"))
	if !ok {
		return
	}
	mrID, ok := parseUUIDOrBadRequest(w, chi.URLParam(r, "mrId"), "merge request id")
	if !ok {
		return
	}
	mr, err := h.Queries.GetGitLabMergeRequestForIssue(r.Context(), db.GetGitLabMergeRequestForIssueParams{
		IssueID:     issue.ID,
		ID:          mrID,
		WorkspaceID: issue.WorkspaceID,
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			writeError(w, http.StatusNotFound, "gitlab merge request not found")
			return
		}
		writeError(w, http.StatusInternalServerError, "failed to load gitlab merge request")
		return
	}
	cfg := gitlab.LoadConfigFromEnv()
	if !cfg.Configured() {
		writeError(w, http.StatusServiceUnavailable, "gitlab integration is not configured")
		return
	}
	api, err := newGitLabClient(cfg)
	if err != nil {
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}
	detailAPI, err := detailGitLabAPI(api)
	if err != nil {
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}
	refreshed, summary, err := h.refreshGitLabMergeRequest(r.Context(), detailAPI, mr)
	if err != nil {
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}
	if len(summary.Errors) > 0 {
		h.markGitLabMergeRequestRefreshError(r.Context(), refreshed, errors.New(strings.Join(summary.Errors, "; ")))
	}
	h.publish(protocol.EventGitLabMergeRequestUpdated, uuidToString(issue.WorkspaceID), "system", "", map[string]any{
		"merge_request_id": uuidToString(mr.ID),
		"linked_issue_ids": []string{uuidToString(issue.ID)},
	})
	h.GetGitLabMergeRequestDetails(w, r)
}

func (h *Handler) MergeGitLabMergeRequestForIssue(w http.ResponseWriter, r *http.Request) {
	issue, ok := h.loadIssueForUser(w, r, chi.URLParam(r, "id"))
	if !ok {
		return
	}
	mrID, ok := parseUUIDOrBadRequest(w, chi.URLParam(r, "mrId"), "merge request id")
	if !ok {
		return
	}
	mr, err := h.Queries.GetGitLabMergeRequestForIssue(r.Context(), db.GetGitLabMergeRequestForIssueParams{
		IssueID:     issue.ID,
		ID:          mrID,
		WorkspaceID: issue.WorkspaceID,
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			writeError(w, http.StatusNotFound, "gitlab merge request not found")
			return
		}
		writeError(w, http.StatusInternalServerError, "failed to load gitlab merge request")
		return
	}
	cfg := gitlab.LoadConfigFromEnv()
	if !cfg.Configured() {
		writeError(w, http.StatusServiceUnavailable, "gitlab integration is not configured")
		return
	}
	api, err := newGitLabClient(cfg)
	if err != nil {
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}
	detailAPI, err := detailGitLabAPI(api)
	if err != nil {
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}
	if _, err := detailAPI.MergeMergeRequest(r.Context(), mr.GitlabProjectID, mr.MrIid, gitlab.MergeRequestMergeOptions{
		ShouldRemoveSourceBranch: true,
	}); err != nil {
		h.markGitLabMergeRequestRefreshError(r.Context(), mr, err)
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}
	refreshed, summary, err := h.refreshGitLabMergeRequest(r.Context(), detailAPI, mr)
	if err != nil {
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}
	if len(summary.Errors) > 0 {
		h.markGitLabMergeRequestRefreshError(r.Context(), refreshed, errors.New(strings.Join(summary.Errors, "; ")))
	}
	h.publish(protocol.EventGitLabMergeRequestUpdated, uuidToString(issue.WorkspaceID), "system", "", map[string]any{
		"merge_request_id": uuidToString(refreshed.ID),
		"linked_issue_ids": []string{uuidToString(issue.ID)},
	})
	h.GetGitLabMergeRequestDetails(w, r)
}

func (h *Handler) RefreshGitLabProjectBinding(w http.ResponseWriter, r *http.Request) {
	workspaceID, ok := parseUUIDOrBadRequest(w, chi.URLParam(r, "id"), "workspace id")
	if !ok {
		return
	}
	bindingID, ok := parseUUIDOrBadRequest(w, chi.URLParam(r, "bindingId"), "binding id")
	if !ok {
		return
	}
	binding, err := h.Queries.GetGitLabProjectBinding(r.Context(), db.GetGitLabProjectBindingParams{
		ID:          bindingID,
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
	cfg := gitlab.LoadConfigFromEnv()
	if !cfg.Configured() {
		writeError(w, http.StatusServiceUnavailable, "gitlab integration is not configured")
		return
	}
	api, err := newGitLabClient(cfg)
	if err != nil {
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}
	detailAPI, err := detailGitLabAPI(api)
	if err != nil {
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}
	_ = h.Queries.MarkGitLabProjectRefreshStarted(r.Context(), db.MarkGitLabProjectRefreshStartedParams{
		ID:          binding.ID,
		WorkspaceID: binding.WorkspaceID,
	})
	rows, err := h.Queries.ListGitLabMergeRequestsByProjectBinding(r.Context(), db.ListGitLabMergeRequestsByProjectBindingParams{
		WorkspaceID:      binding.WorkspaceID,
		ProjectBindingID: binding.ID,
		Limit:            100,
	})
	if err != nil {
		_ = h.Queries.MarkGitLabProjectRefreshFinished(r.Context(), db.MarkGitLabProjectRefreshFinishedParams{
			ID:               binding.ID,
			WorkspaceID:      binding.WorkspaceID,
			LastRefreshError: strToText(redact.Text(err.Error())),
		})
		writeError(w, http.StatusInternalServerError, "failed to list gitlab merge requests")
		return
	}
	var summary gitLabRefreshSummary
	for _, mr := range rows {
		_, mrSummary, err := h.refreshGitLabMergeRequest(r.Context(), detailAPI, mr)
		summary.UpdatedMRs += mrSummary.UpdatedMRs
		summary.UpdatedJobs += mrSummary.UpdatedJobs
		summary.UpdatedDiscussions += mrSummary.UpdatedDiscussions
		summary.Errors = append(summary.Errors, mrSummary.Errors...)
		if err != nil {
			summary.addError(err)
		}
	}
	var refreshErr pgtype.Text
	if len(summary.Errors) > 0 {
		refreshErr = strToText(strings.Join(summary.Errors, "; "))
	}
	_ = h.Queries.MarkGitLabProjectRefreshFinished(r.Context(), db.MarkGitLabProjectRefreshFinishedParams{
		ID:               binding.ID,
		WorkspaceID:      binding.WorkspaceID,
		LastRefreshError: refreshErr,
	})
	h.publish(protocol.EventGitLabProjectCreated, uuidToString(binding.WorkspaceID), "system", "", map[string]any{
		"project": gitLabProjectBindingToResponse(binding),
	})
	writeJSON(w, http.StatusOK, summary)
}

func (h *Handler) GetGitLabJobTrace(w http.ResponseWriter, r *http.Request) {
	issue, job, mr, ok := h.loadGitLabJobForIssue(w, r)
	if !ok {
		return
	}
	cfg := gitlab.LoadConfigFromEnv()
	if !cfg.Configured() {
		writeError(w, http.StatusServiceUnavailable, "gitlab integration is not configured")
		return
	}
	api, err := newGitLabClient(cfg)
	if err != nil {
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}
	detailAPI, err := detailGitLabAPI(api)
	if err != nil {
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}
	trace, err := detailAPI.GetJobTrace(r.Context(), mr.GitlabProjectID, job.JobID, gitLabFailedJobTraceLimit)
	if err != nil {
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}
	summary := redact.Text(trace.Text)
	_ = h.Queries.UpdateGitLabPipelineJobTraceSummary(r.Context(), db.UpdateGitLabPipelineJobTraceSummaryParams{
		ID:             job.ID,
		WorkspaceID:    issue.WorkspaceID,
		TraceSummary:   strToText(summary),
		TraceTruncated: trace.Truncated,
	})
	writeJSON(w, http.StatusOK, map[string]any{
		"trace_summary":   summary,
		"trace_truncated": trace.Truncated,
	})
}

func (h *Handler) OpenGitLabJobArtifacts(w http.ResponseWriter, r *http.Request) {
	_, job, mr, ok := h.loadGitLabJobForIssue(w, r)
	if !ok {
		return
	}
	if !job.ArtifactsFileName.Valid {
		writeError(w, http.StatusNotFound, "gitlab artifact not found")
		return
	}
	cfg := gitlab.LoadConfigFromEnv()
	if !cfg.Configured() {
		writeError(w, http.StatusServiceUnavailable, "gitlab integration is not configured")
		return
	}
	api, err := newGitLabClient(cfg)
	if err != nil {
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}
	detailAPI, err := detailGitLabAPI(api)
	if err != nil {
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}
	resp, err := detailAPI.DownloadJobArtifacts(r.Context(), mr.GitlabProjectID, job.JobID)
	if err != nil {
		writeError(w, http.StatusBadGateway, err.Error())
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

func (h *Handler) loadGitLabJobForIssue(w http.ResponseWriter, r *http.Request) (db.Issue, db.GitlabPipelineJob, db.GitlabMergeRequest, bool) {
	issue, ok := h.loadIssueForUser(w, r, chi.URLParam(r, "id"))
	if !ok {
		return db.Issue{}, db.GitlabPipelineJob{}, db.GitlabMergeRequest{}, false
	}
	jobID, ok := parseUUIDOrBadRequest(w, chi.URLParam(r, "jobId"), "job id")
	if !ok {
		return db.Issue{}, db.GitlabPipelineJob{}, db.GitlabMergeRequest{}, false
	}
	job, err := h.Queries.GetGitLabPipelineJobForIssue(r.Context(), db.GetGitLabPipelineJobForIssueParams{
		IssueID:     issue.ID,
		ID:          jobID,
		WorkspaceID: issue.WorkspaceID,
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			writeError(w, http.StatusNotFound, "gitlab job not found")
			return db.Issue{}, db.GitlabPipelineJob{}, db.GitlabMergeRequest{}, false
		}
		writeError(w, http.StatusInternalServerError, "failed to load gitlab job")
		return db.Issue{}, db.GitlabPipelineJob{}, db.GitlabMergeRequest{}, false
	}
	mr, err := h.Queries.GetGitLabMergeRequestByID(r.Context(), db.GetGitLabMergeRequestByIDParams{
		ID:          job.MergeRequestID,
		WorkspaceID: issue.WorkspaceID,
	})
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to load gitlab merge request")
		return db.Issue{}, db.GitlabPipelineJob{}, db.GitlabMergeRequest{}, false
	}
	return issue, job, mr, true
}

func gitLabMergeRequestToResponse(row db.GitlabMergeRequest, projectPath string) GitLabMergeRequestResponse {
	return GitLabMergeRequestResponse{
		ID:                  uuidToString(row.ID),
		WorkspaceID:         uuidToString(row.WorkspaceID),
		ProjectPath:         projectPath,
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
		out = append(out, GitLabMRDiscussionResponse{
			ID:                 key,
			GitLabDiscussionID: discussion.GitlabDiscussionID,
			IndividualNote:     discussion.IndividualNote,
			Resolved:           boolPtrFromPg(discussion.Resolved),
			CreatedAt:          timestampToPtr(discussion.DiscussionCreatedAt),
			UpdatedAt:          timestampToPtr(discussion.DiscussionUpdatedAt),
			Notes:              notesByDiscussion[key],
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
