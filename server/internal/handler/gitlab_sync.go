package handler

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/integrations/gitlab"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/multica-ai/multica/server/pkg/redact"
)

const gitLabFailedJobTraceLimit = 8192

type gitLabDetailAPI interface {
	GetProject(context.Context, string) (gitlab.Project, error)
	GetMergeRequest(context.Context, int64, int32) (gitlab.MergeRequest, error)
	MergeMergeRequest(context.Context, int64, int32, gitlab.MergeRequestMergeOptions) (gitlab.MergeRequest, error)
	GetMergeRequestChanges(context.Context, int64, int32) (gitlab.MergeRequestChanges, error)
	GetMergeRequestApprovalState(context.Context, int64, int32) (gitlab.ApprovalState, error)
	ListMergeRequestDiscussions(context.Context, int64, int32) ([]gitlab.Discussion, error)
	ListMergeRequestPipelines(context.Context, int64, int32) ([]gitlab.Pipeline, error)
	ListProjectPipelines(context.Context, int64, gitlab.PipelineFilters) ([]gitlab.Pipeline, error)
	GetPipeline(context.Context, int64, int64) (gitlab.Pipeline, error)
	ListPipelineJobs(context.Context, int64, int64) ([]gitlab.Job, error)
	GetJobTrace(context.Context, int64, int64, int) (gitlab.JobTrace, error)
	DownloadJobArtifacts(context.Context, int64, int64) (*http.Response, error)
}

type gitLabRefreshPersistenceError struct {
	cause error
}

func (e *gitLabRefreshPersistenceError) Error() string {
	return e.cause.Error()
}

func (e *gitLabRefreshPersistenceError) Unwrap() error {
	return e.cause
}

func wrapGitLabRefreshPersistenceError(err error) error {
	if err == nil {
		return nil
	}
	var persistenceErr *gitLabRefreshPersistenceError
	if errors.As(err, &persistenceErr) {
		return err
	}
	return &gitLabRefreshPersistenceError{cause: err}
}

func isGitLabRefreshPersistenceError(err error) bool {
	var persistenceErr *gitLabRefreshPersistenceError
	return errors.As(err, &persistenceErr)
}

type gitLabRefreshSummary struct {
	UpdatedMRs           int      `json:"updated_mrs"`
	UpdatedJobs          int      `json:"updated_jobs"`
	UpdatedDiscussions   int      `json:"updated_discussions"`
	Errors               []string `json:"errors,omitempty"`
	PersistenceError     error    `json:"-"`
	PipelineError        error    `json:"-"`
	WakeupSyncGeneration int64    `json:"-"`
}

type gitLabVersionedPipeline struct {
	pipeline  gitlab.Pipeline
	updatedAt time.Time
}

func (s *gitLabRefreshSummary) addError(err error) {
	if err == nil {
		return
	}
	if isGitLabRefreshPersistenceError(err) {
		s.PersistenceError = errors.Join(s.PersistenceError, err)
	}
	s.Errors = append(s.Errors, redact.Text(err.Error()))
}

func (h *Handler) refreshGitLabMergeRequest(ctx context.Context, api gitLabDetailAPI, mr db.GitlabMergeRequest) (db.GitlabMergeRequest, gitLabRefreshSummary, error) {
	var summary gitLabRefreshSummary
	syncGeneration, err := h.Queries.BeginGitLabMergeRequestWakeupSync(ctx, db.BeginGitLabMergeRequestWakeupSyncParams{
		ID:          mr.ID,
		WorkspaceID: mr.WorkspaceID,
	})
	if err != nil {
		return mr, summary, wrapGitLabRefreshPersistenceError(err)
	}
	summary.WakeupSyncGeneration = syncGeneration
	remoteMR, err := api.GetMergeRequest(ctx, mr.GitlabProjectID, mr.MrIid)
	if err != nil {
		return mr, summary, h.markGitLabMergeRequestRefreshErrorForGeneration(ctx, mr, syncGeneration, err)
	}
	changes, err := api.GetMergeRequestChanges(ctx, mr.GitlabProjectID, mr.MrIid)
	if err != nil {
		summary.addError(err)
	}
	additions, deletions, changedFiles := mr.Additions, mr.Deletions, mr.ChangedFiles
	if err == nil {
		additions = changes.Additions()
		deletions = changes.Deletions()
		changedFiles = changes.ChangedFiles()
	}

	sourceProjectID := remoteMR.SourceProjectID
	if sourceProjectID == 0 {
		sourceProjectID = remoteMR.SourceProject.ID
	}
	if remoteMR.TargetProjectID != 0 && remoteMR.TargetProjectID != mr.GitlabProjectID {
		err := fmt.Errorf("gitlab: MR !%d belongs to another target project", mr.MrIid)
		return mr, summary, h.markGitLabMergeRequestRefreshErrorForGeneration(ctx, mr, syncGeneration, err)
	}
	if sourceProjectID <= 0 {
		summary.PipelineError = fmt.Errorf("gitlab: MR !%d source project identity is unavailable", mr.MrIid)
	} else if remoteMR.SourceProject.ID != 0 && remoteMR.SourceProject.ID != sourceProjectID {
		summary.PipelineError = fmt.Errorf("gitlab: MR !%d source project identity is inconsistent", mr.MrIid)
	}
	sourceProjectPath := strings.TrimSpace(remoteMR.SourceProject.PathWithNamespace)
	if summary.PipelineError == nil && sourceProjectPath == "" {
		sourceProject, projectErr := api.GetProject(ctx, fmt.Sprint(sourceProjectID))
		if projectErr != nil {
			summary.PipelineError = projectErr
		} else if sourceProject.ID != sourceProjectID || sourceProject.PathWithNamespace == "" {
			summary.PipelineError = fmt.Errorf("gitlab: MR !%d source project response is incomplete", mr.MrIid)
		} else {
			sourceProjectPath = sourceProject.PathWithNamespace
		}
	}

	conflictState := gitlab.MergeRequestConflictState(remoteMR.HasConflicts, remoteMR.DetailedMergeStatus)
	conflictSHA := strToText("")
	if conflictState != nil {
		conflictSHA = strToText(remoteMR.SHA)
	}

	updated, err := h.Queries.UpdateGitLabMergeRequestEnrichment(ctx, db.UpdateGitLabMergeRequestEnrichmentParams{
		ID:                   mr.ID,
		WorkspaceID:          mr.WorkspaceID,
		WakeupSyncGeneration: syncGeneration,
		Title:                remoteMR.Title,
		State:                deriveGitLabMRState(remoteMR.State, remoteMR.Draft),
		WebUrl:               remoteMR.WebURL,
		Sha:                  remoteMR.SHA,
		Additions:            additions,
		Deletions:            deletions,
		ChangedFiles:         changedFiles,
		Reviewers:            jsonBytes(remoteMR.Reviewers),
		Assignees:            jsonBytes(remoteMR.Assignees),
		Labels:               jsonBytes(remoteMR.Labels),
		MrUpdatedAt:          parseGitLabTimeRequired(remoteMR.UpdatedAt),
		Description:          strToText(remoteMR.Description),
		SourceBranch:         strToText(remoteMR.SourceBranch),
		TargetBranch:         strToText(remoteMR.TargetBranch),
		AuthorUsername:       strToText(remoteMR.Author.Username),
		AuthorAvatarUrl:      strToText(remoteMR.Author.AvatarURL),
		MergeCommitSha:       strToText(remoteMR.MergeCommitSHA),
		DetailedMergeStatus:  strToText(remoteMR.DetailedMergeStatus),
		HasConflicts:         boolToPg(conflictState),
		ConflictSha:          conflictSHA,
		SourceProjectID:      pgtype.Int8{Int64: sourceProjectID, Valid: sourceProjectID > 0},
		SourceProjectPath:    strToText(sourceProjectPath),
		MergedAt:             parseGitLabTime(remoteMR.MergedAt),
		ClosedAt:             parseGitLabTime(remoteMR.ClosedAt),
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			current, currentErr := h.Queries.GetGitLabMergeRequestByID(ctx, db.GetGitLabMergeRequestByIDParams{
				ID:          mr.ID,
				WorkspaceID: mr.WorkspaceID,
			})
			if errors.Is(currentErr, pgx.ErrNoRows) {
				return mr, summary, nil
			}
			if currentErr != nil {
				return mr, summary, wrapGitLabRefreshPersistenceError(currentErr)
			}
			if current.WakeupSyncGeneration != syncGeneration {
				return current, summary, nil
			}
			staleErr := errors.New("GitLab returned an older MR version; retry synchronization")
			return current, summary, h.markGitLabMergeRequestRefreshErrorForGeneration(ctx, current, syncGeneration, staleErr)
		}
		return mr, summary, h.markGitLabMergeRequestRefreshErrorForGeneration(ctx, mr, syncGeneration, wrapGitLabRefreshPersistenceError(err))
	}
	summary.UpdatedMRs = 1

	if err := h.refreshGitLabApprovalState(ctx, api, updated); err != nil {
		summary.addError(err)
	}
	discussionCount, err := h.refreshGitLabDiscussions(ctx, api, updated)
	if err != nil {
		summary.addError(err)
	}
	summary.UpdatedDiscussions = discussionCount
	jobCount := 0
	if summary.PipelineError == nil {
		jobCount, err = h.refreshGitLabJobs(ctx, api, updated)
		if err != nil {
			summary.PipelineError = err
		}
	}
	summary.UpdatedJobs = jobCount
	if err := h.markGitLabMergeRequestPipelineSync(ctx, updated, summary.PipelineError); err != nil {
		summary.PipelineError = errors.Join(summary.PipelineError, err)
	}
	return updated, summary, nil
}

func (h *Handler) refreshGitLabMergeRequestFromWebhook(ctx context.Context, cfg gitlab.Config, mr db.GitlabMergeRequest) error {
	api, err := newGitLabClient(cfg)
	if err != nil {
		if markErr := h.markGitLabMergeRequestRefreshErrorForGeneration(ctx, mr, mr.WakeupSyncGeneration, err); isGitLabRefreshPersistenceError(markErr) {
			return markErr
		}
		return err
	}
	detailAPI, err := detailGitLabAPI(api)
	if err != nil {
		if markErr := h.markGitLabMergeRequestRefreshErrorForGeneration(ctx, mr, mr.WakeupSyncGeneration, err); isGitLabRefreshPersistenceError(markErr) {
			return markErr
		}
		return err
	}
	updated, summary, err := h.refreshGitLabMergeRequest(ctx, detailAPI, mr)
	if err != nil {
		return err
	}
	if summary.PersistenceError != nil {
		return summary.PersistenceError
	}
	if isGitLabRefreshPersistenceError(summary.PipelineError) {
		return summary.PipelineError
	}
	if summary.PipelineError != nil {
		return errors.New(gitLabWakeupFailureMessage(summary.PipelineError))
	}
	if len(summary.Errors) > 0 {
		syncErr := errors.New(strings.Join(summary.Errors, "; "))
		return h.markGitLabMergeRequestRefreshErrorForGeneration(ctx, updated, summary.WakeupSyncGeneration, syncErr)
	}
	return nil
}

func (h *Handler) markGitLabMergeRequestRefreshError(ctx context.Context, mr db.GitlabMergeRequest, err error) {
	_ = h.markGitLabMergeRequestRefreshErrorForGeneration(ctx, mr, mr.WakeupSyncGeneration, err)
}

func (h *Handler) markGitLabMergeRequestRefreshErrorForGeneration(ctx context.Context, mr db.GitlabMergeRequest, syncGeneration int64, syncErr error) error {
	if syncErr == nil {
		return nil
	}
	rows, err := h.Queries.MarkGitLabMergeRequestRefreshError(ctx, db.MarkGitLabMergeRequestRefreshErrorParams{
		ID:                   mr.ID,
		WorkspaceID:          mr.WorkspaceID,
		LastRefreshError:     strToText(redact.Text(syncErr.Error())),
		WakeupSyncGeneration: syncGeneration,
	})
	if err != nil {
		return errors.Join(syncErr, wrapGitLabRefreshPersistenceError(err))
	}
	if rows == 0 {
		return nil
	}
	return syncErr
}

func (h *Handler) markGitLabMergeRequestPipelineSync(ctx context.Context, mr db.GitlabMergeRequest, syncErr error) error {
	if syncErr != nil {
		_, err := h.Queries.MarkGitLabMergeRequestPipelineSync(ctx, db.MarkGitLabMergeRequestPipelineSyncParams{
			ID:                   mr.ID,
			WorkspaceID:          mr.WorkspaceID,
			PipelineSyncError:    strToText(gitLabWakeupFailureMessage(syncErr)),
			WakeupSyncGeneration: mr.WakeupSyncGeneration,
			Sha:                  mr.Sha,
		})
		return wrapGitLabRefreshPersistenceError(err)
	}
	_, err := h.Queries.MarkGitLabMergeRequestPipelineSyncSuccess(ctx, db.MarkGitLabMergeRequestPipelineSyncSuccessParams{
		ID:                   mr.ID,
		WorkspaceID:          mr.WorkspaceID,
		WakeupSyncGeneration: mr.WakeupSyncGeneration,
		Sha:                  mr.Sha,
	})
	return wrapGitLabRefreshPersistenceError(err)
}

func (h *Handler) refreshGitLabApprovalState(ctx context.Context, api gitLabDetailAPI, mr db.GitlabMergeRequest) error {
	approval, err := api.GetMergeRequestApprovalState(ctx, mr.GitlabProjectID, mr.MrIid)
	if err != nil {
		return err
	}
	_, err = h.Queries.UpsertGitLabMRApprovalState(ctx, db.UpsertGitLabMRApprovalStateParams{
		MergeRequestID:    mr.ID,
		WorkspaceID:       mr.WorkspaceID,
		Approved:          approval.Approved,
		ApprovalsRequired: ptrToInt4(approval.ApprovalsRequired),
		ApprovalsLeft:     ptrToInt4(approval.ApprovalsLeft),
		ApprovedBy:        jsonBytes(approval.ApprovedBy),
		Rules:             jsonBytes(approval.Rules),
		RawState:          jsonBytes(approval),
	})
	return wrapGitLabRefreshPersistenceError(err)
}

func (h *Handler) refreshGitLabDiscussions(ctx context.Context, api gitLabDetailAPI, mr db.GitlabMergeRequest) (int, error) {
	discussions, err := api.ListMergeRequestDiscussions(ctx, mr.GitlabProjectID, mr.MrIid)
	if err != nil {
		return 0, err
	}
	for _, discussion := range discussions {
		resolved := discussionResolved(discussion)
		row, err := h.Queries.UpsertGitLabMRDiscussion(ctx, db.UpsertGitLabMRDiscussionParams{
			WorkspaceID:         mr.WorkspaceID,
			MergeRequestID:      mr.ID,
			GitlabDiscussionID:  discussion.ID,
			IndividualNote:      discussion.IndividualNote,
			Resolved:            boolToPg(resolved),
			DiscussionCreatedAt: firstNoteTime(discussion.Notes, true),
			DiscussionUpdatedAt: firstNoteTime(discussion.Notes, false),
		})
		if err != nil {
			return 0, wrapGitLabRefreshPersistenceError(err)
		}
		for _, note := range discussion.Notes {
			if _, err := h.Queries.UpsertGitLabMRNote(ctx, db.UpsertGitLabMRNoteParams{
				WorkspaceID:     mr.WorkspaceID,
				DiscussionID:    row.ID,
				MergeRequestID:  mr.ID,
				GitlabNoteID:    note.ID,
				Body:            note.Body,
				System:          note.System,
				AuthorUsername:  strToText(note.Author.Username),
				AuthorAvatarUrl: strToText(note.Author.AvatarURL),
				Resolved:        boolToPg(note.Resolved),
				Resolvable:      boolToPg(note.Resolvable),
				NoteCreatedAt:   parseGitLabTime(note.CreatedAt),
				NoteUpdatedAt:   parseGitLabTime(note.UpdatedAt),
			}); err != nil {
				return 0, wrapGitLabRefreshPersistenceError(err)
			}
		}
	}
	return len(discussions), nil
}

func (h *Handler) refreshGitLabJobs(ctx context.Context, api gitLabDetailAPI, mr db.GitlabMergeRequest) (int, error) {
	if !mr.SourceProjectID.Valid || mr.SourceProjectID.Int64 <= 0 || !mr.SourceBranch.Valid || mr.SourceBranch.String == "" {
		return 0, fmt.Errorf("gitlab: MR !%d has no readable source project or branch", mr.MrIid)
	}
	sourceProjectID := mr.SourceProjectID.Int64
	sourceBranch := mr.SourceBranch.String
	mrPipelines, err := api.ListMergeRequestPipelines(ctx, mr.GitlabProjectID, mr.MrIid)
	if err != nil {
		return 0, err
	}
	sourcePipelines, err := api.ListProjectPipelines(ctx, sourceProjectID, gitlab.PipelineFilters{SHA: mr.Sha, Ref: sourceBranch})
	if err != nil {
		return 0, fmt.Errorf("gitlab: read MR !%d source-branch pipelines: %w", mr.MrIid, err)
	}

	type pipelineIdentity struct {
		projectID  int64
		pipelineID int64
	}
	byID := make(map[pipelineIdentity]gitLabVersionedPipeline, len(mrPipelines)+len(sourcePipelines))
	add := func(pipeline gitlab.Pipeline, defaultProjectID int64, requireSourceRef bool) error {
		if pipeline.SHA != mr.Sha || (requireSourceRef && pipeline.Ref != sourceBranch) {
			return nil
		}
		if pipeline.ID <= 0 {
			return nil
		}
		projectID := pipeline.ProjectID
		if projectID <= 0 {
			projectID = defaultProjectID
		}
		if projectID != sourceProjectID && projectID != mr.GitlabProjectID {
			return fmt.Errorf("gitlab: pipeline %d belongs to unexpected project %d", pipeline.ID, projectID)
		}
		updatedAt, hasTimestamp := gitLabPipelineUpdatedAt(pipeline)
		if !requireSourceRef || pipeline.ProjectID <= 0 || pipeline.UpdatedAt == "" || pipeline.Status == "" {
			detail, detailErr := api.GetPipeline(ctx, projectID, pipeline.ID)
			if detailErr != nil && projectID != sourceProjectID {
				detail, detailErr = api.GetPipeline(ctx, sourceProjectID, pipeline.ID)
				if detailErr == nil {
					projectID = sourceProjectID
				}
			}
			if detailErr != nil {
				return fmt.Errorf("gitlab: read pipeline %d details: %w", pipeline.ID, detailErr)
			}
			if detail.ID != pipeline.ID {
				return fmt.Errorf("gitlab: pipeline detail response does not match pipeline %d", pipeline.ID)
			}
			if detail.ProjectID > 0 {
				projectID = detail.ProjectID
			}
			if projectID != sourceProjectID && projectID != mr.GitlabProjectID {
				return fmt.Errorf("gitlab: pipeline %d belongs to unexpected project %d", pipeline.ID, projectID)
			}
			pipeline.ProjectID = projectID
			pipeline.SHA = firstNonEmpty(detail.SHA, pipeline.SHA)
			pipeline.Ref = firstNonEmpty(detail.Ref, pipeline.Ref)
			pipeline.Status = firstNonEmpty(detail.Status, pipeline.Status)
			pipeline.WebURL = firstNonEmpty(detail.WebURL, pipeline.WebURL)
			pipeline.UpdatedAt = firstNonEmpty(detail.UpdatedAt, pipeline.UpdatedAt)
			pipeline.FinishedAt = firstNonEmpty(detail.FinishedAt, pipeline.FinishedAt)
			pipeline.CreatedAt = firstNonEmpty(detail.CreatedAt, pipeline.CreatedAt)
		}
		if pipeline.SHA != mr.Sha || (requireSourceRef && pipeline.Ref != sourceBranch) {
			return nil
		}
		updatedAt, hasTimestamp = gitLabPipelineUpdatedAt(pipeline)
		if !hasTimestamp {
			return fmt.Errorf("gitlab: pipeline %d has no update timestamp", pipeline.ID)
		}
		pipeline.ProjectID = projectID
		identity := pipelineIdentity{projectID: projectID, pipelineID: pipeline.ID}
		if current, ok := byID[identity]; !ok || updatedAt.After(current.updatedAt) {
			byID[identity] = gitLabVersionedPipeline{pipeline: pipeline, updatedAt: updatedAt}
		}
		return nil
	}
	for _, pipeline := range mrPipelines {
		if err := add(pipeline, mr.GitlabProjectID, false); err != nil {
			return 0, err
		}
	}
	for _, pipeline := range sourcePipelines {
		if err := add(pipeline, sourceProjectID, true); err != nil {
			return 0, err
		}
	}
	pipelines := make([]gitLabVersionedPipeline, 0, len(byID))
	for _, pipeline := range byID {
		pipelines = append(pipelines, pipeline)
	}
	sort.Slice(pipelines, func(i, j int) bool {
		if pipelines[i].pipeline.ProjectID != pipelines[j].pipeline.ProjectID {
			return pipelines[i].pipeline.ProjectID < pipelines[j].pipeline.ProjectID
		}
		return pipelines[i].pipeline.ID < pipelines[j].pipeline.ID
	})
	persisted, err := h.persistGitLabMergeRequestPipelines(ctx, mr, pipelines)
	if err != nil {
		return 0, err
	}
	if !persisted {
		return 0, nil
	}
	count := 0
	for _, version := range pipelines {
		pipeline := version.pipeline
		projectID := pipeline.ProjectID
		jobs, err := api.ListPipelineJobs(ctx, projectID, pipeline.ID)
		if err != nil {
			return count, err
		}
		for _, job := range jobs {
			row, err := h.Queries.UpsertGitLabPipelineJob(ctx, gitLabPipelineJobParams(mr, pipeline.ID, job))
			if err != nil {
				return count, wrapGitLabRefreshPersistenceError(err)
			}
			count++
			if strings.EqualFold(job.Status, "failed") {
				trace, err := api.GetJobTrace(ctx, projectID, job.ID, gitLabFailedJobTraceLimit)
				if err != nil {
					continue
				}
				if err := h.Queries.UpdateGitLabPipelineJobTraceSummary(ctx, db.UpdateGitLabPipelineJobTraceSummaryParams{
					ID:             row.ID,
					WorkspaceID:    row.WorkspaceID,
					TraceSummary:   strToText(redact.Text(trace.Text)),
					TraceTruncated: trace.Truncated,
				}); err != nil {
					return count, wrapGitLabRefreshPersistenceError(err)
				}
			}
		}
	}
	return count, nil
}

func (h *Handler) persistGitLabMergeRequestPipelines(ctx context.Context, mr db.GitlabMergeRequest, pipelines []gitLabVersionedPipeline) (bool, error) {
	if h.TxStarter == nil {
		return false, wrapGitLabRefreshPersistenceError(errors.New("database transactions are unavailable"))
	}
	tx, err := h.TxStarter.Begin(ctx)
	if err != nil {
		return false, wrapGitLabRefreshPersistenceError(err)
	}
	defer tx.Rollback(ctx)
	q := h.Queries.WithTx(tx)
	if _, err := q.LockGitLabMergeRequestWakeupSync(ctx, db.LockGitLabMergeRequestWakeupSyncParams{
		ID:                   mr.ID,
		WorkspaceID:          mr.WorkspaceID,
		WakeupSyncGeneration: mr.WakeupSyncGeneration,
		Sha:                  mr.Sha,
	}); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return false, nil
		}
		return false, wrapGitLabRefreshPersistenceError(err)
	}
	if _, err := q.DeleteGitLabMRPipelinesByMR(ctx, mr.ID); err != nil {
		return false, wrapGitLabRefreshPersistenceError(err)
	}
	for _, version := range pipelines {
		pipeline := version.pipeline
		if err := q.UpsertGitLabMRPipeline(ctx, db.UpsertGitLabMRPipelineParams{
			MergeRequestID:       mr.ID,
			WorkspaceID:          mr.WorkspaceID,
			WakeupSyncGeneration: mr.WakeupSyncGeneration,
			PipelineID:           pipeline.ID,
			PipelineProjectID:    pipeline.ProjectID,
			Sha:                  pipeline.SHA,
			Ref:                  strToText(pipeline.Ref),
			Status:               normalizeGitLabPipelineStatus(pipeline.Status),
			WebUrl:               strToText(pipeline.WebURL),
			PipelineUpdatedAt:    pgtype.Timestamptz{Time: version.updatedAt, Valid: true},
		}); err != nil {
			return false, wrapGitLabRefreshPersistenceError(err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return false, wrapGitLabRefreshPersistenceError(err)
	}
	return true, nil
}

func gitLabPipelineUpdatedAt(pipeline gitlab.Pipeline) (time.Time, bool) {
	for _, value := range []string{pipeline.UpdatedAt, pipeline.FinishedAt} {
		if timestamp := parseGitLabTime(value); timestamp.Valid {
			return timestamp.Time, true
		}
	}
	return time.Time{}, false
}

func gitLabPipelineJobParams(mr db.GitlabMergeRequest, pipelineID int64, job gitlab.Job) db.UpsertGitLabPipelineJobParams {
	var artifactName pgtype.Text
	var artifactSize pgtype.Int8
	if job.ArtifactsFile != nil {
		artifactName = strToText(job.ArtifactsFile.Filename)
		artifactSize = pgtype.Int8{Int64: job.ArtifactsFile.Size, Valid: true}
	}
	return db.UpsertGitLabPipelineJobParams{
		WorkspaceID:           mr.WorkspaceID,
		MergeRequestID:        mr.ID,
		PipelineID:            pipelineID,
		JobID:                 job.ID,
		Name:                  job.Name,
		Status:                job.Status,
		AllowFailure:          job.AllowFailure,
		Stage:                 strToText(job.Stage),
		Ref:                   strToText(job.Ref),
		Sha:                   strToText(job.SHA),
		WebUrl:                strToText(job.WebURL),
		StartedAt:             parseGitLabTime(job.StartedAt),
		FinishedAt:            parseGitLabTime(job.FinishedAt),
		DurationSeconds:       float64PtrToPg(job.Duration),
		QueuedDurationSeconds: float64PtrToPg(job.QueuedDuration),
		FailureReason:         strToText(job.FailureReason),
		ArtifactsFileName:     artifactName,
		ArtifactsFileSize:     artifactSize,
		ArtifactsExpireAt:     parseGitLabTime(job.ArtifactsExpireAt),
	}
}

func detailGitLabAPI(api gitLabAPI) (gitLabDetailAPI, error) {
	detail, ok := api.(gitLabDetailAPI)
	if !ok {
		return nil, fmt.Errorf("gitlab client does not support details refresh")
	}
	return detail, nil
}

func jsonBytes(v any) []byte {
	raw, err := json.Marshal(v)
	if err != nil || len(raw) == 0 || string(raw) == "null" {
		return []byte("[]")
	}
	return raw
}

func boolToPg(v *bool) pgtype.Bool {
	if v == nil {
		return pgtype.Bool{}
	}
	return pgtype.Bool{Bool: *v, Valid: true}
}

func discussionResolved(d gitlab.Discussion) *bool {
	if d.Resolved != nil {
		return d.Resolved
	}
	for _, note := range d.Notes {
		if note.Resolved != nil {
			return note.Resolved
		}
	}
	return nil
}

func firstNoteTime(notes []gitlab.Note, created bool) pgtype.Timestamptz {
	for _, note := range notes {
		if created {
			if t := parseGitLabTime(note.CreatedAt); t.Valid {
				return t
			}
		} else if t := parseGitLabTime(note.UpdatedAt); t.Valid {
			return t
		}
	}
	return pgtype.Timestamptz{}
}

func float64PtrToPg(v *float64) pgtype.Float8 {
	if v == nil {
		return pgtype.Float8{}
	}
	return pgtype.Float8{Float64: *v, Valid: true}
}
