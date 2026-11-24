package handler

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/integrations/vcs"
	"github.com/multica-ai/multica/server/internal/issuestatus"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/multica-ai/multica/server/pkg/protocol"
)

// ── Response mappers ────────────────────────────────────────────────────────

// vcsPullRequestToResponse maps a stored VCS PR onto the shared PR response
// shape for single-PR webhook broadcasts (no aggregated check counts; the
// frontend re-queries the issue's PR list for fresh counts).
func vcsPullRequestToResponse(p db.VcsPullRequest) GitHubPullRequestResponse {
	return GitHubPullRequestResponse{
		ID:               uuidToString(p.ID),
		Provider:         p.Provider,
		WorkspaceID:      uuidToString(p.WorkspaceID),
		RepoOwner:        p.RepoOwner,
		RepoName:         p.RepoName,
		Number:           p.PrNumber,
		Title:            p.Title,
		State:            p.State,
		HtmlURL:          p.HtmlUrl,
		Branch:           textToPtr(p.Branch),
		AuthorLogin:      textToPtr(p.AuthorLogin),
		AuthorAvatarURL:  textToPtr(p.AuthorAvatarUrl),
		MergedAt:         timestampToPtr(p.MergedAt),
		ClosedAt:         timestampToPtr(p.ClosedAt),
		PRCreatedAt:      timestampToString(p.PrCreatedAt),
		PRUpdatedAt:      timestampToString(p.PrUpdatedAt),
		MergeableState:   nil,
		ChecksConclusion: nil,
		Additions:        p.Additions,
		Deletions:        p.Deletions,
		ChangedFiles:     p.ChangedFiles,
	}
}

// vcsPullRequestRowToResponse maps an issue's PR-list row, which carries the
// aggregated commit-status counts, onto the shared response shape.
func vcsPullRequestRowToResponse(p db.ListVCSPullRequestsByIssueRow) GitHubPullRequestResponse {
	return GitHubPullRequestResponse{
		ID:               uuidToString(p.ID),
		Provider:         p.Provider,
		WorkspaceID:      uuidToString(p.WorkspaceID),
		RepoOwner:        p.RepoOwner,
		RepoName:         p.RepoName,
		Number:           p.PrNumber,
		Title:            p.Title,
		State:            p.State,
		HtmlURL:          p.HtmlUrl,
		Branch:           textToPtr(p.Branch),
		AuthorLogin:      textToPtr(p.AuthorLogin),
		AuthorAvatarURL:  textToPtr(p.AuthorAvatarUrl),
		MergedAt:         timestampToPtr(p.MergedAt),
		ClosedAt:         timestampToPtr(p.ClosedAt),
		PRCreatedAt:      timestampToString(p.PrCreatedAt),
		PRUpdatedAt:      timestampToString(p.PrUpdatedAt),
		MergeableState:   nil,
		ChecksConclusion: aggregateChecksConclusion(p.ChecksFailed, p.ChecksPassed, p.ChecksPending, p.ChecksTotal),
		ChecksTotal:      p.ChecksTotal,
		ChecksPassed:     p.ChecksPassed,
		ChecksFailed:     p.ChecksFailed,
		ChecksPending:    p.ChecksPending,
		ChecksRunning:    p.ChecksPending,
		FailedCheckNames: []string{},
		Additions:        p.Additions,
		Deletions:        p.Deletions,
		ChangedFiles:     p.ChangedFiles,
	}
}

// ── Webhook ─────────────────────────────────────────────────────────────────

// HandleVCSWebhook (POST /api/webhooks/vcs/{connectionId}) authenticates and
// mirrors webhooks from any token-based Git provider. The connection id in the path
// selects the workspace, the provider, and the decryption secret; the provider
// adapter handles the provider-specific signature scheme, event header, and
// payload shape, returning normalized events to the shared mirror logic below.
func (h *Handler) HandleVCSWebhook(w http.ResponseWriter, r *http.Request) {
	// Where the integration is off (the managed cloud) the endpoint behaves as
	// if it does not exist — a bare 404 that reveals nothing about config, the
	// same response a genuinely unknown connection id gets below.
	if !h.isVCSAvailable() {
		writeError(w, http.StatusNotFound, "unknown connection")
		return
	}
	if !h.isVCSConfigured() {
		writeError(w, http.StatusNotFound, "unknown connection")
		return
	}
	connUUID, ok := parseUUIDOrBadRequest(w, chi.URLParam(r, "connectionId"), "connection id")
	if !ok {
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 10<<20)) // 10 MiB cap
	if err != nil {
		writeError(w, http.StatusBadRequest, "read body failed")
		return
	}

	conn, err := h.Queries.GetVCSConnectionByID(r.Context(), connUUID)
	if err != nil {
		if !errors.Is(err, pgx.ErrNoRows) {
			slog.Warn("vcs: lookup connection failed", "err", err)
		}
		writeError(w, http.StatusNotFound, "unknown connection")
		return
	}
	provider, ok := vcs.For(conn.Provider)
	if !ok {
		slog.Error("vcs: connection has unknown provider", "provider", conn.Provider)
		writeError(w, http.StatusInternalServerError, "unknown provider")
		return
	}

	secret, err := h.openVCSSecret(conn.WebhookSecretEncrypted)
	if err != nil {
		slog.Error("vcs: decrypt webhook secret failed", "err", err)
		writeError(w, http.StatusInternalServerError, "secret error")
		return
	}
	if !provider.VerifySignature(secret, r.Header, body) {
		writeError(w, http.StatusUnauthorized, "invalid signature")
		return
	}
	if conn.Provider == string(vcs.KindGitLab) {
		maintenance, currentConn, receiptErr := h.persistGitLabMigrationDeliveryIfNeeded(r.Context(), conn, r, body)
		if receiptErr != nil {
			slog.Warn("vcs: migration webhook receipt failed", "err", receiptErr)
			writeError(w, http.StatusServiceUnavailable, "integration maintenance")
			return
		}
		conn = currentConn
		provider, ok = vcs.For(conn.Provider)
		if !ok {
			writeError(w, http.StatusInternalServerError, "unknown provider")
			return
		}
		if maintenance {
			// A stored receipt is not an acknowledgement: GitLab must retry while
			// maintenance is active so rollback can resume delivery on the old path.
			writeError(w, http.StatusServiceUnavailable, "integration maintenance; delivery recorded for reconciliation; retry")
			return
		}
	} else {
		migrationState, err := h.Queries.GetVCSGitLabMigrationState(r.Context())
		if err != nil {
			slog.Error("vcs: migration state unavailable", "err", err)
			writeError(w, http.StatusServiceUnavailable, "integration maintenance")
			return
		}
		if migrationState.Phase != "complete" || migrationState.MaintenanceMode {
			writeError(w, http.StatusServiceUnavailable, "integration maintenance")
			return
		}
	}

	systemHook := conn.Provider == string(vcs.KindGitLab) && r.Header.Get("X-Gitlab-Event") == "System Hook"
	switch provider.EventKind(r.Header, body) {
	case vcs.EventPullRequest:
		pr, err := provider.ParsePullRequest(body)
		if err != nil {
			slog.Warn("vcs: bad pull_request payload", "provider", conn.Provider, "err", err)
			if conn.Provider == string(vcs.KindGitLab) {
				writeError(w, http.StatusBadRequest, "invalid merge request payload")
				return
			}
			break
		}
		if pr.RepoOwner == "" || pr.RepoName == "" || pr.Number <= 0 {
			if conn.Provider == string(vcs.KindGitLab) {
				writeError(w, http.StatusBadRequest, "merge request identity missing")
				return
			}
			break
		}
		if systemHook {
			allowed, err := h.authorizeGitLabSystemHook(r.Context(), conn, provider, pr.RepoOwner, pr.RepoName, nil)
			if err != nil {
				slog.Warn("vcs: system-hook repository validation failed", "reason", gitLabWakeupFailureMessage(err))
				writeError(w, gitLabSystemHookFailureStatus(err), "system hook validation failed")
				return
			}
			if !allowed {
				break
			}
		}
		if conn.Provider == string(vcs.KindGitLab) && !systemHook {
			projectID, projectPath, identityErr := gitLabWebhookProjectIdentity(body)
			if identityErr != nil {
				slog.Warn("vcs: GitLab merge request identity failed", "err", identityErr)
				writeError(w, http.StatusBadRequest, "merge request project identity missing")
				return
			}
			if projectPath != pr.RepoOwner+"/"+pr.RepoName {
				writeError(w, http.StatusBadRequest, "merge request project identity mismatch")
				return
			}
			allowed, err := h.authorizeGitLabWebhookProject(r.Context(), conn, projectID, projectPath)
			if err != nil {
				slog.Warn("vcs: GitLab project validation failed", "reason", gitLabWakeupFailureMessage(err))
				writeError(w, http.StatusServiceUnavailable, "project validation failed")
				return
			}
			if !allowed {
				writeError(w, http.StatusNotFound, "unknown project")
				return
			}
		}
		if err := h.mirrorVCSPullRequest(r.Context(), conn, pr); err != nil {
			if conn.Provider == string(vcs.KindGitLab) {
				slog.Warn("vcs: merge request mirror failed", "reason", gitLabWakeupFailureMessage(err))
				writeError(w, http.StatusServiceUnavailable, "merge request sync failed")
				return
			}
			slog.Warn("vcs: upsert pr failed", "err", err)
		}
	case vcs.EventCIStatus:
		status, err := provider.ParseCIStatus(body)
		if err != nil {
			slog.Warn("vcs: bad status payload", "provider", conn.Provider, "err", err)
			if conn.Provider == string(vcs.KindGitLab) {
				writeError(w, http.StatusBadRequest, "invalid pipeline payload")
				return
			}
			break
		}
		if conn.Provider == string(vcs.KindGitLab) && (status.SHA == "" || status.PipelineID <= 0 || status.ProjectID <= 0 || status.RepoOwner == "" || status.RepoName == "") {
			writeError(w, http.StatusBadRequest, "pipeline identity missing")
			return
		}
		if systemHook {
			allowed, err := h.authorizeGitLabSystemHook(r.Context(), conn, provider, status.RepoOwner, status.RepoName, &status)
			if err != nil {
				slog.Warn("vcs: system-hook pipeline validation failed", "reason", gitLabWakeupFailureMessage(err))
				writeError(w, gitLabSystemHookFailureStatus(err), "system hook validation failed")
				return
			}
			if !allowed {
				break
			}
		}
		if conn.Provider == string(vcs.KindGitLab) && !systemHook {
			projectPath := status.RepoOwner + "/" + status.RepoName
			allowed, err := h.authorizeGitLabWebhookProject(r.Context(), conn, status.ProjectID, projectPath)
			if err != nil {
				slog.Warn("vcs: GitLab pipeline project validation failed", "reason", gitLabWakeupFailureMessage(err))
				writeError(w, http.StatusServiceUnavailable, "project validation failed")
				return
			}
			if !allowed {
				writeError(w, http.StatusNotFound, "unknown project")
				return
			}
		}
		if err := h.mirrorVCSCIStatus(r.Context(), conn, status); err != nil {
			if conn.Provider == string(vcs.KindGitLab) {
				slog.Warn("vcs: pipeline mirror failed", "reason", gitLabWakeupFailureMessage(err))
				writeError(w, http.StatusServiceUnavailable, "pipeline sync failed")
				return
			}
			slog.Warn("vcs: upsert commit status failed", "err", err)
		}
	default:
		if conn.Provider == string(vcs.KindGitLab) {
			switch r.Header.Get("X-Gitlab-Event") {
			case "Job Hook":
				if err := h.syncGitLabJobWebhook(r.Context(), conn, body); err != nil {
					slog.Warn("vcs: GitLab job hook sync failed", "err", err)
					writeError(w, http.StatusServiceUnavailable, "job sync failed")
					return
				}
			case "Note Hook":
				if err := h.syncGitLabNoteWebhook(r.Context(), conn, body); err != nil {
					slog.Warn("vcs: GitLab note hook sync failed", "err", err)
					writeError(w, http.StatusServiceUnavailable, "note sync failed")
					return
				}
			}
		}
	}
	w.WriteHeader(http.StatusAccepted)
}

type gitLabWebhookJobPayload struct {
	Project struct {
		ID                int64  `json:"id"`
		PathWithNamespace string `json:"path_with_namespace"`
	} `json:"project"`
	ProjectID          int64  `json:"project_id"`
	Ref                string `json:"ref"`
	SHA                string `json:"sha"`
	BuildID            int64  `json:"build_id"`
	BuildName          string `json:"build_name"`
	BuildStage         string `json:"build_stage"`
	BuildStatus        string `json:"build_status"`
	BuildStartedAt     string `json:"build_started_at"`
	BuildFinishedAt    string `json:"build_finished_at"`
	BuildAllowFailure  bool   `json:"build_allow_failure"`
	BuildFailureReason string `json:"build_failure_reason"`
	PipelineID         int64  `json:"pipeline_id"`
	Pipeline           struct {
		ID  int64  `json:"id"`
		Ref string `json:"ref"`
		SHA string `json:"sha"`
	} `json:"pipeline"`
	ObjectAttributes struct {
		ID                int64    `json:"id"`
		Name              string   `json:"name"`
		Stage             string   `json:"stage"`
		Status            string   `json:"status"`
		Ref               string   `json:"ref"`
		SHA               string   `json:"sha"`
		WebURL            string   `json:"web_url"`
		StartedAt         string   `json:"started_at"`
		FinishedAt        string   `json:"finished_at"`
		Duration          *float64 `json:"duration"`
		QueuedDuration    *float64 `json:"queued_duration"`
		FailureReason     string   `json:"failure_reason"`
		AllowFailure      bool     `json:"allow_failure"`
		PipelineID        int64    `json:"pipeline_id"`
		ArtifactsExpireAt string   `json:"artifacts_expire_at"`
		ArtifactsFile     struct {
			Filename string `json:"filename"`
			Size     int64  `json:"size"`
		} `json:"artifacts_file"`
	} `json:"object_attributes"`
}

type gitLabWebhookNotePayload struct {
	Project struct {
		ID                int64  `json:"id"`
		PathWithNamespace string `json:"path_with_namespace"`
	} `json:"project"`
	User struct {
		Username  string `json:"username"`
		AvatarURL string `json:"avatar_url"`
	} `json:"user"`
	ObjectAttributes struct {
		ID           int64  `json:"id"`
		DiscussionID string `json:"discussion_id"`
		Note         string `json:"note"`
		UpdatedAt    string `json:"updated_at"`
		CreatedAt    string `json:"created_at"`
		System       bool   `json:"system"`
		Resolved     *bool  `json:"resolved"`
		Resolvable   *bool  `json:"resolvable"`
	} `json:"object_attributes"`
	MergeRequest struct {
		IID int32 `json:"iid"`
	} `json:"merge_request"`
}

func (h *Handler) syncGitLabJobWebhook(ctx context.Context, conn db.VcsConnection, body []byte) error {
	var payload gitLabWebhookJobPayload
	if err := json.Unmarshal(body, &payload); err != nil {
		return fmt.Errorf("decode GitLab job hook: %w", err)
	}
	path := strings.Trim(payload.Project.PathWithNamespace, "/")
	projectID := payload.Project.ID
	if projectID == 0 {
		projectID = payload.ProjectID
	}
	jobID, pipelineID := payload.ObjectAttributes.ID, payload.ObjectAttributes.PipelineID
	if jobID == 0 {
		jobID = payload.BuildID
	}
	if pipelineID == 0 {
		pipelineID = payload.Pipeline.ID
	}
	if path == "" || projectID <= 0 || jobID <= 0 || pipelineID <= 0 {
		return errors.New("GitLab job hook identity is incomplete")
	}
	owner, name, valid := splitGitLabProjectPath(path)
	if !valid {
		return errors.New("GitLab job hook project path is invalid")
	}
	allowed, err := h.authorizeGitLabWebhookProject(ctx, conn, projectID, path)
	if err != nil || !allowed {
		return errors.Join(errors.New("GitLab job hook project is not authorized"), err)
	}
	sha := firstGitLabWebhookValue(payload.ObjectAttributes.SHA, payload.Pipeline.SHA, payload.SHA)
	ref := firstGitLabWebhookValue(payload.ObjectAttributes.Ref, payload.Pipeline.Ref, payload.Ref)
	if sha == "" {
		return errors.New("GitLab job hook SHA is missing")
	}
	nameValue := firstGitLabWebhookValue(payload.ObjectAttributes.Name, payload.BuildName)
	stage := firstGitLabWebhookValue(payload.ObjectAttributes.Stage, payload.BuildStage)
	status := firstGitLabWebhookValue(payload.ObjectAttributes.Status, payload.BuildStatus)
	if status == "" {
		return errors.New("GitLab job hook status is missing")
	}
	startedAt := firstGitLabWebhookValue(payload.ObjectAttributes.StartedAt, payload.BuildStartedAt)
	finishedAt := firstGitLabWebhookValue(payload.ObjectAttributes.FinishedAt, payload.BuildFinishedAt)
	failureReason := firstGitLabWebhookValue(payload.ObjectAttributes.FailureReason, payload.BuildFailureReason)
	allowFailure := payload.ObjectAttributes.AllowFailure || payload.BuildAllowFailure
	// The authenticated job payload is complete; associate it with the signed
	// project and current MR head/ref without requiring a GitLab API refresh.
	candidates, err := h.gitLabWebhookCandidates(ctx, conn, owner, name, projectID, sha, ref, 0)
	if err != nil {
		return err
	}
	for _, pr := range candidates {
		duplicate, err := h.Queries.IsGitLabPipelineJobEventDuplicate(ctx, db.IsGitLabPipelineJobEventDuplicateParams{
			WorkspaceID: conn.WorkspaceID, PullRequestID: pr.ID, JobID: jobID, PipelineID: pipelineID,
			Name: nameValue, Stage: strToText(stage), Status: status, Ref: strToText(ref), Sha: strToText(sha),
			StartedAt: parseGitLabTime(startedAt), FinishedAt: parseGitLabTime(finishedAt),
			FailureReason: strToText(failureReason), AllowFailure: allowFailure,
		})
		if err != nil {
			return err
		}
		if duplicate {
			continue
		}
		artifactSize := pgtype.Int8{Int64: payload.ObjectAttributes.ArtifactsFile.Size}
		artifactSize.Valid = payload.ObjectAttributes.ArtifactsFile.Filename != "" || payload.ObjectAttributes.ArtifactsFile.Size != 0
		_, err = h.Queries.UpsertGitLabPipelineJobByPullRequest(ctx, db.UpsertGitLabPipelineJobByPullRequestParams{
			WorkspaceID: pr.WorkspaceID, PullRequestID: pr.ID, WakeupSyncGeneration: pr.WakeupSyncGeneration,
			PipelineID: pipelineID, JobID: jobID, Name: nameValue, Stage: strToText(stage), Status: status,
			Ref: strToText(ref), Sha: strToText(sha), WebUrl: strToText(payload.ObjectAttributes.WebURL),
			StartedAt: parseGitLabTime(startedAt), FinishedAt: parseGitLabTime(finishedAt),
			DurationSeconds:       float64PtrToPg(payload.ObjectAttributes.Duration),
			QueuedDurationSeconds: float64PtrToPg(payload.ObjectAttributes.QueuedDuration),
			FailureReason:         strToText(firstGitLabWebhookValue(payload.ObjectAttributes.FailureReason, payload.BuildFailureReason)),
			AllowFailure:          allowFailure, ArtifactsFileName: strToText(payload.ObjectAttributes.ArtifactsFile.Filename),
			ArtifactsFileSize: artifactSize, ArtifactsExpireAt: parseGitLabTime(payload.ObjectAttributes.ArtifactsExpireAt),
		})
		if errors.Is(err, pgx.ErrNoRows) {
			continue
		}
		if err != nil {
			return wrapGitLabRefreshPersistenceError(err)
		}
		h.publishGitLabVCSDetailUpdated(ctx, conn.WorkspaceID, pr.ID)
	}
	return nil
}

func (h *Handler) syncGitLabNoteWebhook(ctx context.Context, conn db.VcsConnection, body []byte) error {
	var payload gitLabWebhookNotePayload
	if err := json.Unmarshal(body, &payload); err != nil {
		return fmt.Errorf("decode GitLab note hook: %w", err)
	}
	path := strings.Trim(payload.Project.PathWithNamespace, "/")
	if path == "" || payload.Project.ID <= 0 || payload.MergeRequest.IID <= 0 || payload.ObjectAttributes.ID <= 0 {
		return errors.New("GitLab note hook identity is incomplete")
	}
	owner, name, valid := splitGitLabProjectPath(path)
	if !valid {
		return errors.New("GitLab note hook project path is invalid")
	}
	allowed, err := h.authorizeGitLabWebhookProject(ctx, conn, payload.Project.ID, path)
	if err != nil || !allowed {
		return errors.Join(errors.New("GitLab note hook project is not authorized"), err)
	}
	pr, err := h.Queries.GetVCSPullRequestByKey(ctx, db.GetVCSPullRequestByKeyParams{
		ConnectionID: conn.ID, RepoOwner: owner, RepoName: name, PrNumber: payload.MergeRequest.IID,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	if pr.WorkspaceID != conn.WorkspaceID || pr.Provider != string(vcs.KindGitLab) {
		return errors.New("GitLab note hook resolved outside its connection workspace")
	}
	updatedAt := payload.ObjectAttributes.UpdatedAt
	if updatedAt == "" {
		updatedAt = payload.ObjectAttributes.CreatedAt
	}
	duplicate, err := h.Queries.IsGitLabMRNoteEventDuplicate(ctx, db.IsGitLabMRNoteEventDuplicateParams{
		WorkspaceID: conn.WorkspaceID, PullRequestID: pr.ID, NoteID: payload.ObjectAttributes.ID,
		NoteBody: payload.ObjectAttributes.Note, NoteUpdatedAt: parseGitLabTime(updatedAt),
	})
	if err != nil {
		return err
	}
	if duplicate {
		return nil
	}
	// Persist the Note Hook's event content against this connection-scoped MR;
	// an optional remote refresh must not gate delivery of the note itself.
	if discussionKey := strings.TrimSpace(payload.ObjectAttributes.DiscussionID); discussionKey != "" {
		discussions, err := h.Queries.ListGitLabMRDiscussionsByPullRequest(ctx, db.ListGitLabMRDiscussionsByPullRequestParams{
			WorkspaceID: pr.WorkspaceID, PullRequestID: pr.ID,
		})
		if err != nil {
			return wrapGitLabRefreshPersistenceError(err)
		}
		var discussionID pgtype.UUID
		for _, discussion := range discussions {
			if discussion.GitlabDiscussionID == discussionKey {
				discussionID = discussion.ID
				break
			}
		}
		if !discussionID.Valid {
			discussion, err := h.Queries.UpsertGitLabMRDiscussionByPullRequest(ctx, db.UpsertGitLabMRDiscussionByPullRequestParams{
				WorkspaceID: pr.WorkspaceID, PullRequestID: pr.ID, WakeupSyncGeneration: pr.WakeupSyncGeneration,
				GitlabDiscussionID: discussionKey, IndividualNote: true,
				Resolved:            boolToPg(payload.ObjectAttributes.Resolved),
				DiscussionCreatedAt: parseGitLabTime(payload.ObjectAttributes.CreatedAt),
				DiscussionUpdatedAt: parseGitLabTime(updatedAt),
			})
			if errors.Is(err, pgx.ErrNoRows) {
				return nil
			}
			if err != nil {
				return wrapGitLabRefreshPersistenceError(err)
			}
			discussionID = discussion.ID
		}
		_, err = h.Queries.UpsertGitLabMRNoteByPullRequest(ctx, db.UpsertGitLabMRNoteByPullRequestParams{
			WorkspaceID: pr.WorkspaceID, DiscussionID: discussionID, PullRequestID: pr.ID,
			WakeupSyncGeneration: pr.WakeupSyncGeneration, GitlabNoteID: payload.ObjectAttributes.ID,
			Body: payload.ObjectAttributes.Note, System: payload.ObjectAttributes.System,
			AuthorUsername: strToText(payload.User.Username), AuthorAvatarUrl: strToText(payload.User.AvatarURL),
			Resolved: boolToPg(payload.ObjectAttributes.Resolved), Resolvable: boolToPg(payload.ObjectAttributes.Resolvable),
			NoteCreatedAt: parseGitLabTime(payload.ObjectAttributes.CreatedAt), NoteUpdatedAt: parseGitLabTime(updatedAt),
		})
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return wrapGitLabRefreshPersistenceError(err)
		}
	}
	h.publishGitLabVCSDetailUpdated(ctx, conn.WorkspaceID, pr.ID)
	return nil
}

func (h *Handler) gitLabWebhookCandidates(ctx context.Context, conn db.VcsConnection, owner, name string, projectID int64, sha, ref string, iid int32) ([]db.VcsPullRequest, error) {
	if iid > 0 {
		pr, err := h.Queries.GetVCSPullRequestByKey(ctx, db.GetVCSPullRequestByKeyParams{
			ConnectionID: conn.ID, RepoOwner: owner, RepoName: name, PrNumber: iid,
		})
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, nil
		}
		if err != nil {
			return nil, err
		}
		if pr.WorkspaceID != conn.WorkspaceID || pr.Provider != string(vcs.KindGitLab) {
			return nil, errors.New("GitLab webhook resolved outside its connection workspace")
		}
		return []db.VcsPullRequest{pr}, nil
	}
	if sha == "" {
		return nil, nil
	}
	candidates := make(map[string]db.VcsPullRequest)
	byProject, err := h.Queries.ListVCSGitLabPullRequestsByProjectHead(ctx, db.ListVCSGitLabPullRequestsByProjectHeadParams{
		WorkspaceID: conn.WorkspaceID, ConnectionID: conn.ID, RepoOwner: owner, RepoName: name, HeadSha: sha,
	})
	if err != nil {
		return nil, err
	}
	for _, pr := range byProject {
		if vcsGitLabPipelineRefMayMatchMR(ref, pr) {
			candidates[uuidToString(pr.ID)] = pr
		}
	}
	bySource, err := h.Queries.ListVCSGitLabPullRequestsBySourceProjectHead(ctx, db.ListVCSGitLabPullRequestsBySourceProjectHeadParams{
		WorkspaceID: conn.WorkspaceID, ConnectionID: conn.ID,
		SourceProjectID: pgtype.Int8{Int64: projectID, Valid: projectID > 0}, HeadSha: sha,
	})
	if err != nil {
		return nil, err
	}
	for _, pr := range bySource {
		if vcsGitLabPipelineRefMayMatchMR(ref, pr) {
			candidates[uuidToString(pr.ID)] = pr
		}
	}
	out := make([]db.VcsPullRequest, 0, len(candidates))
	for _, pr := range candidates {
		out = append(out, pr)
	}
	return out, nil
}

func (h *Handler) publishGitLabVCSDetailUpdated(ctx context.Context, workspaceID, pullRequestID pgtype.UUID) {
	issueIDs, err := h.Queries.ListIssueIDsForVCSPullRequest(ctx, pullRequestID)
	if err != nil {
		slog.Warn("vcs: list GitLab detail issue links failed", "err", err)
		return
	}
	for _, issueID := range issueIDs {
		h.publish(protocol.EventPullRequestUpdated, uuidToString(workspaceID), "system", "", map[string]any{
			"issue_id": uuidToString(issueID),
		})
	}
}

func firstGitLabWebhookValue(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}

func (h *Handler) persistGitLabMigrationDeliveryIfNeeded(ctx context.Context, conn db.VcsConnection, r *http.Request, body []byte) (bool, db.VcsConnection, error) {
	if h.TxStarter == nil {
		return false, conn, errors.New("GitLab migration transaction starter is unavailable")
	}
	tx, err := h.TxStarter.Begin(ctx)
	if err != nil {
		return false, conn, fmt.Errorf("begin GitLab migration ingress transaction: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	qtx := h.Queries.WithTx(tx)
	state, err := qtx.GetVCSGitLabMigrationStateForShare(ctx)
	if err != nil {
		return false, conn, fmt.Errorf("lock GitLab migration state: %w", err)
	}
	currentConn, err := qtx.GetVCSConnectionByID(ctx, conn.ID)
	if err != nil {
		return false, conn, fmt.Errorf("reload GitLab connection under migration lock: %w", err)
	}
	if currentConn.Provider != string(vcs.KindGitLab) {
		return false, conn, errors.New("webhook connection is no longer GitLab")
	}
	currentProvider, ok := vcs.For(currentConn.Provider)
	if !ok {
		return false, conn, errors.New("GitLab provider is unavailable")
	}
	currentSecret, err := h.openVCSSecret(currentConn.WebhookSecretEncrypted)
	if err != nil {
		return false, conn, fmt.Errorf("reload GitLab webhook secret under migration lock: %w", err)
	}
	if !currentProvider.VerifySignature(currentSecret, r.Header, body) {
		return false, conn, errors.New("GitLab webhook signature changed during migration cutover")
	}
	if state.Phase == "complete" && !state.MaintenanceMode {
		if err := tx.Commit(ctx); err != nil {
			return false, conn, fmt.Errorf("release GitLab migration state lock: %w", err)
		}
		return false, currentConn, nil
	}
	if !state.MaintenanceMode {
		return false, currentConn, errors.New("GitLab migration is not complete and maintenance is disabled")
	}
	if err := h.persistGitLabMigrationDelivery(ctx, qtx, currentConn, r, body); err != nil {
		return false, currentConn, err
	}
	if err := tx.Commit(ctx); err != nil {
		return false, currentConn, fmt.Errorf("commit GitLab migration webhook receipt: %w", err)
	}
	return true, currentConn, nil
}

func (h *Handler) persistGitLabMigrationDelivery(ctx context.Context, queries *db.Queries, conn db.VcsConnection, r *http.Request, body []byte) error {
	query := r.URL.Query()
	scopeType := query.Get("scope_type")
	scopeID := strings.TrimSpace(query.Get("scope_id"))
	if scopeID == "" {
		return errors.New("migration webhook scope identity is missing")
	}
	hookID, err := strconv.ParseInt(query.Get("hook_id"), 10, 64)
	if err != nil || hookID <= 0 {
		return errors.New("migration webhook hook identity is missing")
	}
	switch scopeType {
	case "project", "group", "system":
	default:
		return errors.New("migration webhook scope type is invalid")
	}
	hook, err := queries.GetGitLabMigrationHookForDelivery(ctx, db.GetGitLabMigrationHookForDeliveryParams{
		WorkspaceID: conn.WorkspaceID, ConnectionID: conn.ID,
		ScopeType: scopeType, ScopeID: scopeID, HookID: hookID,
	})
	if err != nil {
		return fmt.Errorf("migration hook is not registered for this connection and workspace: %w", err)
	}
	var envelope struct {
		ObjectKind string `json:"object_kind"`
		Project    struct {
			ID                int64  `json:"id"`
			PathWithNamespace string `json:"path_with_namespace"`
		} `json:"project"`
		ProjectID int64 `json:"project_id"`
	}
	if err := json.Unmarshal(body, &envelope); err != nil {
		return fmt.Errorf("decode migration webhook payload: %w", err)
	}
	projectID := envelope.Project.ID
	if projectID == 0 {
		projectID = envelope.ProjectID
	}
	projectPath := strings.Trim(envelope.Project.PathWithNamespace, "/")
	if projectPath == "" || projectID <= 0 {
		return errors.New("migration webhook payload has no project identity")
	}
	if scopeType == "project" {
		scopeProjectID, parseErr := strconv.ParseInt(scopeID, 10, 64)
		if parseErr != nil || scopeProjectID <= 0 || projectID != scopeProjectID || projectPath != strings.Trim(hook.ScopePath, "/") {
			return errors.New("migration project hook does not match webhook payload")
		}
	}
	if scopeType == "group" {
		groupPath := strings.Trim(hook.ScopePath, "/")
		if groupPath == "" || (projectPath != groupPath && !strings.HasPrefix(projectPath, groupPath+"/")) {
			return errors.New("migration group hook does not cover webhook project")
		}
	}
	eventType := r.Header.Get("X-Gitlab-Event")
	if eventType == "" {
		return errors.New("GitLab event type is missing")
	}
	if eventType == "System Hook" && envelope.ObjectKind == "" {
		return errors.New("system hook object_kind is missing")
	}
	persistedEventType := eventType
	if eventType == "System Hook" {
		persistedEventType += "/" + envelope.ObjectKind
	}
	owner, name, valid := splitGitLabProjectPath(projectPath)
	if !valid {
		return errors.New("webhook project path is invalid")
	}
	if scopeType == "system" {
		provider, ok := vcs.For(conn.Provider)
		if !ok || conn.Provider != string(vcs.KindGitLab) {
			return errors.New("GitLab provider is unavailable")
		}
		var pipeline *vcs.CIStatusEvent
		if provider.EventKind(r.Header, body) == vcs.EventCIStatus {
			parsed, parseErr := provider.ParseCIStatus(body)
			if parseErr != nil {
				return fmt.Errorf("decode GitLab system pipeline event: %w", parseErr)
			}
			pipeline = &parsed
		}
		allowed, err := h.authorizeGitLabSystemHookForReceipt(ctx, conn, provider, owner, name, pipeline)
		if err != nil || !allowed {
			return errors.Join(errors.New("system hook project is not authorized for this workspace"), err)
		}
	} else {
		allowed, err := h.authorizeGitLabWebhookProject(ctx, conn, projectID, projectPath)
		if err != nil || !allowed {
			return errors.Join(errors.New("webhook project is not authorized for this workspace"), err)
		}
	}
	deliveryID := strings.TrimSpace(r.Header.Get("X-Gitlab-Webhook-UUID"))
	if deliveryID == "" {
		hasher := sha256.New()
		for _, part := range []string{uuidToString(conn.ID), scopeType, scopeID, strconv.FormatInt(hookID, 10), persistedEventType} {
			_, _ = hasher.Write([]byte(part))
			_, _ = hasher.Write([]byte{0})
		}
		_, _ = hasher.Write(body)
		deliveryID = "sha256:" + hex.EncodeToString(hasher.Sum(nil))
	}
	if len(deliveryID) > 255 {
		return errors.New("GitLab delivery identity is too long")
	}
	_, err = queries.UpsertVCSGitLabMigrationDelivery(ctx, db.UpsertVCSGitLabMigrationDeliveryParams{
		DeliveryID: deliveryID, WorkspaceID: conn.WorkspaceID, ConnectionID: conn.ID,
		ScopeType: scopeType, ScopeID: scopeID, ScopePath: hook.ScopePath, HookID: hookID,
		EventType: persistedEventType, Payload: json.RawMessage(body),
	})
	if err != nil {
		return fmt.Errorf("persist GitLab migration webhook receipt: %w", err)
	}
	return nil
}

// ReplayGitLabMigrationDelivery reconciles every receipt sharing a delivery UUID
// while maintenance remains active. The composite key keeps deliveries from
// project/group/system hooks independent.
func (h *Handler) ReplayGitLabMigrationDelivery(ctx context.Context, deliveryID string) error {
	deliveryID = strings.TrimSpace(deliveryID)
	if deliveryID == "" || len(deliveryID) > 255 {
		return errors.New("GitLab delivery replay identity is invalid")
	}
	if h.TxStarter == nil {
		return errors.New("GitLab delivery replay transaction starter is unavailable")
	}
	state, err := h.Queries.GetVCSGitLabMigrationState(ctx)
	if err != nil {
		return err
	}
	if !state.MaintenanceMode || !gitLabMigrationReplayPhaseAllowed(state.Phase) {
		return errors.New("GitLab delivery replay requires migration maintenance mode")
	}

	for {
		deliveries, err := h.Queries.ListGitLabMigrationDeliveriesByID(ctx, deliveryID)
		if err != nil {
			return err
		}
		if len(deliveries) == 0 {
			return nil
		}
		for _, pending := range deliveries {
			tx, err := h.TxStarter.Begin(ctx)
			if err != nil {
				return err
			}
			qtx := h.Queries.WithTx(tx)
			state, err := qtx.GetVCSGitLabMigrationStateForShare(ctx)
			if err != nil {
				_ = tx.Rollback(ctx)
				return err
			}
			if !state.MaintenanceMode || !gitLabMigrationReplayPhaseAllowed(state.Phase) {
				_ = tx.Rollback(ctx)
				return errors.New("GitLab migration left maintenance before receipt replay")
			}
			locked, err := qtx.GetGitLabMigrationDeliveryForUpdate(ctx, db.GetGitLabMigrationDeliveryForUpdateParams{
				DeliveryID: pending.DeliveryID, WorkspaceID: pending.WorkspaceID,
				ConnectionID: pending.ConnectionID, ScopeType: pending.ScopeType,
				ScopeID: pending.ScopeID, HookID: pending.HookID,
			})
			if err != nil {
				_ = tx.Rollback(ctx)
				return err
			}
			if locked.ReconciledAt.Valid {
				if err := tx.Commit(ctx); err != nil {
					_ = tx.Rollback(ctx)
					return err
				}
				continue
			}
			if len(locked.Payload) == 0 || !json.Valid(locked.Payload) {
				_ = tx.Rollback(ctx)
				return errors.New("GitLab migration receipt has no valid retained payload")
			}
			conn, err := qtx.GetVCSConnectionByID(ctx, locked.ConnectionID)
			if err == nil && (conn.WorkspaceID != locked.WorkspaceID || conn.Provider != string(vcs.KindGitLab)) {
				err = errors.New("GitLab receipt connection is outside its recorded workspace or provider")
			}
			if err == nil {
				hook, hookErr := qtx.GetGitLabMigrationHookForDelivery(ctx, db.GetGitLabMigrationHookForDeliveryParams{
					WorkspaceID: locked.WorkspaceID, ConnectionID: locked.ConnectionID,
					ScopeType: locked.ScopeType, ScopeID: locked.ScopeID, HookID: locked.HookID,
				})
				if hookErr != nil {
					err = hookErr
				} else if hook.ScopePath != locked.ScopePath {
					err = errors.New("GitLab receipt scope path no longer matches its registered hook")
				}
			}
			if err == nil {
				err = h.replayGitLabMigrationPayload(ctx, conn, locked.ScopeType, locked.ScopeID, locked.ScopePath, locked.EventType, locked.Payload)
			}
			if err == nil {
				var rows int64
				rows, err = qtx.MarkGitLabMigrationDeliveryReconciled(ctx, db.MarkGitLabMigrationDeliveryReconciledParams{
					DeliveryID: locked.DeliveryID, WorkspaceID: locked.WorkspaceID,
					ConnectionID: locked.ConnectionID, ScopeType: locked.ScopeType,
					ScopeID: locked.ScopeID, HookID: locked.HookID,
					ReconciliationNote: strToText("replayed from authoritative GitLab state"),
				})
				if err == nil && rows != 1 {
					err = errors.New("GitLab delivery receipt changed during replay")
				}
			}
			if err == nil {
				err = tx.Commit(ctx)
				if err != nil {
					_ = tx.Rollback(ctx)
				}
			} else {
				_ = tx.Rollback(ctx)
			}
			if err != nil {
				return err
			}
		}
	}
}

func gitLabMigrationReplayPhaseAllowed(phase string) bool {
	switch phase {
	case "maintenance", "database_migrated", "hooks_pending", "verification_pending", "ready":
		return true
	default:
		return false
	}
}

func (h *Handler) replayGitLabMigrationPayload(ctx context.Context, conn db.VcsConnection, scopeType, scopeID, scopePath, eventType string, raw []byte) error {
	provider, ok := vcs.For(conn.Provider)
	if !ok || conn.Provider != string(vcs.KindGitLab) {
		return errors.New("migration receipt connection is not GitLab")
	}
	var envelope struct {
		ObjectKind string `json:"object_kind"`
		Project    struct {
			ID                int64  `json:"id"`
			PathWithNamespace string `json:"path_with_namespace"`
		} `json:"project"`
		ProjectID int64 `json:"project_id"`
	}
	if err := json.Unmarshal(raw, &envelope); err != nil {
		return err
	}
	projectID := envelope.Project.ID
	if projectID == 0 {
		projectID = envelope.ProjectID
	}
	path := strings.Trim(envelope.Project.PathWithNamespace, "/")
	if path == "" || projectID <= 0 {
		return errors.New("migration receipt project identity is incomplete")
	}
	switch scopeType {
	case "project":
		scopeProjectID, err := strconv.ParseInt(scopeID, 10, 64)
		if err != nil || scopeProjectID <= 0 || projectID != scopeProjectID || path != strings.Trim(scopePath, "/") {
			return errors.New("migration receipt project scope does not match")
		}
	case "group":
		groupPath := strings.Trim(scopePath, "/")
		if groupPath == "" || (path != groupPath && !strings.HasPrefix(path, groupPath+"/")) {
			return errors.New("migration receipt group scope does not match")
		}
	case "system":
	default:
		return errors.New("migration receipt scope is invalid")
	}
	owner, name, valid := splitGitLabProjectPath(path)
	if !valid {
		return errors.New("migration receipt project path is invalid")
	}

	system := strings.HasPrefix(eventType, "System Hook/")
	if system {
		objectKind := strings.TrimPrefix(eventType, "System Hook/")
		if objectKind == "" || (envelope.ObjectKind != "" && objectKind != envelope.ObjectKind) {
			return errors.New("migration system-hook event type does not match its payload")
		}
		eventType = "System Hook"
	}
	headers := make(http.Header)
	headers.Set("X-Gitlab-Event", eventType)
	var pipeline *vcs.CIStatusEvent
	if scopeType == "system" && provider.EventKind(headers, raw) == vcs.EventCIStatus {
		parsed, err := provider.ParseCIStatus(raw)
		if err != nil {
			return err
		}
		pipeline = &parsed
	}
	if scopeType == "system" {
		allowed, err := h.authorizeGitLabSystemHookForReceipt(ctx, conn, provider, owner, name, pipeline)
		if err != nil || !allowed {
			return errors.Join(errors.New("migration system-hook project is not authorized"), err)
		}
	} else {
		allowed, err := h.authorizeGitLabWebhookProject(ctx, conn, projectID, path)
		if err != nil || !allowed {
			return errors.Join(errors.New("migration webhook project is not authorized"), err)
		}
	}

	switch provider.EventKind(headers, raw) {
	case vcs.EventPullRequest:
		ev, err := provider.ParsePullRequest(raw)
		if err != nil {
			return err
		}
		return h.syncGitLabMigrationMergeRequest(ctx, conn, ev.RepoOwner, ev.RepoName, ev.Number)
	case vcs.EventCIStatus:
		ev, err := provider.ParseCIStatus(raw)
		if err != nil {
			return err
		}
		candidates, err := h.gitLabWebhookCandidates(ctx, conn, ev.RepoOwner, ev.RepoName, ev.ProjectID, ev.SHA, ev.Ref, 0)
		if err != nil {
			return err
		}
		for _, pr := range candidates {
			if err := h.syncGitLabMigrationMergeRequest(ctx, conn, pr.RepoOwner, pr.RepoName, pr.PrNumber); err != nil {
				return err
			}
		}
		return nil
	case vcs.EventOther:
		switch eventType {
		case "Job Hook":
			var job gitLabWebhookJobPayload
			if err := json.Unmarshal(raw, &job); err != nil {
				return err
			}
			jobPath := strings.Trim(job.Project.PathWithNamespace, "/")
			jobOwner, jobName, valid := splitGitLabProjectPath(jobPath)
			if !valid {
				return errors.New("migration job project path is invalid")
			}
			sha := firstGitLabWebhookValue(job.ObjectAttributes.SHA, job.Pipeline.SHA, job.SHA)
			ref := firstGitLabWebhookValue(job.ObjectAttributes.Ref, job.Pipeline.Ref, job.Ref)
			candidates, err := h.gitLabWebhookCandidates(ctx, conn, jobOwner, jobName, job.Project.ID, sha, ref, 0)
			if err != nil {
				return err
			}
			for _, pr := range candidates {
				if err := h.syncGitLabMigrationMergeRequest(ctx, conn, pr.RepoOwner, pr.RepoName, pr.PrNumber); err != nil {
					return err
				}
			}
			return nil
		case "Note Hook":
			var note gitLabWebhookNotePayload
			if err := json.Unmarshal(raw, &note); err != nil {
				return err
			}
			noteOwner, noteName, valid := splitGitLabProjectPath(strings.Trim(note.Project.PathWithNamespace, "/"))
			if !valid {
				return errors.New("migration note project path is invalid")
			}
			if note.MergeRequest.IID <= 0 {
				return nil
			}
			return h.syncGitLabMigrationMergeRequest(ctx, conn, noteOwner, noteName, note.MergeRequest.IID)
		case "Push Hook":
			// Normal ingress accepts Push Hooks without syncing state.
			return nil
		default:
			if system {
				return nil
			}
			return errors.New("unsupported GitLab migration event")
		}
	default:
		return errors.New("unsupported GitLab migration event")
	}
}

func (h *Handler) syncGitLabMigrationMergeRequest(ctx context.Context, conn db.VcsConnection, owner, name string, iid int32) error {
	if owner == "" || name == "" || iid <= 0 {
		return errors.New("GitLab migration MR identity is incomplete")
	}
	token, err := h.openVCSSecret(conn.AccessTokenEncrypted)
	if err != nil || token == "" {
		return errors.Join(errGitLabSystemHookCredential, err)
	}
	api, err := h.gitLabDetailAPIForVCSConnection(conn, token)
	if err != nil {
		return err
	}
	path := owner + "/" + name
	project, err := h.Queries.GetVCSGitLabProjectByConnectionPath(ctx, db.GetVCSGitLabProjectByConnectionPathParams{
		WorkspaceID: conn.WorkspaceID, ConnectionID: conn.ID, PathWithNamespace: path,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		remoteProject, projectErr := api.GetProject(ctx, path)
		if projectErr != nil {
			return projectErr
		}
		project = db.VcsGitlabProject{
			WorkspaceID: conn.WorkspaceID, ConnectionID: conn.ID,
			GitlabProjectID: remoteProject.ID, PathWithNamespace: remoteProject.PathWithNamespace,
		}
	} else if err != nil {
		return err
	}
	if project.GitlabProjectID <= 0 || project.PathWithNamespace != path {
		return errors.New("GitLab migration project identity does not match")
	}
	remoteMR, err := api.GetMergeRequest(ctx, project.GitlabProjectID, iid)
	if err != nil {
		return err
	}
	if remoteMR.IID != iid || (remoteMR.TargetProjectID != 0 && remoteMR.TargetProjectID != project.GitlabProjectID) {
		return errors.New("GitLab migration MR does not match authorized project")
	}
	_, summary, err := h.syncVCSGitLabMergeRequest(ctx, api, project, remoteMR)
	if err != nil {
		return err
	}
	if len(summary.Errors) != 0 {
		return fmt.Errorf("GitLab migration MR detail refresh incomplete: %s", strings.Join(summary.Errors, "; "))
	}
	if summary.PipelineError != nil {
		return fmt.Errorf("GitLab migration MR pipeline refresh incomplete: %w", summary.PipelineError)
	}
	return nil
}

var errGitLabSystemHookCredential = errors.New("GitLab system-hook credentials are unavailable")

func gitLabSystemHookFailureStatus(err error) int {
	if errors.Is(err, errGitLabSystemHookCredential) {
		return http.StatusInternalServerError
	}
	return http.StatusServiceUnavailable
}

// A system hook covers the entire GitLab instance. Only repositories explicitly
// configured in this workspace may affect its issues; a similarly named repo
// on another host or under another namespace must not match.
func gitLabSystemHookRepoAllowed(raw []byte, instanceURL, owner, name string) bool {
	var repos []workspaceRepoRef
	if json.Unmarshal(raw, &repos) != nil || owner == "" || name == "" {
		return false
	}
	instance, err := url.Parse(instanceURL)
	if err != nil || instance.Host == "" {
		return false
	}
	want := strings.Trim(instance.Path, "/")
	if want != "" {
		want += "/"
	}
	want += owner + "/" + name
	for _, repo := range repos {
		u, err := parseGitLabRepoURL(strings.TrimSpace(repo.URL))
		if err != nil || !gitLabRepoURLMatchesInstance(u, instance) {
			continue
		}
		path := strings.TrimSuffix(strings.Trim(u.Path, "/"), ".git")
		if path == want {
			return true
		}
	}
	return false
}

func parseGitLabRepoURL(raw string) (*url.URL, error) {
	if strings.Contains(raw, "://") {
		return url.Parse(raw)
	}
	at := strings.Index(raw, "@")
	colon := strings.Index(raw, ":")
	if at <= 0 || colon <= at+1 || colon == len(raw)-1 {
		return nil, errors.New("invalid scp-style repository URL")
	}
	return &url.URL{Scheme: "ssh", Host: raw[at+1 : colon], Path: "/" + raw[colon+1:], User: url.User(raw[:at])}, nil
}
func gitLabRepoURLMatchesInstance(repo, instance *url.URL) bool {
	if repo.RawQuery != "" || repo.Fragment != "" {
		return false
	}
	if repo.Scheme == "ssh" {
		return strings.EqualFold(repo.Hostname(), instance.Hostname()) && repo.User != nil && repo.User.Username() == "git"
	}
	if repo.Scheme == "https" || repo.Scheme == "http" {
		return repo.Scheme == instance.Scheme && strings.EqualFold(repo.Host, instance.Host) && repo.User == nil
	}
	return false
}

func (h *Handler) authorizeGitLabSystemHook(ctx context.Context, conn db.VcsConnection, provider vcs.Provider, owner, name string, pipeline *vcs.CIStatusEvent) (bool, error) {
	return h.authorizeGitLabSystemHookWithSideEffects(ctx, conn, provider, owner, name, pipeline, true)
}

func (h *Handler) authorizeGitLabSystemHookForReceipt(ctx context.Context, conn db.VcsConnection, provider vcs.Provider, owner, name string, pipeline *vcs.CIStatusEvent) (bool, error) {
	return h.authorizeGitLabSystemHookWithSideEffects(ctx, conn, provider, owner, name, pipeline, false)
}

func (h *Handler) authorizeGitLabSystemHookWithSideEffects(ctx context.Context, conn db.VcsConnection, provider vcs.Provider, owner, name string, pipeline *vcs.CIStatusEvent, recordPipelineSyncError bool) (bool, error) {
	workspace, err := h.Queries.GetWorkspace(ctx, conn.WorkspaceID)
	if err != nil {
		return false, fmt.Errorf("load workspace for GitLab system hook: %w", err)
	}
	candidates := make(map[string]db.VcsPullRequest)
	if pipeline != nil && pipeline.ProjectID > 0 && pipeline.SHA != "" {
		byProject, err := h.Queries.ListVCSGitLabPullRequestsByProjectHead(ctx, db.ListVCSGitLabPullRequestsByProjectHeadParams{
			WorkspaceID:  conn.WorkspaceID,
			ConnectionID: conn.ID,
			RepoOwner:    owner,
			RepoName:     name,
			HeadSha:      pipeline.SHA,
		})
		if err != nil {
			return false, fmt.Errorf("find configured GitLab target pipelines: %w", err)
		}
		for _, candidate := range byProject {
			candidates[uuidToString(candidate.ID)] = candidate
		}
		bySource, err := h.Queries.ListVCSGitLabPullRequestsBySourceProjectHead(ctx, db.ListVCSGitLabPullRequestsBySourceProjectHeadParams{
			WorkspaceID:     conn.WorkspaceID,
			ConnectionID:    conn.ID,
			SourceProjectID: pgtype.Int8{Int64: pipeline.ProjectID, Valid: true},
			HeadSha:         pipeline.SHA,
		})
		if err != nil {
			return false, fmt.Errorf("find configured GitLab source pipelines: %w", err)
		}
		for _, candidate := range bySource {
			candidates[uuidToString(candidate.ID)] = candidate
		}
	}
	allowed := gitLabSystemHookRepoAllowed(workspace.Repos, conn.InstanceUrl, owner, name)
	for _, candidate := range candidates {
		if gitLabSystemHookRepoAllowed(workspace.Repos, conn.InstanceUrl, candidate.RepoOwner, candidate.RepoName) {
			allowed = true
			break
		}
	}
	if !allowed {
		return false, nil
	}
	checker, ok := provider.(vcs.RepositoryAccessChecker)
	if !ok {
		return true, nil
	}
	token, err := h.openVCSSecret(conn.AccessTokenEncrypted)
	if err != nil || token == "" {
		if err == nil {
			err = errors.New("empty GitLab access token")
		}
		return false, errors.Join(errGitLabSystemHookCredential, fmt.Errorf("open GitLab access token: %w", err))
	}
	readable, err := checker.RepositoryReadable(ctx, conn.InstanceUrl, owner, name, token)
	if err == nil && !readable {
		return false, nil
	}
	if err != nil {
		if pipeline != nil && recordPipelineSyncError {
			for _, candidate := range candidates {
				if gitLabSystemHookRepoAllowed(workspace.Repos, conn.InstanceUrl, candidate.RepoOwner, candidate.RepoName) {
					_, _ = h.Queries.MarkGitLabVCSPullRequestPipelineSync(ctx, db.MarkGitLabVCSPullRequestPipelineSyncParams{
						ID:                   candidate.ID,
						WorkspaceID:          conn.WorkspaceID,
						PipelineSyncError:    strToText(gitLabWakeupFailureMessage(err)),
						WakeupSyncGeneration: candidate.WakeupSyncGeneration,
						HeadSha:              candidate.HeadSha,
					})
				}
			}
		}
		return false, err
	}
	return true, nil
}

func gitLabWebhookProjectIdentity(raw []byte) (int64, string, error) {
	var payload struct {
		Project struct {
			ID                int64  `json:"id"`
			PathWithNamespace string `json:"path_with_namespace"`
		} `json:"project"`
		ProjectID int64 `json:"project_id"`
	}
	if err := json.Unmarshal(raw, &payload); err != nil {
		return 0, "", err
	}
	projectID := payload.Project.ID
	if projectID == 0 {
		projectID = payload.ProjectID
	}
	if projectID < 0 {
		return 0, "", errors.New("GitLab webhook project ID is invalid")
	}
	path := strings.Trim(payload.Project.PathWithNamespace, "/")
	if path == "" {
		return 0, "", errors.New("GitLab webhook project path is missing")
	}
	if _, _, ok := splitGitLabProjectPath(path); !ok {
		return 0, "", errors.New("GitLab webhook project path is invalid")
	}
	return projectID, path, nil
}

func (h *Handler) authorizeGitLabWebhookProject(ctx context.Context, conn db.VcsConnection, projectID int64, path string) (bool, error) {
	path = strings.Trim(path, "/")
	if path == "" {
		return false, nil
	}
	if _, _, ok := splitGitLabProjectPath(path); !ok {
		return false, nil
	}
	if projectID > 0 {
		project, err := h.Queries.GetVCSGitLabProjectByConnectionProject(ctx, db.GetVCSGitLabProjectByConnectionProjectParams{
			WorkspaceID: conn.WorkspaceID, ConnectionID: conn.ID, GitlabProjectID: projectID,
		})
		if err == nil {
			return project.PathWithNamespace == path, nil
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return false, err
		}
	}
	_, err := h.Queries.GetVCSGitLabProjectByConnectionPath(ctx, db.GetVCSGitLabProjectByConnectionPathParams{
		WorkspaceID: conn.WorkspaceID, ConnectionID: conn.ID, PathWithNamespace: path,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		// An ordinary project/group hook is authorized by its connection's
		// webhook secret when no persisted workspace/connection binding exists.
		// Unlike System Hooks, it does not require workspace.repos or GitLab API.
		return true, nil
	}
	if err != nil {
		return false, err
	}
	// A path binding exists, but its numeric project ID does not match (or was
	// missing from the payload).
	return false, nil
}

func (h *Handler) mirrorVCSPullRequest(ctx context.Context, conn db.VcsConnection, ev vcs.PullRequestEvent) error {
	if ev.RepoOwner == "" || ev.RepoName == "" || ev.Number <= 0 {
		return errors.New("vcs: pull_request missing repository identity")
	}

	prevState := ""
	prev, err := h.Queries.GetVCSPullRequestByKey(ctx, db.GetVCSPullRequestByKeyParams{
		ConnectionID: conn.ID,
		RepoOwner:    ev.RepoOwner,
		RepoName:     ev.RepoName,
		PrNumber:     ev.Number,
	})
	if err == nil {
		prevState = prev.State
		if conn.Provider == string(vcs.KindGitLab) {
			incomingVersion := parseGHTime(ev.UpdatedAt)
			if prev.PrUpdatedAt.Valid && incomingVersion.Valid && !incomingVersion.Time.After(prev.PrUpdatedAt.Time) {
				// Retry a failed refresh for the exact MR version without replaying
				// auto-link or completion side effects.
				if incomingVersion.Time.Equal(prev.PrUpdatedAt.Time) &&
					(prev.StatusSyncError.Valid || prev.PipelineSyncError.Valid) {
					return h.retryGitLabWebhookMergeRequestSync(ctx, conn, prev)
				}
				return nil
			}
		}
	} else if conn.Provider == string(vcs.KindGitLab) && !errors.Is(err, pgx.ErrNoRows) {
		return err
	}
	conflictState := ev.HasConflicts
	conflictHeadSHA := ""
	if conflictState != nil && ev.HeadSHA != "" {
		conflictHeadSHA = ev.HeadSHA
	} else {
		conflictState = nil
	}
	sourceProjectID := pgtype.Int8{}
	if ev.SourceProjectID > 0 {
		sourceProjectID = pgtype.Int8{Int64: ev.SourceProjectID, Valid: true}
	}
	pr, err := h.Queries.UpsertVCSPullRequest(ctx, db.UpsertVCSPullRequestParams{
		WorkspaceID:       conn.WorkspaceID,
		ConnectionID:      conn.ID,
		Provider:          conn.Provider,
		RepoOwner:         ev.RepoOwner,
		RepoName:          ev.RepoName,
		PrNumber:          ev.Number,
		Title:             ev.Title,
		State:             ev.State,
		HtmlUrl:           ev.HTMLURL,
		Branch:            ptrToText(strPtrOrNil(ev.Branch)),
		AuthorLogin:       ptrToText(strPtrOrNil(ev.AuthorLogin)),
		AuthorAvatarUrl:   ptrToText(strPtrOrNil(ev.AuthorAvatarURL)),
		MergedAt:          parseGHTime(ev.MergedAt),
		ClosedAt:          parseGHTime(ev.ClosedAt),
		PrCreatedAt:       parseGHTimeRequired(ev.CreatedAt),
		PrUpdatedAt:       parseGHTimeRequired(ev.UpdatedAt),
		Additions:         ev.Additions,
		Deletions:         ev.Deletions,
		ChangedFiles:      ev.ChangedFiles,
		HeadSha:           ev.HeadSHA,
		HasConflicts:      boolToPg(conflictState),
		ConflictHeadSha:   conflictHeadSHA,
		SourceProjectID:   sourceProjectID,
		SourceProjectPath: strToText(ev.SourceProjectPath),
	})
	if err != nil {
		if conn.Provider == string(vcs.KindGitLab) && errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		return err
	}

	evUpdatedAt := parseGHTimeRequired(ev.UpdatedAt)
	if pr.PrUpdatedAt.Valid && evUpdatedAt.Valid && pr.PrUpdatedAt.Time.After(evUpdatedAt.Time) {
		return nil
	}

	workspaceID := uuidToString(conn.WorkspaceID)
	resp := vcsPullRequestToResponse(pr)
	linkedIssueIDs := make([]string, 0)
	ws, err := h.Queries.GetWorkspace(ctx, conn.WorkspaceID)
	if err != nil {
		if conn.Provider == string(vcs.KindGitLab) {
			return err
		}
		slog.Warn("vcs: load workspace failed", "err", err)
	} else {
		var touched map[pgtype.UUID]struct{}
		idents := prClaimedIdentifiers(ev.Title, ev.Body, ev.Branch)
		closeIntentIDs := make(map[string]struct{})
		for _, identifier := range extractClosingIdentifiers(ev.Title, ev.Body) {
			closeIntentIDs[identifier] = struct{}{}
		}
		legacyGitLabPolicy := false
		if conn.Provider == string(vcs.KindGitLab) {
			project, err := h.Queries.GetVCSGitLabProjectByConnectionPath(ctx, db.GetVCSGitLabProjectByConnectionPathParams{
				WorkspaceID:       conn.WorkspaceID,
				ConnectionID:      conn.ID,
				PathWithNamespace: ev.RepoOwner + "/" + ev.RepoName,
			})
			if err == nil {
				legacyGitLabPolicy = project.LegacyLinkPolicy
			} else if !errors.Is(err, pgx.ErrNoRows) {
				return err
			}
		}
		linkedIssueIDs, touched = h.reconcileAutoLinks(ctx, ws, pr.ID, ev.State, prAutoLinkInput{
			idents:      idents,
			permits:     func(string) bool { return true },
			ambiguous:   func(string) bool { return false },
			closeIntent: func(identifier string) bool { _, ok := closeIntentIDs[identifier]; return ok },
			link: func(issueID pgtype.UUID, closeIntent bool) (int64, error) {
				if conn.Provider == string(vcs.KindGitLab) && legacyGitLabPolicy {
					return h.Queries.LinkGitLabIssueToVCSPullRequest(ctx, db.LinkGitLabIssueToVCSPullRequestParams{
						IssueID: issueID, PullRequestID: pr.ID, GitlabCloseIntent: closeIntent,
						PreserveCloseIntent: ev.State == "merged" || ev.State == "closed",
					})
				}
				return h.Queries.LinkIssueToVCSPullRequest(ctx, db.LinkIssueToVCSPullRequestParams{IssueID: issueID, PullRequestID: pr.ID})
			},
			unlink: func(issueID pgtype.UUID) (bool, error) {
				if conn.Provider == string(vcs.KindGitLab) && legacyGitLabPolicy {
					unlinked, err := h.Queries.UnlinkGitLabIssueFromVCSPullRequest(ctx, db.UnlinkGitLabIssueFromVCSPullRequestParams{IssueID: issueID, PullRequestID: pr.ID})
					return unlinked > 0, err
				}
				unlinked, err := h.Queries.UnlinkIssueFromVCSPullRequest(ctx, db.UnlinkIssueFromVCSPullRequestParams{IssueID: issueID, PullRequestID: pr.ID})
				return unlinked > 0, err
			},
			listAuto: func() ([]pgtype.UUID, error) {
				if conn.Provider == string(vcs.KindGitLab) && legacyGitLabPolicy {
					return h.Queries.ListAutoLinkedGitLabIssueIDsForVCSPullRequest(ctx, pr.ID)
				}
				return h.Queries.ListAutoLinkedIssueIDsForVCSPullRequest(ctx, pr.ID)
			},
		})
		if ev.State == "merged" && prevState != "merged" {
			issueIDs, err := h.Queries.ListIssueIDsForVCSPullRequest(ctx, pr.ID)
			if err != nil {
				if conn.Provider == string(vcs.KindGitLab) {
					return err
				}
				slog.Warn("vcs: list linked issues failed", "err", err)
			}
			for _, id := range issueIDs {
				touched[id] = struct{}{}
			}
		}
		resolver := issuestatus.NewResolver(conn.WorkspaceID)
		for issueID := range touched {
			h.maybeAutoCompleteIssue(ctx, conn.WorkspaceID, issueID, resolver)
		}
	}

	h.publish(protocol.EventPullRequestUpdated, workspaceID, "system", "", map[string]any{
		"pull_request":     resp,
		"linked_issue_ids": linkedIssueIDs,
	})
	var pendingGitLabPipelineSync *vcsGitLabWakeupPipelineSync
	var mergeRequestSyncErr error
	if conn.Provider == string(vcs.KindGitLab) {
		pendingGitLabPipelineSync, err = h.beginGitLabWakeupTargetForMergeRequestWebhook(ctx, vcs.GitLabWakeupTarget{
			Source:         vcs.GitLabWakeupSourceVCSConnection,
			WorkspaceID:    uuidToString(conn.WorkspaceID),
			ConnectionID:   uuidToString(conn.ID),
			MergeRequestID: uuidToString(pr.ID),
			Repository:     ev.RepoOwner + "/" + ev.RepoName,
			Number:         ev.Number,
		})
		if err != nil {
			if isGitLabRefreshPersistenceError(err) {
				return err
			}
			// The webhook's authenticated MR state is already canonical. A
			// transient enrichment failure must not skip auto-linking or its
			// close-intent automation; return it after those effects so GitLab
			// can still retry the detail refresh.
			mergeRequestSyncErr = err
		}
	}
	if pendingGitLabPipelineSync != nil {
		if err := h.syncVCSGitLabWakeupPipelines(ctx, pendingGitLabPipelineSync); err != nil {
			if isGitLabRefreshPersistenceError(err) {
				return err
			}
			slog.Warn("vcs: merge request Pipeline refresh failed", "reason", gitLabWakeupFailureMessage(err), "pr_id", uuidToString(pr.ID))
		}
	}
	return mergeRequestSyncErr
}
func (h *Handler) retryGitLabWebhookMergeRequestSync(ctx context.Context, conn db.VcsConnection, pr db.VcsPullRequest) error {
	pending, err := h.beginGitLabWakeupTargetForMergeRequestWebhook(ctx, vcs.GitLabWakeupTarget{
		Source:         vcs.GitLabWakeupSourceVCSConnection,
		WorkspaceID:    uuidToString(conn.WorkspaceID),
		ConnectionID:   uuidToString(conn.ID),
		MergeRequestID: uuidToString(pr.ID),
		Repository:     pr.RepoOwner + "/" + pr.RepoName,
		Number:         pr.PrNumber,
	})
	if err != nil {
		return err
	}
	if pending == nil {
		return nil
	}
	if err := h.syncVCSGitLabWakeupPipelines(ctx, pending); err != nil {
		if isGitLabRefreshPersistenceError(err) {
			return err
		}
		slog.Warn("vcs: merge request Pipeline refresh failed", "reason", gitLabWakeupFailureMessage(err), "pr_id", uuidToString(pr.ID))
	}
	return nil
}

func (h *Handler) mirrorVCSCIStatus(ctx context.Context, conn db.VcsConnection, ev vcs.CIStatusEvent) error {
	if ev.SHA == "" || ev.State == "" {
		return nil
	}
	if conn.Provider == string(vcs.KindGitLab) {
		return h.syncGitLabPipelineEvent(ctx, conn, ev)
	}
	if err := h.Queries.UpsertVCSCommitStatus(ctx, db.UpsertVCSCommitStatusParams{
		ConnectionID: conn.ID,
		Sha:          ev.SHA,
		Context:      ev.Context,
		State:        ev.State,
		TargetUrl:    ptrToText(strPtrOrNil(ev.TargetURL)),
		Description:  ptrToText(strPtrOrNil(ev.Description)),
		UpdatedAt:    parseGHTimeRequired(ev.UpdatedAt),
	}); err != nil {
		return err
	}

	issueIDs, err := h.Queries.ListIssueIDsForVCSPRHead(ctx, db.ListIssueIDsForVCSPRHeadParams{
		ConnectionID: conn.ID,
		HeadSha:      ev.SHA,
	})
	if err != nil {
		return err
	}
	workspaceID := uuidToString(conn.WorkspaceID)
	for _, issueID := range issueIDs {
		h.publish(protocol.EventPullRequestUpdated, workspaceID, "system", "", map[string]any{
			"issue_id": uuidToString(issueID),
		})
	}
	return nil
}

func (h *Handler) syncGitLabPipelineEvent(ctx context.Context, conn db.VcsConnection, ev vcs.CIStatusEvent) error {
	candidates := make(map[string]db.VcsPullRequest)
	byProject, err := h.Queries.ListVCSGitLabPullRequestsByProjectHead(ctx, db.ListVCSGitLabPullRequestsByProjectHeadParams{
		WorkspaceID:  conn.WorkspaceID,
		ConnectionID: conn.ID,
		RepoOwner:    ev.RepoOwner,
		RepoName:     ev.RepoName,
		HeadSha:      ev.SHA,
	})
	if err != nil {
		return err
	}
	for _, pr := range byProject {
		if vcsGitLabPipelineRefMayMatchMR(ev.Ref, pr) {
			candidates[uuidToString(pr.ID)] = pr
		}
	}
	bySource, err := h.Queries.ListVCSGitLabPullRequestsBySourceProjectHead(ctx, db.ListVCSGitLabPullRequestsBySourceProjectHeadParams{
		WorkspaceID:     conn.WorkspaceID,
		ConnectionID:    conn.ID,
		SourceProjectID: pgtype.Int8{Int64: ev.ProjectID, Valid: ev.ProjectID > 0},
		HeadSha:         ev.SHA,
	})
	if err != nil {
		return err
	}
	for _, pr := range bySource {
		if vcsGitLabPipelineRefMayMatchMR(ev.Ref, pr) {
			candidates[uuidToString(pr.ID)] = pr
		}
	}
	eventUpdatedAt := parseGHTime(ev.UpdatedAt)
	if !eventUpdatedAt.Valid {
		return errors.New("GitLab pipeline event is missing updated_at")
	}
	projectPath := ev.RepoOwner + "/" + ev.RepoName
	for _, pr := range candidates {
		existing, err := h.Queries.GetVCSGitLabPipelineByMRIdentity(ctx, db.GetVCSGitLabPipelineByMRIdentityParams{
			WorkspaceID: conn.WorkspaceID, PullRequestID: pr.ID,
			PipelineProjectPath: projectPath, PipelineID: ev.PipelineID,
		})
		if err == nil && existing.PipelineUpdatedAt.Valid && !eventUpdatedAt.Time.After(existing.PipelineUpdatedAt.Time) {
			continue
		}
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		if err := h.syncGitLabWakeupTargetForWebhook(ctx, vcs.GitLabWakeupTarget{
			Source:         vcs.GitLabWakeupSourceVCSConnection,
			WorkspaceID:    uuidToString(conn.WorkspaceID),
			ConnectionID:   uuidToString(conn.ID),
			MergeRequestID: uuidToString(pr.ID),
			Repository:     pr.RepoOwner + "/" + pr.RepoName,
			Number:         pr.PrNumber,
		}); err != nil {
			return err
		}
		h.publishGitLabVCSDetailUpdated(ctx, conn.WorkspaceID, pr.ID)
	}
	return nil
}

func vcsGitLabPipelineRefMayMatchMR(ref string, pr db.VcsPullRequest) bool {
	if ref == "" {
		return true
	}
	if pr.Branch.Valid && strings.TrimPrefix(ref, "refs/heads/") == pr.Branch.String {
		return true
	}
	parts := strings.Split(ref, "/")
	if len(parts) == 4 && parts[0] == "refs" && parts[1] == "merge-requests" && (parts[3] == "head" || parts[3] == "merge") {
		number, err := strconv.ParseInt(parts[2], 10, 32)
		return err == nil && int32(number) == pr.PrNumber
	}
	return false
}
