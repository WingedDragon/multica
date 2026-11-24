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

func (s *gitLabRefreshSummary) addError(err error) {
	if err == nil {
		return
	}
	if isGitLabRefreshPersistenceError(err) {
		s.PersistenceError = errors.Join(s.PersistenceError, err)
	}
	s.Errors = append(s.Errors, redact.Text(err.Error()))
}

func gitLabPipelineUpdatedAt(pipeline gitlab.Pipeline) (time.Time, bool) {
	for _, value := range []string{pipeline.UpdatedAt, pipeline.FinishedAt, pipeline.CreatedAt} {
		if timestamp := parseGitLabTime(value); timestamp.Valid {
			return timestamp.Time, true
		}
	}
	return time.Time{}, false
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

type gitLabCanonicalVersionedPipeline struct {
	pipeline    gitlab.Pipeline
	projectPath string
	updatedAt   time.Time
}

func (h *Handler) syncVCSGitLabMergeRequest(ctx context.Context, api gitLabDetailAPI, project db.VcsGitlabProject, remoteMR gitlab.MergeRequest) (db.VcsPullRequest, gitLabRefreshSummary, error) {
	return h.syncVCSGitLabMergeRequestAtGeneration(ctx, api, project, remoteMR, nil, 0)
}

func (h *Handler) refreshVCSGitLabMergeRequest(ctx context.Context, api gitLabDetailAPI, project db.VcsGitlabProject, current db.VcsPullRequest) (db.VcsPullRequest, gitLabRefreshSummary, error) {
	var summary gitLabRefreshSummary
	if _, err := h.Queries.EnsureVCSGitLabMRDetail(ctx, db.EnsureVCSGitLabMRDetailParams{
		WorkspaceID: current.WorkspaceID, PullRequestID: current.ID,
	}); err != nil {
		return current, summary, wrapGitLabRefreshPersistenceError(err)
	}
	generation, err := h.Queries.BeginGitLabVCSPullRequestWakeupSync(ctx, db.BeginGitLabVCSPullRequestWakeupSyncParams{
		ID: current.ID, WorkspaceID: current.WorkspaceID,
	})
	if err != nil {
		return current, summary, wrapGitLabRefreshPersistenceError(err)
	}
	current.WakeupSyncGeneration = generation
	summary.WakeupSyncGeneration = generation
	remoteMR, err := api.GetMergeRequest(ctx, project.GitlabProjectID, current.PrNumber)
	if err != nil {
		return current, summary, h.markVCSGitLabMergeRequestRefreshErrorForGeneration(ctx, current, generation, err)
	}
	return h.syncVCSGitLabMergeRequestAtGeneration(ctx, api, project, remoteMR, &current, generation)
}

func (h *Handler) syncVCSGitLabMergeRequestAtGeneration(ctx context.Context, api gitLabDetailAPI, project db.VcsGitlabProject, remoteMR gitlab.MergeRequest, current *db.VcsPullRequest, generation int64) (db.VcsPullRequest, gitLabRefreshSummary, error) {
	var summary gitLabRefreshSummary
	projectOwner, projectName, ok := splitGitLabProjectPath(project.PathWithNamespace)
	if !ok || project.GitlabProjectID <= 0 {
		return db.VcsPullRequest{}, summary, errors.New("GitLab project identity is invalid")
	}
	if remoteMR.IID <= 0 {
		return db.VcsPullRequest{}, summary, errors.New("GitLab merge request number is invalid")
	}
	if current != nil && remoteMR.IID != current.PrNumber {
		return *current, summary, fmt.Errorf("gitlab: requested MR !%d but received MR !%d", current.PrNumber, remoteMR.IID)
	}
	if remoteMR.TargetProjectID != 0 && remoteMR.TargetProjectID != project.GitlabProjectID {
		return db.VcsPullRequest{}, summary, fmt.Errorf("gitlab: MR !%d belongs to another target project", remoteMR.IID)
	}
	updatedAt := parseGitLabTimeRequired(remoteMR.UpdatedAt)
	if !updatedAt.Valid {
		return db.VcsPullRequest{}, summary, fmt.Errorf("gitlab: MR !%d has no valid update timestamp", remoteMR.IID)
	}
	createdAt := parseGitLabTimeRequired(remoteMR.CreatedAt)
	if !createdAt.Valid {
		return db.VcsPullRequest{}, summary, fmt.Errorf("gitlab: MR !%d has no valid creation timestamp", remoteMR.IID)
	}

	var previous db.VcsPullRequest
	var previousErr error
	if current != nil {
		previous = *current
	} else {
		previous, previousErr = h.Queries.GetVCSGitLabPullRequestByWakeupIdentity(ctx, db.GetVCSGitLabPullRequestByWakeupIdentityParams{
			WorkspaceID: project.WorkspaceID, ConnectionID: project.ConnectionID,
			RepoOwner: projectOwner, RepoName: projectName, PrNumber: remoteMR.IID,
		})
		if previousErr != nil && !errors.Is(previousErr, pgx.ErrNoRows) {
			return db.VcsPullRequest{}, summary, wrapGitLabRefreshPersistenceError(previousErr)
		}
	}
	additions, deletions, changedFiles := int32(0), int32(0), int32(0)
	if previousErr == nil || current != nil {
		additions, deletions, changedFiles = previous.Additions, previous.Deletions, previous.ChangedFiles
	}
	changes, changesErr := api.GetMergeRequestChanges(ctx, project.GitlabProjectID, remoteMR.IID)
	if changesErr == nil {
		additions, deletions, changedFiles = changes.Additions(), changes.Deletions(), changes.ChangedFiles()
	} else {
		summary.addError(changesErr)
	}

	sourceProjectID := remoteMR.SourceProjectID
	if sourceProjectID == 0 {
		sourceProjectID = remoteMR.SourceProject.ID
	}
	sourceProjectPath := strings.TrimSpace(remoteMR.SourceProject.PathWithNamespace)
	if sourceProjectID <= 0 && previous.SourceProjectID.Valid {
		sourceProjectID = previous.SourceProjectID.Int64
	}
	if sourceProjectPath == "" && previous.SourceProjectPath.Valid {
		sourceProjectPath = previous.SourceProjectPath.String
	}
	if sourceProjectID <= 0 {
		sourceProjectID = 0
		sourceProjectPath = ""
	}
	conflictState := gitlab.MergeRequestConflictState(remoteMR.HasConflicts, remoteMR.DetailedMergeStatus)
	conflictHeadSHA := ""
	if conflictState != nil {
		conflictHeadSHA = remoteMR.SHA
	}
	pr, err := h.Queries.UpsertVCSPullRequest(ctx, db.UpsertVCSPullRequestParams{
		WorkspaceID: project.WorkspaceID, ConnectionID: project.ConnectionID,
		Provider: "gitlab", RepoOwner: projectOwner, RepoName: projectName,
		PrNumber: remoteMR.IID, Title: remoteMR.Title,
		State: deriveGitLabMRState(remoteMR.State, remoteMR.Draft), HtmlUrl: remoteMR.WebURL,
		Branch:      strToText(remoteMR.SourceBranch),
		AuthorLogin: strToText(remoteMR.Author.Username), AuthorAvatarUrl: strToText(remoteMR.Author.AvatarURL),
		MergedAt: parseGitLabTime(remoteMR.MergedAt), ClosedAt: parseGitLabTime(remoteMR.ClosedAt),
		PrCreatedAt: createdAt, PrUpdatedAt: updatedAt,
		Additions: additions, Deletions: deletions, ChangedFiles: changedFiles,
		HeadSha: remoteMR.SHA, HasConflicts: boolToPg(conflictState), ConflictHeadSha: conflictHeadSHA,
		SourceProjectID:   pgtype.Int8{Int64: sourceProjectID, Valid: sourceProjectID > 0},
		SourceProjectPath: strToText(sourceProjectPath),
	})
	if err != nil {
		if !errors.Is(err, pgx.ErrNoRows) {
			return db.VcsPullRequest{}, summary, wrapGitLabRefreshPersistenceError(err)
		}
		pr, err = h.Queries.GetVCSGitLabPullRequestByWakeupIdentity(ctx, db.GetVCSGitLabPullRequestByWakeupIdentityParams{
			WorkspaceID: project.WorkspaceID, ConnectionID: project.ConnectionID,
			RepoOwner: projectOwner, RepoName: projectName, PrNumber: remoteMR.IID,
		})
		if err != nil {
			return db.VcsPullRequest{}, summary, wrapGitLabRefreshPersistenceError(err)
		}
		if pr.PrUpdatedAt.Valid && updatedAt.Time.Before(pr.PrUpdatedAt.Time) {
			return pr, summary, nil
		}
	}
	if current != nil && pr.ID != current.ID {
		return db.VcsPullRequest{}, summary, errors.New("GitLab merge request identity changed")
	}
	summary.UpdatedMRs = 1
	if generation == 0 {
		if _, err := h.Queries.EnsureVCSGitLabMRDetail(ctx, db.EnsureVCSGitLabMRDetailParams{
			WorkspaceID: pr.WorkspaceID, PullRequestID: pr.ID,
		}); err != nil {
			return pr, summary, wrapGitLabRefreshPersistenceError(err)
		}
		generation, err = h.Queries.BeginGitLabVCSPullRequestWakeupSync(ctx, db.BeginGitLabVCSPullRequestWakeupSyncParams{
			ID: pr.ID, WorkspaceID: pr.WorkspaceID,
		})
		if err != nil {
			return pr, summary, wrapGitLabRefreshPersistenceError(err)
		}
	}
	pr.WakeupSyncGeneration = generation
	summary.WakeupSyncGeneration = generation
	if sourceProjectID > 0 && sourceProjectPath == "" {
		sourceProject, projectErr := api.GetProject(ctx, fmt.Sprint(sourceProjectID))
		if projectErr != nil {
			summary.PipelineError = projectErr
		} else if sourceProject.ID != sourceProjectID || sourceProject.PathWithNamespace == "" {
			summary.PipelineError = fmt.Errorf("gitlab: MR !%d source project response is incomplete", remoteMR.IID)
		} else {
			sourceProjectPath = sourceProject.PathWithNamespace
		}
	}
	if sourceProjectID > 0 && remoteMR.SourceProject.ID != 0 && remoteMR.SourceProject.ID != sourceProjectID {
		summary.PipelineError = fmt.Errorf("gitlab: MR !%d source project identity is inconsistent", remoteMR.IID)
	}

	detailParams := db.UpsertVCSGitLabMRDetailParams{
		WorkspaceID: pr.WorkspaceID, PullRequestID: pr.ID,
		WakeupSyncGeneration: generation,
		Description:          strToText(remoteMR.Description), MergeCommitSha: strToText(remoteMR.MergeCommitSHA),
		DetailedMergeStatus: strToText(remoteMR.DetailedMergeStatus), TargetBranch: strToText(remoteMR.TargetBranch),
		Reviewers: jsonBytes(remoteMR.Reviewers), Assignees: jsonBytes(remoteMR.Assignees), Labels: jsonBytes(remoteMR.Labels),
		LastRefreshedAt:  pgtype.Timestamptz{Time: time.Now(), Valid: true},
		LastRefreshError: pgtype.Text{}, ConflictSha: strToText(conflictHeadSHA),
	}
	if err := h.refreshVCSGitLabApprovalState(ctx, api, pr, project.GitlabProjectID, remoteMR.IID); err != nil {
		summary.addError(err)
	}
	discussionCount, err := h.refreshVCSGitLabDiscussions(ctx, api, pr, project.GitlabProjectID, remoteMR.IID)
	if err != nil {
		summary.addError(err)
	}
	summary.UpdatedDiscussions = discussionCount
	jobCount := 0
	if summary.PipelineError == nil {
		jobCount, err = h.refreshVCSGitLabJobs(ctx, api, pr, project.GitlabProjectID, project.PathWithNamespace, sourceProjectPath)
		if err != nil {
			summary.PipelineError = err
		}
	}
	summary.UpdatedJobs = jobCount
	if err := h.markVCSGitLabMRPipelineSync(ctx, pr, summary.PipelineError); err != nil {
		summary.PipelineError = errors.Join(summary.PipelineError, err)
	}
	if isGitLabRefreshPersistenceError(summary.PipelineError) {
		summary.PersistenceError = errors.Join(summary.PersistenceError, summary.PipelineError)
	}
	detailParams.PipelineSyncAt = pgtype.Timestamptz{Time: time.Now(), Valid: true}
	if summary.PipelineError != nil {
		detailParams.PipelineSyncError = strToText(gitLabWakeupFailureMessage(summary.PipelineError))
	}
	if len(summary.Errors) > 0 {
		detailParams.LastRefreshError = strToText(strings.Join(summary.Errors, "; "))
	}
	if _, err := h.Queries.UpsertVCSGitLabMRDetail(ctx, detailParams); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return pr, summary, nil
		}
		summary.addError(wrapGitLabRefreshPersistenceError(err))
	}
	if summary.PersistenceError != nil {
		return pr, summary, summary.PersistenceError
	}
	if len(summary.Errors) > 0 {
		refreshErr := errors.New(strings.Join(summary.Errors, "; "))
		if err := h.markVCSGitLabMergeRequestRefreshErrorForGeneration(ctx, pr, generation, refreshErr); err != nil && isGitLabRefreshPersistenceError(err) {
			return pr, summary, err
		}
	}
	if summary.PipelineError != nil {
		summary.Errors = append(summary.Errors, redact.Text(summary.PipelineError.Error()))
	}
	return pr, summary, nil
}

func (h *Handler) markVCSGitLabMergeRequestRefreshErrorForGeneration(ctx context.Context, pr db.VcsPullRequest, generation int64, syncErr error) error {
	if syncErr == nil {
		return nil
	}
	message := redact.Text(syncErr.Error())
	rows, err := h.Queries.MarkGitLabVCSPullRequestWakeupSyncError(ctx, db.MarkGitLabVCSPullRequestWakeupSyncErrorParams{
		ID: pr.ID, WorkspaceID: pr.WorkspaceID, StatusSyncError: strToText(message),
		WakeupSyncGeneration: generation,
	})
	if err != nil {
		return errors.Join(syncErr, wrapGitLabRefreshPersistenceError(err))
	}
	if rows == 0 {
		return nil
	}
	_, detailErr := h.Queries.MarkVCSGitLabMRRefreshError(ctx, db.MarkVCSGitLabMRRefreshErrorParams{
		PullRequestID: pr.ID, WorkspaceID: pr.WorkspaceID,
		LastRefreshError: strToText(message), WakeupSyncGeneration: generation,
	})
	if detailErr != nil {
		return errors.Join(syncErr, wrapGitLabRefreshPersistenceError(detailErr))
	}
	return syncErr
}

func (h *Handler) markVCSGitLabMRPipelineSync(ctx context.Context, pr db.VcsPullRequest, syncErr error) error {
	if syncErr != nil {
		_, err := h.Queries.MarkGitLabVCSPullRequestPipelineSync(ctx, db.MarkGitLabVCSPullRequestPipelineSyncParams{
			ID: pr.ID, WorkspaceID: pr.WorkspaceID,
			PipelineSyncError:    strToText(gitLabWakeupFailureMessage(syncErr)),
			WakeupSyncGeneration: pr.WakeupSyncGeneration, HeadSha: pr.HeadSha,
		})
		if err != nil {
			return wrapGitLabRefreshPersistenceError(err)
		}
		_, err = h.Queries.MarkVCSGitLabMRPipelineSync(ctx, db.MarkVCSGitLabMRPipelineSyncParams{
			PullRequestID: pr.ID, WorkspaceID: pr.WorkspaceID,
			PipelineSyncError:    strToText(gitLabWakeupFailureMessage(syncErr)),
			WakeupSyncGeneration: pr.WakeupSyncGeneration, HeadSha: pr.HeadSha,
		})
		return wrapGitLabRefreshPersistenceError(err)
	}
	_, err := h.Queries.MarkGitLabVCSPullRequestPipelineSyncSuccess(ctx, db.MarkGitLabVCSPullRequestPipelineSyncSuccessParams{
		ID: pr.ID, WorkspaceID: pr.WorkspaceID,
		WakeupSyncGeneration: pr.WakeupSyncGeneration, HeadSha: pr.HeadSha,
	})
	if err != nil {
		return wrapGitLabRefreshPersistenceError(err)
	}
	_, err = h.Queries.MarkVCSGitLabMRPipelineSyncSuccess(ctx, db.MarkVCSGitLabMRPipelineSyncSuccessParams{
		PullRequestID: pr.ID, WorkspaceID: pr.WorkspaceID,
		WakeupSyncGeneration: pr.WakeupSyncGeneration, HeadSha: pr.HeadSha,
	})
	return wrapGitLabRefreshPersistenceError(err)
}

func (h *Handler) refreshVCSGitLabApprovalState(ctx context.Context, api gitLabDetailAPI, pr db.VcsPullRequest, projectID int64, iid int32) error {
	approval, err := api.GetMergeRequestApprovalState(ctx, projectID, iid)
	if err != nil {
		return err
	}
	_, err = h.Queries.UpsertGitLabMRApprovalStateByPullRequest(ctx, db.UpsertGitLabMRApprovalStateByPullRequestParams{
		PullRequestID: pr.ID, WorkspaceID: pr.WorkspaceID, WakeupSyncGeneration: pr.WakeupSyncGeneration,
		Approved:          approval.Approved,
		ApprovalsRequired: ptrToInt4(approval.ApprovalsRequired), ApprovalsLeft: ptrToInt4(approval.ApprovalsLeft),
		ApprovedBy: jsonBytes(approval.ApprovedBy), Rules: jsonBytes(approval.Rules), RawState: jsonBytes(approval),
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	return wrapGitLabRefreshPersistenceError(err)
}

func (h *Handler) refreshVCSGitLabDiscussions(ctx context.Context, api gitLabDetailAPI, pr db.VcsPullRequest, projectID int64, iid int32) (int, error) {
	discussions, err := api.ListMergeRequestDiscussions(ctx, projectID, iid)
	if err != nil {
		return 0, err
	}
	for _, discussion := range discussions {
		row, err := h.Queries.UpsertGitLabMRDiscussionByPullRequest(ctx, db.UpsertGitLabMRDiscussionByPullRequestParams{
			WorkspaceID: pr.WorkspaceID, PullRequestID: pr.ID, WakeupSyncGeneration: pr.WakeupSyncGeneration,
			GitlabDiscussionID: discussion.ID, IndividualNote: discussion.IndividualNote,
			Resolved:            boolToPg(discussionResolved(discussion)),
			DiscussionCreatedAt: firstNoteTime(discussion.Notes, true),
			DiscussionUpdatedAt: firstNoteTime(discussion.Notes, false),
		})
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return 0, nil
			}
			return 0, wrapGitLabRefreshPersistenceError(err)
		}
		for _, note := range discussion.Notes {
			if _, err := h.Queries.UpsertGitLabMRNoteByPullRequest(ctx, db.UpsertGitLabMRNoteByPullRequestParams{
				WorkspaceID: pr.WorkspaceID, DiscussionID: row.ID, PullRequestID: pr.ID,
				WakeupSyncGeneration: pr.WakeupSyncGeneration,
				GitlabNoteID:         note.ID, Body: note.Body, System: note.System,
				AuthorUsername: strToText(note.Author.Username), AuthorAvatarUrl: strToText(note.Author.AvatarURL),
				Resolved: boolToPg(note.Resolved), Resolvable: boolToPg(note.Resolvable),
				NoteCreatedAt: parseGitLabTime(note.CreatedAt), NoteUpdatedAt: parseGitLabTime(note.UpdatedAt),
			}); err != nil {
				if errors.Is(err, pgx.ErrNoRows) {
					return 0, nil
				}
				return 0, wrapGitLabRefreshPersistenceError(err)
			}
		}
	}
	return len(discussions), nil
}

func (h *Handler) refreshVCSGitLabJobs(ctx context.Context, api gitLabDetailAPI, pr db.VcsPullRequest, targetProjectID int64, targetProjectPath, sourceProjectPath string) (int, error) {
	if !pr.SourceProjectID.Valid || pr.SourceProjectID.Int64 <= 0 || !pr.Branch.Valid || pr.Branch.String == "" {
		return 0, fmt.Errorf("gitlab: MR !%d has no readable source project or branch", pr.PrNumber)
	}
	sourceProjectID := pr.SourceProjectID.Int64
	sourceBranch := pr.Branch.String
	if sourceProjectID != targetProjectID && strings.TrimSpace(sourceProjectPath) == "" && pr.SourceProjectPath.Valid {
		sourceProjectPath = pr.SourceProjectPath.String
	}
	if sourceProjectID == targetProjectID {
		sourceProjectPath = ""
	}
	if sourceProjectID != targetProjectID && strings.TrimSpace(sourceProjectPath) == "" {
		sourceProject, err := api.GetProject(ctx, fmt.Sprint(sourceProjectID))
		if err != nil {
			return 0, err
		}
		if sourceProject.ID != sourceProjectID || sourceProject.PathWithNamespace == "" {
			return 0, errors.New("GitLab source project response is incomplete")
		}
		sourceProjectPath = sourceProject.PathWithNamespace
	}
	mrPipelines, err := api.ListMergeRequestPipelines(ctx, targetProjectID, pr.PrNumber)
	if err != nil {
		return 0, err
	}
	sourcePipelines, err := api.ListProjectPipelines(ctx, sourceProjectID, gitlab.PipelineFilters{SHA: pr.HeadSha, Ref: sourceBranch})
	if err != nil {
		return 0, fmt.Errorf("gitlab: read MR !%d source-branch pipelines: %w", pr.PrNumber, err)
	}
	type pipelineIdentity struct {
		projectID  int64
		pipelineID int64
	}
	byIdentity := make(map[pipelineIdentity]gitLabCanonicalVersionedPipeline, len(mrPipelines)+len(sourcePipelines))
	add := func(pipeline gitlab.Pipeline, defaultProjectID int64, requireSourceRef bool) error {
		if pipeline.SHA != pr.HeadSha || (requireSourceRef && pipeline.Ref != sourceBranch) || pipeline.ID <= 0 {
			return nil
		}
		projectID := pipeline.ProjectID
		if projectID <= 0 {
			projectID = defaultProjectID
		}
		if projectID != sourceProjectID && projectID != targetProjectID {
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
			pipeline.ProjectID = projectID
			pipeline.SHA = firstNonEmpty(detail.SHA, pipeline.SHA)
			pipeline.Ref = firstNonEmpty(detail.Ref, pipeline.Ref)
			pipeline.Status = firstNonEmpty(detail.Status, pipeline.Status)
			pipeline.WebURL = firstNonEmpty(detail.WebURL, pipeline.WebURL)
			pipeline.UpdatedAt = firstNonEmpty(detail.UpdatedAt, pipeline.UpdatedAt)
			pipeline.FinishedAt = firstNonEmpty(detail.FinishedAt, pipeline.FinishedAt)
			pipeline.CreatedAt = firstNonEmpty(detail.CreatedAt, pipeline.CreatedAt)
		}
		if projectID != sourceProjectID && projectID != targetProjectID {
			return fmt.Errorf("gitlab: pipeline %d belongs to unexpected project %d", pipeline.ID, projectID)
		}
		if pipeline.SHA != pr.HeadSha || (requireSourceRef && pipeline.Ref != sourceBranch) {
			return nil
		}
		updatedAt, hasTimestamp = gitLabPipelineUpdatedAt(pipeline)
		if !hasTimestamp {
			return fmt.Errorf("gitlab: pipeline %d has no update timestamp", pipeline.ID)
		}
		projectPath := sourceProjectPath
		if projectID == targetProjectID {
			projectPath = strings.TrimSpace(targetProjectPath)
		}
		if projectPath == "" {
			return fmt.Errorf("gitlab: pipeline %d project path is unavailable", pipeline.ID)
		}
		identity := pipelineIdentity{projectID: projectID, pipelineID: pipeline.ID}
		version := gitLabCanonicalVersionedPipeline{pipeline: pipeline, projectPath: projectPath, updatedAt: updatedAt}
		if current, ok := byIdentity[identity]; !ok || updatedAt.After(current.updatedAt) {
			byIdentity[identity] = version
		}
		return nil
	}
	for _, pipeline := range mrPipelines {
		if err := add(pipeline, targetProjectID, false); err != nil {
			return 0, err
		}
	}
	for _, pipeline := range sourcePipelines {
		if err := add(pipeline, sourceProjectID, true); err != nil {
			return 0, err
		}
	}
	pipelines := make([]gitLabCanonicalVersionedPipeline, 0, len(byIdentity))
	for _, pipeline := range byIdentity {
		pipelines = append(pipelines, pipeline)
	}
	sort.Slice(pipelines, func(i, j int) bool {
		if pipelines[i].pipeline.ProjectID != pipelines[j].pipeline.ProjectID {
			return pipelines[i].pipeline.ProjectID < pipelines[j].pipeline.ProjectID
		}
		return pipelines[i].pipeline.ID < pipelines[j].pipeline.ID
	})
	persisted, err := h.persistVCSGitLabMergeRequestPipelines(ctx, pr, pipelines)
	if err != nil {
		return 0, err
	}
	if !persisted {
		return 0, nil
	}
	count := 0
	for _, version := range pipelines {
		pipeline := version.pipeline
		jobs, err := api.ListPipelineJobs(ctx, pipeline.ProjectID, pipeline.ID)
		if err != nil {
			return count, err
		}
		for _, job := range jobs {
			if job.SHA == "" {
				job.SHA = pipeline.SHA
			}
			if job.Ref == "" {
				job.Ref = pipeline.Ref
			}
			row, err := h.Queries.UpsertGitLabPipelineJobByPullRequest(ctx, gitLabPipelineJobByPullRequestParams(pr, pipeline.ID, job))
			if err != nil {
				if errors.Is(err, pgx.ErrNoRows) {
					return count, nil
				}
				return count, wrapGitLabRefreshPersistenceError(err)
			}
			count++
			if strings.EqualFold(job.Status, "failed") {
				trace, traceErr := api.GetJobTrace(ctx, pipeline.ProjectID, job.ID, gitLabFailedJobTraceLimit)
				if traceErr != nil {
					return count, fmt.Errorf("gitlab: read failed job %d trace: %w", job.ID, traceErr)
				}
				if _, err := h.Queries.UpdateGitLabPipelineJobTraceSummaryByPullRequest(ctx, db.UpdateGitLabPipelineJobTraceSummaryByPullRequestParams{
					JobID: row.ID, WorkspaceID: row.WorkspaceID, PullRequestID: pr.ID,
					WakeupSyncGeneration: pr.WakeupSyncGeneration,
					TraceSummary:         strToText(redact.Text(trace.Text)), TraceTruncated: trace.Truncated,
				}); err != nil {
					return count, wrapGitLabRefreshPersistenceError(err)
				}
			}
		}
	}
	return count, nil
}

func (h *Handler) persistVCSGitLabMergeRequestPipelines(ctx context.Context, pr db.VcsPullRequest, pipelines []gitLabCanonicalVersionedPipeline) (bool, error) {
	if h.TxStarter == nil {
		return false, wrapGitLabRefreshPersistenceError(errors.New("database transactions are unavailable"))
	}
	tx, err := h.TxStarter.Begin(ctx)
	if err != nil {
		return false, wrapGitLabRefreshPersistenceError(err)
	}
	defer tx.Rollback(ctx)
	q := h.Queries.WithTx(tx)
	if _, err := q.LockGitLabVCSPullRequestWakeupSync(ctx, db.LockGitLabVCSPullRequestWakeupSyncParams{
		ID: pr.ID, WorkspaceID: pr.WorkspaceID,
		WakeupSyncGeneration: pr.WakeupSyncGeneration, HeadSha: pr.HeadSha,
		PrUpdatedAt: pr.PrUpdatedAt,
	}); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return false, nil
		}
		return false, wrapGitLabRefreshPersistenceError(err)
	}
	if _, err := q.DeleteVCSGitLabPipelinesByMR(ctx, db.DeleteVCSGitLabPipelinesByMRParams{
		WorkspaceID: pr.WorkspaceID, PullRequestID: pr.ID,
	}); err != nil {
		return false, wrapGitLabRefreshPersistenceError(err)
	}
	for _, version := range pipelines {
		pipeline := version.pipeline
		if err := q.UpsertVCSGitLabPipeline(ctx, db.UpsertVCSGitLabPipelineParams{
			ID: pr.ID, WorkspaceID: pr.WorkspaceID, PipelineID: pipeline.ID,
			PipelineProjectID: pipeline.ProjectID, PipelineProjectPath: version.projectPath,
			Sha: pipeline.SHA, Ref: pipeline.Ref,
			Status:            normalizeGitLabPipelineStatus(pipeline.Status),
			WebUrl:            strToText(pipeline.WebURL),
			PipelineUpdatedAt: pgtype.Timestamptz{Time: version.updatedAt, Valid: true},
		}); err != nil {
			return false, wrapGitLabRefreshPersistenceError(err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return false, wrapGitLabRefreshPersistenceError(err)
	}
	return true, nil
}

func gitLabPipelineJobByPullRequestParams(pr db.VcsPullRequest, pipelineID int64, job gitlab.Job) db.UpsertGitLabPipelineJobByPullRequestParams {
	var artifactName pgtype.Text
	var artifactSize pgtype.Int8
	if job.ArtifactsFile != nil {
		artifactName = strToText(job.ArtifactsFile.Filename)
		artifactSize = pgtype.Int8{Int64: job.ArtifactsFile.Size, Valid: true}
	}
	return db.UpsertGitLabPipelineJobByPullRequestParams{
		WorkspaceID: pr.WorkspaceID, PullRequestID: pr.ID,
		WakeupSyncGeneration: pr.WakeupSyncGeneration,
		PipelineID:           pipelineID, JobID: job.ID, Name: job.Name,
		Status: job.Status, AllowFailure: job.AllowFailure,
		Stage: strToText(job.Stage), Ref: strToText(job.Ref), Sha: strToText(job.SHA),
		WebUrl: strToText(job.WebURL), StartedAt: parseGitLabTime(job.StartedAt),
		FinishedAt:      parseGitLabTime(job.FinishedAt),
		DurationSeconds: float64PtrToPg(job.Duration), QueuedDurationSeconds: float64PtrToPg(job.QueuedDuration),
		FailureReason: strToText(job.FailureReason), ArtifactsFileName: artifactName,
		ArtifactsFileSize: artifactSize, ArtifactsExpireAt: parseGitLabTime(job.ArtifactsExpireAt),
	}
}

func splitGitLabProjectPath(path string) (owner, name string, ok bool) {
	path = strings.Trim(strings.TrimSpace(path), "/")
	if path == "" || strings.ContainsAny(path, "\\?#") {
		return "", "", false
	}
	parts := strings.Split(path, "/")
	if len(parts) < 2 {
		return "", "", false
	}
	for _, part := range parts {
		if strings.TrimSpace(part) == "" {
			return "", "", false
		}
	}
	return strings.Join(parts[:len(parts)-1], "/"), parts[len(parts)-1], true
}
