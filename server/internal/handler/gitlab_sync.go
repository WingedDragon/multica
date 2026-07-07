package handler

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/integrations/gitlab"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/multica-ai/multica/server/pkg/redact"
)

const gitLabFailedJobTraceLimit = 8192

type gitLabDetailAPI interface {
	GetMergeRequest(context.Context, int64, int32) (gitlab.MergeRequest, error)
	GetMergeRequestChanges(context.Context, int64, int32) (gitlab.MergeRequestChanges, error)
	GetMergeRequestApprovalState(context.Context, int64, int32) (gitlab.ApprovalState, error)
	ListMergeRequestDiscussions(context.Context, int64, int32) ([]gitlab.Discussion, error)
	ListProjectPipelines(context.Context, int64, gitlab.PipelineFilters) ([]gitlab.Pipeline, error)
	GetPipeline(context.Context, int64, int64) (gitlab.Pipeline, error)
	ListPipelineJobs(context.Context, int64, int64) ([]gitlab.Job, error)
	GetJobTrace(context.Context, int64, int64, int) (gitlab.JobTrace, error)
	DownloadJobArtifacts(context.Context, int64, int64) (*http.Response, error)
}

type gitLabRefreshSummary struct {
	UpdatedMRs         int      `json:"updated_mrs"`
	UpdatedJobs        int      `json:"updated_jobs"`
	UpdatedDiscussions int      `json:"updated_discussions"`
	Errors             []string `json:"errors,omitempty"`
}

func (s *gitLabRefreshSummary) addError(err error) {
	if err == nil {
		return
	}
	s.Errors = append(s.Errors, redact.Text(err.Error()))
}

func (h *Handler) refreshGitLabMergeRequest(ctx context.Context, api gitLabDetailAPI, mr db.GitlabMergeRequest) (db.GitlabMergeRequest, gitLabRefreshSummary, error) {
	var summary gitLabRefreshSummary
	remoteMR, err := api.GetMergeRequest(ctx, mr.GitlabProjectID, mr.MrIid)
	if err != nil {
		h.markGitLabMergeRequestRefreshError(ctx, mr, err)
		return mr, summary, err
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

	updated, err := h.Queries.UpdateGitLabMergeRequestEnrichment(ctx, db.UpdateGitLabMergeRequestEnrichmentParams{
		ID:                  mr.ID,
		WorkspaceID:         mr.WorkspaceID,
		Title:               remoteMR.Title,
		State:               deriveGitLabMRState(remoteMR.State, remoteMR.Draft),
		WebUrl:              remoteMR.WebURL,
		Sha:                 remoteMR.SHA,
		Additions:           additions,
		Deletions:           deletions,
		ChangedFiles:        changedFiles,
		Reviewers:           jsonBytes(remoteMR.Reviewers),
		Assignees:           jsonBytes(remoteMR.Assignees),
		Labels:              jsonBytes(remoteMR.Labels),
		MrUpdatedAt:         parseGitLabTimeRequired(remoteMR.UpdatedAt),
		Description:         strToText(remoteMR.Description),
		SourceBranch:        strToText(remoteMR.SourceBranch),
		TargetBranch:        strToText(remoteMR.TargetBranch),
		AuthorUsername:      strToText(remoteMR.Author.Username),
		AuthorAvatarUrl:     strToText(remoteMR.Author.AvatarURL),
		MergeCommitSha:      strToText(remoteMR.MergeCommitSHA),
		DetailedMergeStatus: strToText(remoteMR.DetailedMergeStatus),
		HasConflicts:        pgtype.Bool{Bool: remoteMR.HasConflicts, Valid: true},
		MergedAt:            parseGitLabTime(remoteMR.MergedAt),
		ClosedAt:            parseGitLabTime(remoteMR.ClosedAt),
	})
	if err != nil {
		h.markGitLabMergeRequestRefreshError(ctx, mr, err)
		return mr, summary, err
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
	jobCount, err := h.refreshGitLabJobs(ctx, api, updated)
	if err != nil {
		summary.addError(err)
	}
	summary.UpdatedJobs = jobCount
	return updated, summary, nil
}

func (h *Handler) refreshGitLabMergeRequestFromWebhook(ctx context.Context, cfg gitlab.Config, mr db.GitlabMergeRequest) {
	api, err := newGitLabClient(cfg)
	if err != nil {
		h.markGitLabMergeRequestRefreshError(ctx, mr, err)
		return
	}
	detailAPI, err := detailGitLabAPI(api)
	if err != nil {
		h.markGitLabMergeRequestRefreshError(ctx, mr, err)
		return
	}
	_, summary, err := h.refreshGitLabMergeRequest(ctx, detailAPI, mr)
	if err != nil {
		return
	}
	if len(summary.Errors) > 0 {
		h.markGitLabMergeRequestRefreshError(ctx, mr, errors.New(strings.Join(summary.Errors, "; ")))
	}
}

func (h *Handler) markGitLabMergeRequestRefreshError(ctx context.Context, mr db.GitlabMergeRequest, err error) {
	if err == nil {
		return
	}
	_ = h.Queries.MarkGitLabMergeRequestRefreshError(ctx, db.MarkGitLabMergeRequestRefreshErrorParams{
		ID:               mr.ID,
		WorkspaceID:      mr.WorkspaceID,
		LastRefreshError: strToText(redact.Text(err.Error())),
	})
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
	return err
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
			return 0, err
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
				return 0, err
			}
		}
	}
	return len(discussions), nil
}

func (h *Handler) refreshGitLabJobs(ctx context.Context, api gitLabDetailAPI, mr db.GitlabMergeRequest) (int, error) {
	pipelines, err := api.ListProjectPipelines(ctx, mr.GitlabProjectID, gitlab.PipelineFilters{SHA: mr.Sha})
	if err != nil {
		return 0, err
	}
	count := 0
	for _, pipeline := range pipelines {
		if err := h.Queries.UpsertGitLabMRPipeline(ctx, db.UpsertGitLabMRPipelineParams{
			MergeRequestID:    mr.ID,
			PipelineID:        pipeline.ID,
			Sha:               pipeline.SHA,
			Status:            normalizeGitLabPipelineStatus(pipeline.Status),
			PipelineUpdatedAt: parseGitLabTimeRequired(pipeline.UpdatedAt),
			Ref:               strToText(pipeline.Ref),
			WebUrl:            strToText(pipeline.WebURL),
		}); err != nil {
			return count, err
		}
		jobs, err := api.ListPipelineJobs(ctx, mr.GitlabProjectID, pipeline.ID)
		if err != nil {
			return count, err
		}
		for _, job := range jobs {
			row, err := h.Queries.UpsertGitLabPipelineJob(ctx, gitLabPipelineJobParams(mr, pipeline.ID, job))
			if err != nil {
				return count, err
			}
			count++
			if strings.EqualFold(job.Status, "failed") {
				trace, err := api.GetJobTrace(ctx, mr.GitlabProjectID, job.ID, gitLabFailedJobTraceLimit)
				if err != nil {
					continue
				}
				if err := h.Queries.UpdateGitLabPipelineJobTraceSummary(ctx, db.UpdateGitLabPipelineJobTraceSummaryParams{
					ID:             row.ID,
					WorkspaceID:    row.WorkspaceID,
					TraceSummary:   strToText(redact.Text(trace.Text)),
					TraceTruncated: trace.Truncated,
				}); err != nil {
					return count, err
				}
			}
		}
	}
	return count, nil
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
