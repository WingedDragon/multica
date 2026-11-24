package handler

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/integrations/gitlab"
	"github.com/multica-ai/multica/server/internal/integrations/vcs"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

func (h *Handler) gitLabDetailAPIForVCSConnection(connection db.VcsConnection, token string) (gitLabDetailAPI, error) {
	cfg := gitlab.LoadConfigFromEnv()
	cfg.BaseURL = connection.InstanceUrl
	cfg.Token = token
	api, err := newGitLabClient(cfg)
	if err != nil {
		return nil, err
	}
	return detailGitLabAPI(api)
}

type vcsGitLabWakeupPipelineSync struct {
	reader         vcs.GitLabWakeupSnapshotReader
	instanceURL    string
	token          string
	target         vcs.GitLabWakeupTarget
	pr             db.VcsPullRequest
	syncGeneration int64
	mrSnapshot     vcs.GitLabWakeupMergeRequestSnapshot
}

// SyncGitLabWakeupTarget refreshes MR state and records Pipeline status separately.
// Pipeline-only failures remain visible to CI waiters without blocking non-CI rules.
func (h *Handler) SyncGitLabWakeupTarget(ctx context.Context, target vcs.GitLabWakeupTarget) error {
	return h.syncGitLabWakeupTarget(ctx, target, false)
}

func (h *Handler) syncGitLabWakeupTargetForWebhook(ctx context.Context, target vcs.GitLabWakeupTarget) error {
	return h.syncGitLabWakeupTarget(ctx, target, true)
}

// MR webhooks persist authoritative state before supplementary pipeline reads,
// allowing close-intent work to run inside the caller's live context.
func (h *Handler) beginGitLabWakeupTargetForMergeRequestWebhook(ctx context.Context, target vcs.GitLabWakeupTarget) (*vcsGitLabWakeupPipelineSync, error) {
	if target.Source != vcs.GitLabWakeupSourceVCSConnection || target.Number <= 0 || !validGitLabRepositoryPath(target.Repository) {
		return nil, errors.New("invalid GitLab merge request webhook target")
	}
	workspaceID, err := parseGitLabWakeupUUID(target.WorkspaceID)
	if err != nil {
		return nil, errors.New("invalid GitLab wakeup workspace")
	}
	connectionID, err := parseGitLabWakeupUUID(target.ConnectionID)
	if err != nil {
		return nil, errors.New("invalid GitLab wakeup connection")
	}
	if target.MergeRequestID != "" {
		if _, err := parseGitLabWakeupUUID(target.MergeRequestID); err != nil {
			return nil, errors.New("invalid GitLab wakeup merge request")
		}
	}
	return h.beginVCSGitLabWakeupTarget(ctx, target, workspaceID, connectionID)
}

func (h *Handler) syncGitLabWakeupTarget(ctx context.Context, target vcs.GitLabWakeupTarget, pipelineFailureIsFatal bool) error {
	if target.Number <= 0 || !validGitLabRepositoryPath(target.Repository) {
		return errors.New("invalid GitLab wakeup target")
	}
	workspaceID, err := parseGitLabWakeupUUID(target.WorkspaceID)
	if err != nil {
		return errors.New("invalid GitLab wakeup workspace")
	}
	connectionID, err := parseGitLabWakeupUUID(target.ConnectionID)
	if err != nil {
		return errors.New("invalid GitLab wakeup connection")
	}
	if target.MergeRequestID != "" {
		if _, err := parseGitLabWakeupUUID(target.MergeRequestID); err != nil {
			return errors.New("invalid GitLab wakeup merge request")
		}
	}
	if target.Source != vcs.GitLabWakeupSourceVCSConnection &&
		target.Source != vcs.GitLabWakeupSourceWorkspaceConnection {
		return errors.New("invalid GitLab wakeup source")
	}

	target, connectionID, err = h.canonicalizeGitLabWakeupTarget(ctx, target, workspaceID, connectionID)
	if err != nil {
		return errors.New(gitLabWakeupFailureMessage(err))
	}
	pipelineErr, mrErr := h.syncVCSGitLabWakeupTarget(ctx, target, workspaceID, connectionID)
	if mrErr != nil {
		return errors.New(gitLabWakeupFailureMessage(mrErr))
	}
	if pipelineFailureIsFatal && pipelineErr != nil {
		return errors.New(gitLabWakeupFailureMessage(pipelineErr))
	}
	return nil
}

func (h *Handler) syncVCSGitLabWakeupTarget(ctx context.Context, target vcs.GitLabWakeupTarget, workspaceID, connectionID pgtype.UUID) (error, error) {
	pending, err := h.beginVCSGitLabWakeupTarget(ctx, target, workspaceID, connectionID)
	if err != nil {
		return nil, err
	}
	return h.syncVCSGitLabWakeupPipelines(ctx, pending), nil
}

func (h *Handler) beginVCSGitLabWakeupTarget(ctx context.Context, target vcs.GitLabWakeupTarget, workspaceID, connectionID pgtype.UUID) (*vcsGitLabWakeupPipelineSync, error) {
	connection, err := h.Queries.GetVCSConnectionByID(ctx, connectionID)
	if err != nil {
		return nil, err
	}
	if connection.WorkspaceID != workspaceID || connection.Provider != string(vcs.KindGitLab) {
		return nil, errors.New("GitLab connection does not belong to this workspace")
	}
	owner, name, ok := splitGitLabRepositoryPath(target.Repository)
	if !ok {
		return nil, errors.New("invalid GitLab repository path")
	}
	pr, err := h.Queries.GetVCSGitLabPullRequestByWakeupIdentity(ctx, db.GetVCSGitLabPullRequestByWakeupIdentityParams{
		WorkspaceID:  workspaceID,
		ConnectionID: connectionID,
		RepoOwner:    owner,
		RepoName:     name,
		PrNumber:     target.Number,
	})
	if err != nil {
		return nil, err
	}
	if pr.Provider != string(vcs.KindGitLab) || pr.WorkspaceID != workspaceID ||
		pr.ConnectionID != connectionID || pr.PrNumber != target.Number ||
		!strings.EqualFold(pr.RepoOwner+"/"+pr.RepoName, target.Repository) {
		return nil, errors.New("GitLab merge request does not match the canonical workspace target")
	}
	if target.MergeRequestID != "" && uuidToString(pr.ID) != target.MergeRequestID {
		return nil, errors.New("GitLab merge request identity changed")
	}
	syncGeneration, err := h.Queries.BeginGitLabVCSPullRequestWakeupSync(ctx, db.BeginGitLabVCSPullRequestWakeupSyncParams{
		ID:          pr.ID,
		WorkspaceID: workspaceID,
	})
	if err != nil {
		return nil, err
	}
	markMRFailure := func(syncErr error) error {
		rows, markErr := h.Queries.MarkGitLabVCSPullRequestWakeupSyncError(ctx, db.MarkGitLabVCSPullRequestWakeupSyncErrorParams{
			ID:                   pr.ID,
			WorkspaceID:          workspaceID,
			StatusSyncError:      strToText(gitLabWakeupFailureMessage(syncErr)),
			WakeupSyncGeneration: syncGeneration,
		})
		if markErr != nil {
			return errors.Join(syncErr, markErr)
		}
		if rows == 0 {
			return nil
		}
		return syncErr
	}

	token, err := h.openVCSSecret(connection.AccessTokenEncrypted)
	if err != nil || token == "" {
		if err == nil {
			err = errors.New("empty GitLab access token")
		}
		return nil, markMRFailure(err)
	}
	provider, ok := vcs.For(string(vcs.KindGitLab))
	if !ok {
		return nil, markMRFailure(errors.New("GitLab status reader is unavailable"))
	}
	reader, ok := provider.(vcs.GitLabWakeupSnapshotReader)
	if !ok {
		return nil, markMRFailure(errors.New("GitLab status reader is unavailable"))
	}
	mrSnapshot, err := reader.ReadGitLabWakeupMergeRequest(ctx, connection.InstanceUrl, target.Repository, token, target.Number)
	if err != nil {
		return nil, markMRFailure(err)
	}
	snapshot := vcs.GitLabWakeupSnapshot{MergeRequest: mrSnapshot.MergeRequest}
	if err := h.persistVCSGitLabWakeupMR(ctx, pr, snapshot, syncGeneration); err != nil {
		return nil, markMRFailure(err)
	}
	detailAPI, err := h.gitLabDetailAPIForVCSConnection(connection, token)
	if err != nil {
		return nil, markMRFailure(err)
	}
	project, err := h.Queries.GetVCSGitLabProjectByConnectionPath(ctx, db.GetVCSGitLabProjectByConnectionPathParams{
		WorkspaceID: workspaceID, ConnectionID: connectionID, PathWithNamespace: target.Repository,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		remoteProject, projectErr := detailAPI.GetProject(ctx, target.Repository)
		if projectErr != nil {
			return nil, markMRFailure(projectErr)
		}
		project = db.VcsGitlabProject{
			WorkspaceID: workspaceID, ConnectionID: connectionID,
			GitlabProjectID: remoteProject.ID, PathWithNamespace: remoteProject.PathWithNamespace,
		}
	} else if err != nil {
		return nil, markMRFailure(err)
	}
	if project.WorkspaceID != workspaceID || project.ConnectionID != connectionID ||
		project.GitlabProjectID <= 0 || project.PathWithNamespace != target.Repository {
		return nil, markMRFailure(errors.New("GitLab project identity did not match the authorized target"))
	}
	remoteMR, err := detailAPI.GetMergeRequest(ctx, project.GitlabProjectID, pr.PrNumber)
	if err != nil {
		return nil, markMRFailure(err)
	}
	current := pr
	current.WakeupSyncGeneration = syncGeneration
	pr, _, err = h.syncVCSGitLabMergeRequestAtGeneration(ctx, detailAPI, project, remoteMR, &current, syncGeneration)
	if err != nil {
		// The authoritative MR snapshot was persisted above. Errors from the
		// later detail/Pipeline refresh must not mark that successful status sync
		// as failed.
		return nil, err
	}
	return &vcsGitLabWakeupPipelineSync{
		reader: reader, instanceURL: connection.InstanceUrl, token: token, target: target,
		pr: pr, syncGeneration: syncGeneration, mrSnapshot: mrSnapshot,
	}, nil
}

func (h *Handler) syncVCSGitLabWakeupPipelines(ctx context.Context, pending *vcsGitLabWakeupPipelineSync) error {
	pipelineSnapshot := pending.reader.ReadGitLabWakeupPipelines(ctx, pending.instanceURL, pending.token, pending.target.Number, pending.mrSnapshot)
	snapshot := vcs.GitLabWakeupSnapshot{
		MergeRequest:  pending.mrSnapshot.MergeRequest,
		Pipelines:     pipelineSnapshot.Pipelines,
		PipelineError: pipelineSnapshot.PipelineError,
	}
	markPipelineFailure := func(syncErr error) error {
		rows, markErr := h.Queries.MarkGitLabVCSPullRequestPipelineSync(ctx, db.MarkGitLabVCSPullRequestPipelineSyncParams{
			ID:                   pending.pr.ID,
			WorkspaceID:          pending.pr.WorkspaceID,
			PipelineSyncError:    strToText(gitLabWakeupFailureMessage(syncErr)),
			WakeupSyncGeneration: pending.syncGeneration,
			HeadSha:              snapshot.MergeRequest.HeadSHA,
		})
		if markErr != nil {
			return errors.Join(syncErr, wrapGitLabRefreshPersistenceError(markErr))
		}
		if rows == 0 {
			return nil
		}
		return syncErr
	}
	if snapshot.PipelineError != nil {
		return markPipelineFailure(snapshot.PipelineError)
	}
	if err := validateVCSGitLabWakeupPipelines(pending.pr, pending.target.Repository, snapshot); err != nil {
		return markPipelineFailure(err)
	}
	persisted, err := h.persistVCSGitLabWakeupPipelines(ctx, pending.pr, snapshot, pending.syncGeneration)
	if err != nil {
		return markPipelineFailure(err)
	}
	if !persisted {
		return nil
	}
	return nil
}

func (h *Handler) persistVCSGitLabWakeupMR(ctx context.Context, pr db.VcsPullRequest, snapshot vcs.GitLabWakeupSnapshot, syncGeneration int64) error {
	mr := snapshot.MergeRequest
	if mr.HeadSHA == "" || mr.SourceBranch == "" || mr.UpdatedAt == "" {
		return errors.New("GitLab returned incomplete merge request state")
	}
	updatedAt, err := parseGitLabWakeupTime(mr.UpdatedAt)
	if err != nil {
		return err
	}
	conflictState := pgtype.Bool{}
	conflictHeadSHA := ""
	if mr.HasConflicts != nil {
		conflictState = pgtype.Bool{Bool: *mr.HasConflicts, Valid: true}
		conflictHeadSHA = mr.HeadSHA
	}
	sourceProjectID := pgtype.Int8{}
	if mr.SourceProjectID > 0 {
		sourceProjectID = pgtype.Int8{Int64: mr.SourceProjectID, Valid: true}
	}
	rows, err := h.Queries.UpdateGitLabVCSPullRequestWakeupState(ctx, db.UpdateGitLabVCSPullRequestWakeupStateParams{
		ID:                   pr.ID,
		WorkspaceID:          pr.WorkspaceID,
		State:                mr.State,
		HeadSha:              mr.HeadSHA,
		ConflictHeadSha:      conflictHeadSHA,
		PrUpdatedAt:          updatedAt,
		WakeupSyncGeneration: syncGeneration,
		HasConflicts:         conflictState,
		SourceProjectID:      sourceProjectID,
		SourceProjectPath:    strToText(mr.SourceProjectPath),
		MergedAt:             parseGHTime(mr.MergedAt),
		ClosedAt:             parseGHTime(mr.ClosedAt),
		ExpectedHeadSha:      pr.HeadSha,
		ExpectedPrUpdatedAt:  pr.PrUpdatedAt,
	})
	if err != nil {
		return err
	}
	if rows != 1 {
		return errors.New("GitLab merge request changed during synchronization")
	}
	return nil
}

func validateVCSGitLabWakeupPipelines(pr db.VcsPullRequest, repository string, snapshot vcs.GitLabWakeupSnapshot) error {
	mr := snapshot.MergeRequest
	if mr.SourceProjectID <= 0 || mr.SourceProjectPath == "" {
		return errors.New("GitLab source project identity is unavailable for Pipeline refresh")
	}
	for _, pipeline := range snapshot.Pipelines {
		if pipeline.ID <= 0 || pipeline.ProjectID <= 0 || pipeline.ProjectPath == "" || pipeline.SHA != mr.HeadSHA || pipeline.Ref == "" {
			return errors.New("GitLab returned an invalid current-head pipeline")
		}
		if pipeline.Status != "pending" && pipeline.Status != "passed" && pipeline.Status != "failed" {
			return errors.New("GitLab returned an unknown pipeline state")
		}
		if _, err := parseGitLabWakeupTime(pipeline.UpdatedAt); err != nil {
			return err
		}
		if gitLabWakeupMRRef(pipeline.Ref, pr.PrNumber) {
			if pipeline.ProjectPath != repository {
				return errors.New("GitLab returned an MR pipeline from another project")
			}
		} else if pipeline.ProjectPath != mr.SourceProjectPath ||
			pipeline.ProjectID != mr.SourceProjectID || pipeline.Ref != mr.SourceBranch {
			return errors.New("GitLab returned a Pipeline outside the current source branch")
		}
	}
	return nil
}

func (h *Handler) persistVCSGitLabWakeupPipelines(ctx context.Context, pr db.VcsPullRequest, snapshot vcs.GitLabWakeupSnapshot, syncGeneration int64) (bool, error) {
	if h.TxStarter == nil {
		return false, wrapGitLabRefreshPersistenceError(errors.New("database transactions are unavailable"))
	}
	updatedAt, err := parseGitLabWakeupTime(snapshot.MergeRequest.UpdatedAt)
	if err != nil {
		return false, err
	}
	tx, err := h.TxStarter.Begin(ctx)
	if err != nil {
		return false, wrapGitLabRefreshPersistenceError(err)
	}
	defer tx.Rollback(ctx)
	q := h.Queries.WithTx(tx)
	if _, err := q.LockGitLabVCSPullRequestWakeupSync(ctx, db.LockGitLabVCSPullRequestWakeupSyncParams{
		ID:                   pr.ID,
		WorkspaceID:          pr.WorkspaceID,
		WakeupSyncGeneration: syncGeneration,
		HeadSha:              snapshot.MergeRequest.HeadSHA,
		PrUpdatedAt:          updatedAt,
	}); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return false, nil
		}
		return false, wrapGitLabRefreshPersistenceError(err)
	}
	if _, err := q.DeleteVCSGitLabPipelinesByMR(ctx, db.DeleteVCSGitLabPipelinesByMRParams{
		WorkspaceID:   pr.WorkspaceID,
		PullRequestID: pr.ID,
	}); err != nil {
		return false, wrapGitLabRefreshPersistenceError(err)
	}
	for _, pipeline := range snapshot.Pipelines {
		pipelineUpdatedAt, err := parseGitLabWakeupTime(pipeline.UpdatedAt)
		if err != nil {
			return false, err
		}
		if err := q.UpsertVCSGitLabPipeline(ctx, db.UpsertVCSGitLabPipelineParams{
			ID:                  pr.ID,
			WorkspaceID:         pr.WorkspaceID,
			PipelineID:          pipeline.ID,
			PipelineProjectID:   pipeline.ProjectID,
			PipelineProjectPath: pipeline.ProjectPath,
			Sha:                 pipeline.SHA,
			Ref:                 pipeline.Ref,
			Status:              pipeline.Status,
			PipelineUpdatedAt:   pipelineUpdatedAt,
			WebUrl:              strToText(pipeline.WebURL),
		}); err != nil {
			return false, wrapGitLabRefreshPersistenceError(err)
		}
	}
	rows, err := q.MarkGitLabVCSPullRequestPipelineSyncSuccess(ctx, db.MarkGitLabVCSPullRequestPipelineSyncSuccessParams{
		ID:                   pr.ID,
		WorkspaceID:          pr.WorkspaceID,
		WakeupSyncGeneration: syncGeneration,
		HeadSha:              snapshot.MergeRequest.HeadSHA,
	})
	if err != nil {
		return false, wrapGitLabRefreshPersistenceError(err)
	}
	if rows != 1 {
		return false, errors.New("GitLab Pipeline sync marker changed during synchronization")
	}
	if err := tx.Commit(ctx); err != nil {
		return false, wrapGitLabRefreshPersistenceError(err)
	}
	return true, nil
}

func (h *Handler) canonicalizeGitLabWakeupTarget(ctx context.Context, target vcs.GitLabWakeupTarget, workspaceID, requestedConnectionID pgtype.UUID) (vcs.GitLabWakeupTarget, pgtype.UUID, error) {
	legacySource := target.Source == vcs.GitLabWakeupSourceWorkspaceConnection
	if !legacySource && target.Source != vcs.GitLabWakeupSourceVCSConnection {
		return target, pgtype.UUID{}, errors.New("invalid GitLab wakeup source")
	}
	connectionID, err := h.canonicalGitLabConnectionID(ctx, workspaceID, requestedConnectionID, legacySource)
	if err != nil {
		return target, pgtype.UUID{}, err
	}
	target.Source = vcs.GitLabWakeupSourceVCSConnection
	target.ConnectionID = uuidToString(connectionID)
	if target.MergeRequestID != "" {
		pullRequestID, err := h.canonicalGitLabPullRequestID(ctx, workspaceID, connectionID, target.MergeRequestID, legacySource)
		if err != nil {
			return target, pgtype.UUID{}, err
		}
		target.MergeRequestID = uuidToString(pullRequestID)
	}
	return target, connectionID, nil
}

func (h *Handler) canonicalGitLabConnectionID(ctx context.Context, workspaceID, candidateID pgtype.UUID, legacy bool) (pgtype.UUID, error) {
	connectionID := candidateID
	if legacy {
		unifiedID, err := h.Queries.ResolveGitLabMigrationID(ctx, db.ResolveGitLabMigrationIDParams{
			WorkspaceID: workspaceID,
			EntityKind:  "connection",
			LegacyID:    candidateID,
		})
		if errors.Is(err, pgx.ErrNoRows) {
			return pgtype.UUID{}, errors.New("legacy GitLab connection alias is unavailable in this workspace")
		}
		if err != nil {
			return pgtype.UUID{}, err
		}
		connectionID = unifiedID
	}
	connection, err := h.Queries.GetVCSConnectionByID(ctx, connectionID)
	if err != nil {
		return pgtype.UUID{}, err
	}
	if connection.WorkspaceID != workspaceID || connection.Provider != string(vcs.KindGitLab) {
		return pgtype.UUID{}, errors.New("GitLab connection does not belong to this workspace")
	}
	return connection.ID, nil
}

func (h *Handler) canonicalGitLabPullRequestID(ctx context.Context, workspaceID, connectionID pgtype.UUID, candidateID string, legacy bool) (pgtype.UUID, error) {
	pullRequestID, err := parseGitLabWakeupUUID(candidateID)
	if err != nil {
		return pgtype.UUID{}, errors.New("invalid GitLab wakeup merge request")
	}
	if legacy {
		unifiedID, err := h.Queries.ResolveGitLabMigrationID(ctx, db.ResolveGitLabMigrationIDParams{
			WorkspaceID: workspaceID,
			EntityKind:  "pull_request",
			LegacyID:    pullRequestID,
		})
		if errors.Is(err, pgx.ErrNoRows) {
			return pgtype.UUID{}, errors.New("legacy GitLab merge request alias is unavailable in this workspace")
		}
		if err != nil {
			return pgtype.UUID{}, err
		}
		pullRequestID = unifiedID
	}
	pullRequest, err := h.Queries.GetVCSPullRequestInWorkspace(ctx, db.GetVCSPullRequestInWorkspaceParams{
		ID: pullRequestID, WorkspaceID: workspaceID,
	})
	if err != nil {
		return pgtype.UUID{}, err
	}
	if pullRequest.Provider != string(vcs.KindGitLab) || pullRequest.ConnectionID != connectionID {
		return pgtype.UUID{}, errors.New("GitLab merge request does not belong to this workspace connection")
	}
	return pullRequest.ID, nil
}

func gitLabWakeupFailureMessage(err error) string {
	if err == nil {
		return "GitLab state refresh failed; verify the connection and project access, then retry."
	}
	if errors.Is(err, context.DeadlineExceeded) || strings.Contains(strings.ToLower(err.Error()), "timeout") {
		return "GitLab request timed out; retry the status refresh."
	}
	if errors.Is(err, pgx.ErrNoRows) || strings.Contains(strings.ToLower(err.Error()), "not found") || strings.Contains(err.Error(), "404") {
		return "GitLab project or merge request was not found or is not readable."
	}
	message := strings.ToLower(err.Error())
	switch {
	case strings.Contains(message, "401"), strings.Contains(message, "unauthorized"):
		return "GitLab rejected the connection credentials."
	case strings.Contains(message, "403"), strings.Contains(message, "forbidden"), strings.Contains(message, "permission"):
		return "GitLab connection cannot read the target or source project."
	case strings.Contains(message, "429"), strings.Contains(message, "rate limit"):
		return "GitLab rate-limited the status refresh; retry later."
	case strings.Contains(message, "pagination"), strings.Contains(message, "page limit"):
		return "GitLab returned too many pipeline pages; retry the status refresh."
	case strings.Contains(message, "connection refused"), strings.Contains(message, "no such host"), strings.Contains(message, "unreachable"):
		return "GitLab is unreachable; retry the status refresh."
	default:
		return "GitLab state refresh failed; verify the connection and project access, then retry."
	}
}

func parseGitLabWakeupUUID(value string) (pgtype.UUID, error) {
	var id pgtype.UUID
	if err := id.Scan(value); err != nil || !id.Valid {
		return pgtype.UUID{}, errors.New("invalid UUID")
	}
	return id, nil
}

func validGitLabRepositoryPath(path string) bool {
	if path == "" || strings.TrimSpace(path) != path || strings.ContainsAny(path, "?#\\") {
		return false
	}
	parts := strings.Split(path, "/")
	if len(parts) < 2 {
		return false
	}
	for _, part := range parts {
		if part == "" || part == "." || part == ".." {
			return false
		}
	}
	return true
}

func splitGitLabRepositoryPath(path string) (owner, name string, ok bool) {
	if !validGitLabRepositoryPath(path) {
		return "", "", false
	}
	separator := strings.LastIndexByte(path, '/')
	return path[:separator], path[separator+1:], true
}

func parseGitLabWakeupTime(value string) (pgtype.Timestamptz, error) {
	if value == "" {
		return pgtype.Timestamptz{}, errors.New("GitLab returned a pipeline without an update timestamp")
	}
	parsed, err := time.Parse(time.RFC3339Nano, value)
	if err != nil {
		return pgtype.Timestamptz{}, fmt.Errorf("parse GitLab update timestamp: %w", err)
	}
	return pgtype.Timestamptz{Time: parsed, Valid: true}, nil
}

func gitLabWakeupMRRef(ref string, number int32) bool {
	return ref == fmt.Sprintf("refs/merge-requests/%d/head", number) || ref == fmt.Sprintf("refs/merge-requests/%d/merge", number)
}
