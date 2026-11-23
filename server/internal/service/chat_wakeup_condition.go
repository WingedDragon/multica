package service

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/integrations/vcs"
	"github.com/multica-ai/multica/server/internal/issuestatus"
	"github.com/multica-ai/multica/server/internal/projectaccess"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// ChatWakeupCondition is a fact the platform can look up on its own. A chat
// wakeup is not anchored to an issue, so every condition names what it
// watches. Fields marked "display" are filled in by the server.
type ChatWakeupCondition struct {
	// issue | children_done | pull_request | run | issue_reply
	Type string `json:"type"`
	// issue, children_done, issue_reply
	IssueID    string `json:"issue_id,omitempty"`
	Identifier string `json:"identifier,omitempty"` // display
	// issue: done | ended | in_review | blocked, or Status for one status key.
	State  string `json:"state,omitempty"`
	Status string `json:"status,omitempty"`
	// children_done: this stage and earlier ones; every sub-issue when omitted.
	Stage *int32 `json:"stage,omitempty"`
	// pull_request: owner/name for GitHub; full group/project path for GitLab.
	Repo   string `json:"repo,omitempty"`
	Number int32  `json:"number,omitempty"`
	Event  string `json:"event,omitempty"`
	// pull_request: omitted provider means the original GitHub condition.
	Provider     string `json:"provider,omitempty"`
	ConnectionID string `json:"connection_id,omitempty"`
	// GitLab is server-owned after normalization: it binds to a stable mirror ID
	// and, for CI, stores the baseline captured when the rule became active.
	GitLab *chatGitLabTargetState `json:"gitlab,omitempty"`
	// run: one agent run reaching a terminal state.
	TaskID string `json:"task_id,omitempty"`
	// issue_reply: a comment by AuthorID (any member when empty) after Since.
	AuthorID   string `json:"author_id,omitempty"`
	AuthorName string `json:"author_name,omitempty"` // display
	Since      string `json:"since,omitempty"`       // display
}

type chatGitLabTargetState struct {
	Source            vcs.GitLabWakeupSource      `json:"source"`
	MergeRequestID    string                      `json:"merge_request_id"`
	ActivatedAt       string                      `json:"activated_at,omitempty"`
	BaselineAvailable bool                        `json:"baseline_available"`
	BaselineHeadSHA   string                      `json:"baseline_head_sha,omitempty"`
	BaselinePipelines []chatGitLabPipelineVersion `json:"baseline_pipelines,omitempty"`
}

type chatGitLabPipelineVersion struct {
	PipelineID          string `json:"pipeline_id"`
	PipelineProjectID   string `json:"pipeline_project_id"`
	PipelineProjectPath string `json:"pipeline_project_path"`
	Ref                 string `json:"ref"`
	SHA                 string `json:"sha"`
	Status              string `json:"status"`
	UpdatedAt           string `json:"updated_at"`
}

type chatGitLabPreparation struct {
	target    vcs.GitLabWakeupTarget
	syncError error
}

func badChatCondition(msg string) error {
	return fmt.Errorf("%w: %s", ErrChatWakeupInput, msg)
}

// errChatConditionPending explains a wait the platform cannot resolve yet; the
// rule keeps polling and shows the reason.
type errChatConditionPending struct{ msg string }

func (e errChatConditionPending) Error() string { return e.msg }

// errChatConditionHidden means the condition reads an issue or run in a
// private project the wakeup's requester or the agent's owner cannot see.
var errChatConditionHidden = errors.New("chat wakeup condition is in a private project")

// normalizeChatPRIdentity preserves the original GitHub shape (provider omitted)
// while validating the explicit, connection-scoped GitLab shape.
func normalizeChatPRIdentity(c *ChatWakeupCondition) error {
	if c.Type != "pull_request" {
		if c.Provider != "" || c.ConnectionID != "" {
			return badChatCondition("provider and connection_id are only valid for pull_request conditions")
		}
		return nil
	}
	if c.Number <= 0 || int64(c.Number) > 2147483647 {
		return badChatCondition("pull request number must be a positive 32-bit integer")
	}
	c.Repo = strings.TrimSpace(c.Repo)
	if strings.ContainsAny(c.Repo, " \t\r\n") {
		return badChatCondition("repo must not contain whitespace")
	}
	switch c.Provider {
	case "":
		if c.ConnectionID != "" {
			return badChatCondition("connection_id requires provider=gitlab")
		}
		owner, repo, ok := strings.Cut(c.Repo, "/")
		if !ok || owner == "" || repo == "" || strings.Contains(repo, "/") {
			return badChatCondition("repo must be owner/name")
		}
	case "gitlab":
		connectionID, err := util.ParseUUID(c.ConnectionID)
		if err != nil {
			return badChatCondition("GitLab pull_request conditions require a valid connection_id")
		}
		c.ConnectionID = util.UUIDToString(connectionID)
		parts := strings.Split(c.Repo, "/")
		if len(parts) < 2 {
			return badChatCondition("GitLab repo must be a full project path including its group")
		}
		for _, part := range parts {
			if part == "" || part == "." || part == ".." {
				return badChatCondition("GitLab repo must be a full project path including its group")
			}
		}
	default:
		return badChatCondition("provider must be gitlab; omit provider for GitHub")
	}
	if c.Event != "merged" && c.Event != "closed" && c.Event != "checks_finished" {
		return badChatCondition("pull_request event must be merged, closed, or checks_finished")
	}
	return nil
}

func decodeChatWakeupCondition(raw json.RawMessage) (ChatWakeupCondition, error) {
	var condition ChatWakeupCondition
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&condition); err != nil {
		return ChatWakeupCondition{}, badChatCondition(err.Error())
	}
	if err := decoder.Decode(new(any)); !errors.Is(err, io.EOF) {
		if err == nil {
			return ChatWakeupCondition{}, badChatCondition("condition must contain exactly one JSON object")
		}
		return ChatWakeupCondition{}, badChatCondition(err.Error())
	}
	return condition, nil
}

func (s *ChatWakeupService) prepareGitLabChatCondition(ctx context.Context, workspaceID pgtype.UUID, raw json.RawMessage) (*chatGitLabPreparation, error) {
	condition, err := decodeChatWakeupCondition(raw)
	if err != nil {
		return nil, err
	}
	if condition.Provider != "gitlab" {
		return nil, nil
	}
	if err := normalizeChatPRIdentity(&condition); err != nil {
		return nil, err
	}

	var target vcs.GitLabWakeupTarget
	if err := s.inTx(ctx, func(tx pgx.Tx, _ *db.Queries) error {
		var err error
		target, err = resolveGitLabChatTarget(ctx, tx, workspaceID, condition)
		return err
	}); err != nil {
		return nil, err
	}
	preparation := &chatGitLabPreparation{target: target}
	if s.Tasks == nil || s.Tasks.GitLabWakeupSyncer == nil {
		preparation.syncError = errChatConditionPending{msg: "GitLab synchronization is unavailable; this condition is waiting for the configured integration to sync state"}
		return preparation, nil
	}
	if err := s.Tasks.GitLabWakeupSyncer.SyncGitLabWakeupTarget(ctx, target); err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return nil, err
		}
		preparation.syncError = errChatConditionPending{msg: fmt.Sprintf(
			"GitLab status sync for %s!%d on connection %s is unavailable: %v",
			target.Repository, target.Number, target.ConnectionID, err,
		)}
	}
	return preparation, nil
}

func resolveGitLabChatTarget(ctx context.Context, tx pgx.Tx, workspaceID pgtype.UUID, condition ChatWakeupCondition) (vcs.GitLabWakeupTarget, error) {
	connectionID, err := util.ParseUUID(condition.ConnectionID)
	if err != nil {
		return vcs.GitLabWakeupTarget{}, badChatCondition("GitLab pull_request conditions require a valid connection_id")
	}
	var hasVCSConnection, hasWorkspaceConnection bool
	if err := tx.QueryRow(ctx, `
		SELECT EXISTS (
			SELECT 1 FROM vcs_connection
			WHERE workspace_id=$1 AND id=$2 AND provider='gitlab'
		)`,
		workspaceID, connectionID,
	).Scan(&hasVCSConnection); err != nil {
		return vcs.GitLabWakeupTarget{}, err
	}
	if err := tx.QueryRow(ctx, `
		SELECT EXISTS (
			SELECT 1 FROM gitlab_connection
			WHERE workspace_id=$1 AND id=$2
		)`,
		workspaceID, connectionID,
	).Scan(&hasWorkspaceConnection); err != nil {
		return vcs.GitLabWakeupTarget{}, err
	}
	if hasVCSConnection && hasWorkspaceConnection {
		return vcs.GitLabWakeupTarget{}, badChatCondition("GitLab connection_id is ambiguous in this workspace; choose one connection source")
	}
	if !hasVCSConnection && !hasWorkspaceConnection {
		return vcs.GitLabWakeupTarget{}, badChatCondition("GitLab connection_id was not found in this workspace; run 'multica chat wakeup connections' to list available IDs")
	}

	var source vcs.GitLabWakeupSource
	var mergeRequestID, repository string
	switch {
	case hasVCSConnection:
		err = tx.QueryRow(ctx, `
			SELECT id::text, repo_owner || '/' || repo_name FROM vcs_pull_request
			WHERE workspace_id=$1 AND connection_id=$2 AND provider='gitlab'
			  AND lower(repo_owner || '/' || repo_name)=lower($3) AND pr_number=$4
		`,
			workspaceID, connectionID, condition.Repo, int32(condition.Number),
		).Scan(&mergeRequestID, &repository)
		source = vcs.GitLabWakeupSourceVCSConnection
	case hasWorkspaceConnection:
		err = tx.QueryRow(ctx, `
			SELECT mr.id::text, pb.path_with_namespace FROM gitlab_merge_request mr
			JOIN gitlab_project_binding pb
			  ON pb.workspace_id=mr.workspace_id AND pb.id=mr.project_binding_id
			JOIN gitlab_connection gc
			  ON gc.workspace_id=pb.workspace_id AND gc.id=pb.connection_id
			WHERE mr.workspace_id=$1 AND pb.connection_id=$2
			  AND lower(pb.path_with_namespace)=lower($3) AND mr.mr_iid=$4
		`,
			workspaceID, connectionID, condition.Repo, int32(condition.Number),
		).Scan(&mergeRequestID, &repository)
		source = vcs.GitLabWakeupSourceWorkspaceConnection
	}
	if errors.Is(err, pgx.ErrNoRows) {
		return vcs.GitLabWakeupTarget{}, badChatCondition(fmt.Sprintf(
			"GitLab merge request %s!%d is not mirrored for connection %s; sync the project or choose an available target",
			condition.Repo, condition.Number, condition.ConnectionID,
		))
	}
	if err != nil {
		return vcs.GitLabWakeupTarget{}, err
	}
	return vcs.GitLabWakeupTarget{
		Source: source, WorkspaceID: util.UUIDToString(workspaceID),
		ConnectionID: condition.ConnectionID, MergeRequestID: mergeRequestID,
		Repository: repository, Number: int32(condition.Number),
	}, nil
}

func normalizeGitLabPipelineStatus(status string) string {
	switch strings.ToLower(strings.TrimSpace(status)) {
	case "success", "skipped", "passed":
		return "success"
	case "failed", "failure", "canceled", "cancelled":
		return "failure"
	default:
		return ""
	}
}

func chatGitLabPipelineAfterBaseline(current chatGitLabPipelineVersion, currentHead string, baselineAvailable bool, baseline []chatGitLabPipelineVersion, activatedAt time.Time) bool {
	status := normalizeGitLabPipelineStatus(current.Status)
	if status == "" || current.SHA != currentHead {
		return false
	}
	current.Status = status
	currentAt, err := time.Parse(time.RFC3339Nano, current.UpdatedAt)
	if err != nil || !currentAt.After(activatedAt) {
		return false
	}
	if baselineAvailable {
		for _, old := range baseline {
			old.Status = normalizeGitLabPipelineStatus(old.Status)
			if current == old {
				return false
			}
		}
	}
	return true
}

func chatGitLabTargetLabel(condition ChatWakeupCondition) string {
	return fmt.Sprintf("%s!%d", condition.Repo, condition.Number)
}
func sameGitLabWakeupTarget(a, b vcs.GitLabWakeupTarget) bool {
	return a.Source == b.Source &&
		a.WorkspaceID == b.WorkspaceID &&
		a.ConnectionID == b.ConnectionID &&
		a.MergeRequestID == b.MergeRequestID &&
		strings.EqualFold(a.Repository, b.Repository) &&
		a.Number == b.Number
}

type chatGitLabWakeupSnapshot struct {
	State             string
	HeadSHA           string
	Title             string
	URL               string
	TerminalPipelines []chatGitLabPipelineVersion
}

func loadChatGitLabWakeupSnapshot(ctx context.Context, tx pgx.Tx, target vcs.GitLabWakeupTarget, includePipelines bool) (chatGitLabWakeupSnapshot, error) {
	var snapshot chatGitLabWakeupSnapshot
	var refreshedAt, pipelineRefreshedAt pgtype.Timestamptz
	var refreshError, pipelineRefreshError pgtype.Text
	var err error
	switch target.Source {
	case vcs.GitLabWakeupSourceVCSConnection:
		err = tx.QueryRow(ctx, `
			SELECT pr.state, pr.head_sha, pr.title, pr.html_url,
			       pr.status_sync_at, pr.status_sync_error,
			       pr.pipeline_sync_at, pr.pipeline_sync_error
			FROM vcs_pull_request pr
			WHERE pr.workspace_id=$1 AND pr.connection_id=$2 AND pr.id=$3
			  AND pr.provider='gitlab'
			  AND lower(pr.repo_owner || '/' || pr.repo_name)=lower($4) AND pr.pr_number=$5
		`,
			target.WorkspaceID, target.ConnectionID, target.MergeRequestID, target.Repository, target.Number,
		).Scan(&snapshot.State, &snapshot.HeadSHA, &snapshot.Title, &snapshot.URL,
			&refreshedAt, &refreshError, &pipelineRefreshedAt, &pipelineRefreshError)
	case vcs.GitLabWakeupSourceWorkspaceConnection:
		err = tx.QueryRow(ctx, `
			SELECT mr.state, mr.sha, mr.title, mr.web_url,
			       mr.last_refreshed_at, mr.last_refresh_error,
			       mr.pipeline_sync_at, mr.pipeline_sync_error
			FROM gitlab_merge_request mr
			JOIN gitlab_project_binding pb
			  ON pb.workspace_id=mr.workspace_id AND pb.id=mr.project_binding_id
			WHERE mr.workspace_id=$1 AND mr.id=$2 AND pb.connection_id=$3
			  AND lower(pb.path_with_namespace)=lower($4) AND mr.mr_iid=$5
		`,
			target.WorkspaceID, target.MergeRequestID, target.ConnectionID, target.Repository, target.Number,
		).Scan(&snapshot.State, &snapshot.HeadSHA, &snapshot.Title, &snapshot.URL,
			&refreshedAt, &refreshError, &pipelineRefreshedAt, &pipelineRefreshError)
	default:
		return snapshot, fmt.Errorf("unsupported GitLab wakeup source %q", target.Source)
	}
	if errors.Is(err, pgx.ErrNoRows) {
		return snapshot, errChatConditionPending{msg: "The GitLab merge request is no longer mirrored in this workspace; check the GitLab connection and project binding"}
	}
	if err != nil {
		return snapshot, err
	}
	if refreshError.Valid && strings.TrimSpace(refreshError.String) != "" {
		return snapshot, errChatConditionPending{msg: fmt.Sprintf("GitLab merge-request status is unavailable: %s", strings.TrimSpace(refreshError.String))}
	}
	if !refreshedAt.Valid {
		return snapshot, errChatConditionPending{msg: "Waiting for a successful GitLab merge-request status refresh"}
	}
	snapshot.State = strings.ToLower(strings.TrimSpace(snapshot.State))
	if snapshot.State == "" {
		return snapshot, errChatConditionPending{msg: "GitLab merge-request state is unavailable"}
	}
	if includePipelines {
		if strings.TrimSpace(snapshot.HeadSHA) == "" {
			return snapshot, errChatConditionPending{msg: "GitLab merge-request head SHA is unavailable"}
		}
		if pipelineRefreshError.Valid && strings.TrimSpace(pipelineRefreshError.String) != "" {
			return snapshot, errChatConditionPending{msg: fmt.Sprintf("GitLab CI status is unavailable: %s", strings.TrimSpace(pipelineRefreshError.String))}
		}
		if !pipelineRefreshedAt.Valid {
			return snapshot, errChatConditionPending{msg: "Waiting for a successful GitLab CI status refresh"}
		}
		pipelines, err := loadChatGitLabWakeupPipelines(ctx, tx, target, snapshot.HeadSHA)
		if err != nil {
			return snapshot, err
		}
		snapshot.TerminalPipelines = pipelines
	}
	return snapshot, nil
}

func chatGitLabMRPipelineRef(ref string, number int32) bool {
	prefix := fmt.Sprintf("refs/merge-requests/%d/", number)
	return ref == prefix+"head" || ref == prefix+"merge"
}

func chatGitLabPipelineMatchesMR(
	projectID int64, projectPath, ref string, target vcs.GitLabWakeupTarget,
	targetProjectID int64, targetProjectPath string, sourceProjectID pgtype.Int8, sourceProjectPath, sourceBranch string,
) (matches, unknown bool) {
	if projectID <= 0 || strings.TrimSpace(projectPath) == "" {
		return false, true
	}
	targetPathMatches := strings.EqualFold(projectPath, targetProjectPath)
	if targetProjectID > 0 {
		targetIDMatches := projectID == targetProjectID
		if targetIDMatches != targetPathMatches {
			return false, true
		}
		if targetIDMatches && chatGitLabMRPipelineRef(ref, target.Number) {
			return true, false
		}
	} else if targetPathMatches && chatGitLabMRPipelineRef(ref, target.Number) {
		return true, false
	}
	if sourceProjectID.Valid && projectID == sourceProjectID.Int64 {
		if strings.TrimSpace(sourceProjectPath) == "" || !strings.EqualFold(projectPath, sourceProjectPath) || sourceBranch == "" {
			return false, true
		}
		return ref == sourceBranch, false
	}
	if targetProjectID > 0 && projectID == targetProjectID {
		return false, false
	}
	if targetProjectID == 0 && targetPathMatches {
		return false, false
	}
	return false, true
}

func loadChatGitLabWakeupPipelines(ctx context.Context, tx pgx.Tx, target vcs.GitLabWakeupTarget, currentHead string) ([]chatGitLabPipelineVersion, error) {
	var rows pgx.Rows
	var err error
	switch target.Source {
	case vcs.GitLabWakeupSourceVCSConnection:
		rows, err = tx.Query(ctx, `
			SELECT p.pipeline_id, p.pipeline_project_id, COALESCE(p.pipeline_project_path, ''),
			       COALESCE(p.ref, ''), p.sha, p.status, p.pipeline_updated_at,
			       pr.source_project_id, COALESCE(pr.source_project_path, ''), COALESCE(pr.branch, ''),
			       0::bigint, pr.repo_owner || '/' || pr.repo_name
			FROM vcs_gitlab_pipeline p
			JOIN vcs_pull_request pr ON pr.id=p.pull_request_id
			WHERE p.workspace_id=$1 AND p.connection_id=$2 AND p.pull_request_id=$3
			  AND pr.workspace_id=$1 AND pr.connection_id=$2 AND pr.provider='gitlab'
			  AND lower(pr.repo_owner || '/' || pr.repo_name)=lower($4) AND pr.pr_number=$5
			ORDER BY p.pipeline_updated_at, p.pipeline_project_id, p.pipeline_id
		`, target.WorkspaceID, target.ConnectionID, target.MergeRequestID, target.Repository, target.Number)
	case vcs.GitLabWakeupSourceWorkspaceConnection:
		rows, err = tx.Query(ctx, `
			SELECT p.pipeline_id, p.pipeline_project_id,
			       CASE
			         WHEN p.pipeline_project_id=mr.gitlab_project_id THEN pb.path_with_namespace
			         WHEN p.pipeline_project_id=mr.source_project_id THEN COALESCE(mr.source_project_path, '')
			         ELSE ''
			       END,
			       COALESCE(p.ref, ''), p.sha, p.status, p.pipeline_updated_at,
			       mr.source_project_id, COALESCE(mr.source_project_path, ''), COALESCE(mr.source_branch, ''),
			       mr.gitlab_project_id, pb.path_with_namespace
			FROM gitlab_mr_pipeline p
			JOIN gitlab_merge_request mr ON mr.id=p.merge_request_id
			JOIN gitlab_project_binding pb
			  ON pb.workspace_id=mr.workspace_id AND pb.id=mr.project_binding_id
			WHERE p.merge_request_id=$1 AND mr.workspace_id=$2 AND pb.connection_id=$3
			  AND lower(pb.path_with_namespace)=lower($4) AND mr.mr_iid=$5
			ORDER BY p.pipeline_updated_at, p.pipeline_project_id, p.pipeline_id
		`, target.MergeRequestID, target.WorkspaceID, target.ConnectionID, target.Repository, target.Number)
	default:
		return nil, fmt.Errorf("unsupported GitLab wakeup source %q", target.Source)
	}
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var terminal []chatGitLabPipelineVersion
	for rows.Next() {
		var pipeline chatGitLabPipelineVersion
		var pipelineID, pipelineProjectID, targetProjectID int64
		var updatedAt pgtype.Timestamptz
		var sourceProjectID pgtype.Int8
		var sourceProjectPath, sourceBranch, targetProjectPath string
		if err := rows.Scan(
			&pipelineID, &pipelineProjectID, &pipeline.PipelineProjectPath, &pipeline.Ref,
			&pipeline.SHA, &pipeline.Status, &updatedAt,
			&sourceProjectID, &sourceProjectPath, &sourceBranch,
			&targetProjectID, &targetProjectPath,
		); err != nil {
			return nil, err
		}
		if pipeline.SHA != currentHead {
			continue
		}
		pipeline.Status = normalizeGitLabPipelineStatus(pipeline.Status)
		if pipeline.Status == "" {
			continue
		}
		matches, unknown := chatGitLabPipelineMatchesMR(
			pipelineProjectID, pipeline.PipelineProjectPath, pipeline.Ref, target,
			targetProjectID, targetProjectPath, sourceProjectID, sourceProjectPath, sourceBranch,
		)
		if unknown {
			return nil, errChatConditionPending{msg: fmt.Sprintf(
				"GitLab MR %s!%d has a current-head pipeline with unknown project identity",
				target.Repository, target.Number,
			)}
		}
		if !matches {
			continue
		}
		if pipelineID <= 0 || !updatedAt.Valid {
			return nil, errChatConditionPending{msg: "A current-head GitLab pipeline is missing stable project or update identity"}
		}
		pipeline.PipelineID = fmt.Sprint(pipelineID)
		pipeline.PipelineProjectID = fmt.Sprint(pipelineProjectID)
		pipeline.UpdatedAt = updatedAt.Time.UTC().Format(time.RFC3339Nano)
		terminal = append(terminal, pipeline)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return terminal, nil
}

// chatConditionProjects lists the projects whose content a condition reads.
// children_done reports only counts of sub-issues, so the parent's project is
// the one that matters there.
func chatConditionProjects(ctx context.Context, q *db.Queries, workspaceID pgtype.UUID, c ChatWakeupCondition) ([]pgtype.UUID, error) {
	switch c.Type {
	case "issue", "children_done", "issue_reply":
		id, err := util.ParseUUID(c.IssueID)
		if err != nil {
			return nil, err
		}
		issue, err := q.GetIssue(ctx, id)
		if errors.Is(err, pgx.ErrNoRows) || (err == nil && issue.WorkspaceID != workspaceID) {
			return nil, nil
		}
		if err != nil {
			return nil, err
		}
		return []pgtype.UUID{issue.ProjectID}, nil
	case "run":
		id, err := util.ParseUUID(c.TaskID)
		if err != nil {
			return nil, err
		}
		task, err := q.GetAgentTask(ctx, id)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, nil
		}
		if err != nil {
			return nil, err
		}
		return projectaccess.TaskProjects(ctx, q, task)
	}
	return nil, nil
}

// checkChatConditionAccess applies the private-project rule every run obeys:
// the requester and the agent's owner must both see what the condition reads.
func checkChatConditionAccess(ctx context.Context, q *db.Queries, workspaceID, requesterID, agentOwnerID pgtype.UUID, raw []byte) error {
	var c ChatWakeupCondition
	if err := json.Unmarshal(raw, &c); err != nil {
		return err
	}
	projects, err := chatConditionProjects(ctx, q, workspaceID, c)
	if err != nil {
		return err
	}
	if !slices.ContainsFunc(projects, func(p pgtype.UUID) bool { return p.Valid }) {
		return nil
	}
	hidden, err := projectaccess.ForPrincipals(ctx, q, workspaceID, requesterID, agentOwnerID)
	if err != nil {
		return fmt.Errorf("resolve project access: %w", err)
	}
	for _, p := range projects {
		if p.Valid && !hidden.CanSeeProject(p) {
			return errChatConditionHidden
		}
	}
	return nil
}

// validateChatCondition normalizes a condition against the session's
// workspace. It rejects anything the platform could never evaluate, so a typo
// fails at creation instead of waiting out the whole deadline, and anything in
// a private project the requester or the agent's owner cannot see.
func (s *ChatWakeupService) validateChatCondition(ctx context.Context, tx pgx.Tx, q *db.Queries, workspaceID, requesterID, agentOwnerID pgtype.UUID, raw json.RawMessage, preparation *chatGitLabPreparation, now time.Time) (json.RawMessage, error) {
	normalized, err := s.normalizeChatCondition(ctx, tx, workspaceID, raw, preparation, now)
	if err != nil {
		return nil, err
	}
	if err := checkChatConditionAccess(ctx, q, workspaceID, requesterID, agentOwnerID, normalized); err != nil {
		if errors.Is(err, errChatConditionHidden) {
			return nil, badChatCondition("this is in a private project that the person who asked, or this agent's owner, cannot see")
		}
		return nil, err
	}
	return normalized, nil
}

func (s *ChatWakeupService) normalizeChatCondition(ctx context.Context, tx pgx.Tx, workspaceID pgtype.UUID, raw json.RawMessage, preparation *chatGitLabPreparation, now time.Time) (json.RawMessage, error) {
	c, err := decodeChatWakeupCondition(raw)
	if err != nil {
		return nil, err
	}
	if err := normalizeChatPRIdentity(&c); err != nil {
		return nil, err
	}
	issueRef := func() (pgtype.UUID, string, error) {
		id, err := util.ParseUUID(c.IssueID)
		if err != nil {
			return id, "", badChatCondition("issue_id must be a UUID")
		}
		var identifier string
		err = tx.QueryRow(ctx, "SELECT ws.issue_prefix||'-'||i.number FROM issue i JOIN workspace ws ON ws.id=i.workspace_id WHERE i.workspace_id=$1 AND i.id=$2", workspaceID, id).Scan(&identifier)
		if errors.Is(err, pgx.ErrNoRows) {
			return id, "", badChatCondition("unknown issue")
		}
		return id, identifier, err
	}
	var out ChatWakeupCondition
	switch c.Type {
	case "issue":
		id, identifier, err := issueRef()
		if err != nil {
			return nil, err
		}
		out = ChatWakeupCondition{Type: c.Type, IssueID: util.UUIDToString(id), Identifier: identifier}
		switch {
		case c.Status != "" && c.State != "":
			return nil, badChatCondition("use state or status, not both")
		case c.Status != "":
			if !issuestatus.IsBuiltIn(c.Status) {
				var ok bool
				if err := tx.QueryRow(ctx, "SELECT EXISTS(SELECT 1 FROM issue_status WHERE workspace_id=$1 AND key=$2)", workspaceID, c.Status).Scan(&ok); err != nil {
					return nil, err
				}
				if !ok {
					return nil, badChatCondition("unknown status")
				}
			}
			out.Status = c.Status
		case slices.Contains([]string{"done", "ended", "in_review", "blocked"}, c.State):
			out.State = c.State
		default:
			return nil, badChatCondition("state must be done, ended, in_review or blocked, or pass a status key")
		}
	case "children_done":
		id, identifier, err := issueRef()
		if err != nil {
			return nil, err
		}
		if c.Stage != nil && (*c.Stage < 1 || *c.Stage > 1000) {
			return nil, badChatCondition("stage must be a positive number")
		}
		out = ChatWakeupCondition{Type: c.Type, IssueID: util.UUIDToString(id), Identifier: identifier, Stage: c.Stage}
	case "pull_request":
		if c.Provider == "gitlab" {
			if preparation == nil {
				return nil, badChatCondition("GitLab target could not be prepared; retry this wakeup")
			}
			target, err := resolveGitLabChatTarget(ctx, tx, workspaceID, c)
			if err != nil {
				return nil, err
			}
			if !sameGitLabWakeupTarget(target, preparation.target) {
				return nil, badChatCondition("GitLab target changed while syncing; retry this wakeup")
			}
			state := &chatGitLabTargetState{
				Source: target.Source, MergeRequestID: target.MergeRequestID,
				ActivatedAt: s.clock().UTC().Format(time.RFC3339Nano),
			}
			if c.Event == "checks_finished" && preparation.syncError == nil {
				snapshot, snapshotErr := loadChatGitLabWakeupSnapshot(ctx, tx, target, true)
				if snapshotErr != nil {
					if errors.Is(snapshotErr, context.Canceled) || errors.Is(snapshotErr, context.DeadlineExceeded) {
						return nil, snapshotErr
					}
					preparation.syncError = errChatConditionPending{msg: fmt.Sprintf(
						"GitLab checks for %s on connection %s are unavailable: %v",
						chatGitLabTargetLabel(c), c.ConnectionID, snapshotErr,
					)}
				} else {
					state.BaselineAvailable = true
					state.BaselineHeadSHA = snapshot.HeadSHA
					state.BaselinePipelines = snapshot.TerminalPipelines
				}
			}
			out = ChatWakeupCondition{
				Type: c.Type, Provider: "gitlab", ConnectionID: c.ConnectionID,
				Repo: target.Repository, Number: c.Number, Event: c.Event, GitLab: state,
			}
		} else {
			owner, name, _ := strings.Cut(c.Repo, "/")
			if c.Event == "checks_finished" && (s.Tasks == nil || !s.Tasks.PRSnapshotsEnabled) {
				return nil, badChatCondition("PR checks cannot be watched on this server: GitHub check snapshots are not configured")
			}
			var exists bool
			if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM github_pull_request WHERE workspace_id=$1 AND lower(repo_owner)=lower($2) AND lower(repo_name)=lower($3) AND pr_number=$4)`,
				workspaceID, owner, name, c.Number).Scan(&exists); err != nil {
				return nil, err
			}
			if !exists {
				return nil, badChatCondition(fmt.Sprintf("this workspace has no pull request %s/%s#%d; it must be on a repository connected through the GitHub integration", owner, name, c.Number))
			}
			out = ChatWakeupCondition{Type: c.Type, Repo: owner + "/" + name, Number: c.Number, Event: c.Event}
		}
	case "run":
		id, err := util.ParseUUID(c.TaskID)
		if err != nil {
			return nil, badChatCondition("task_id must be a UUID")
		}
		var exists bool
		if err := tx.QueryRow(ctx, "SELECT EXISTS(SELECT 1 FROM agent_task_queue t JOIN agent a ON a.id=t.agent_id WHERE t.id=$1 AND a.workspace_id=$2)", id, workspaceID).Scan(&exists); err != nil {
			return nil, err
		}
		if !exists {
			return nil, badChatCondition("unknown run")
		}
		out = ChatWakeupCondition{Type: c.Type, TaskID: util.UUIDToString(id)}
	case "issue_reply":
		id, identifier, err := issueRef()
		if err != nil {
			return nil, err
		}
		out = ChatWakeupCondition{Type: c.Type, IssueID: util.UUIDToString(id), Identifier: identifier, Since: now.UTC().Format(time.RFC3339Nano)}
		if c.AuthorID != "" {
			author, err := util.ParseUUID(c.AuthorID)
			if err != nil {
				return nil, badChatCondition("author_id must be a member's user id")
			}
			var name string
			err = tx.QueryRow(ctx, `SELECT u.name FROM member m JOIN "user" u ON u.id=m.user_id WHERE m.workspace_id=$1 AND m.user_id=$2`, workspaceID, author).Scan(&name)
			if errors.Is(err, pgx.ErrNoRows) {
				return nil, badChatCondition("author_id is not a member of this workspace")
			}
			if err != nil {
				return nil, err
			}
			out.AuthorID, out.AuthorName = util.UUIDToString(author), name
		}
	default:
		return nil, badChatCondition("type must be issue, children_done, pull_request, run or issue_reply")
	}
	normalized, _ := json.Marshal(out)
	return normalized, nil
}

// chatConditionFiresOnChange reports whether facts already true at creation
// are ignored. Finished checks usually belong to the head the agent is about
// to replace, so a checks condition waits for a newer result.
func chatConditionFiresOnChange(raw []byte) bool {
	var c ChatWakeupCondition
	_ = json.Unmarshal(raw, &c)
	return c.Type == "pull_request" && c.Event == "checks_finished"
}

// evaluateChatCondition reports whether the condition holds, a fingerprint of
// the satisfying facts and what was observed, for the woken agent.
func evaluateGitLabChatCondition(ctx context.Context, tx pgx.Tx, w db.ChatWakeup, c ChatWakeupCondition) (bool, string, map[string]any, error) {
	if c.GitLab == nil || c.ConnectionID == "" || c.GitLab.MergeRequestID == "" {
		return false, "", nil, errChatConditionPending{msg: "GitLab wakeup identity is incomplete; update the wakeup to select a connection and merge request"}
	}
	target := vcs.GitLabWakeupTarget{
		Source: c.GitLab.Source, WorkspaceID: util.UUIDToString(w.WorkspaceID),
		ConnectionID: c.ConnectionID, MergeRequestID: c.GitLab.MergeRequestID,
		Repository: c.Repo, Number: c.Number,
	}
	includePipelines := c.Event == "checks_finished"
	snapshot, err := loadChatGitLabWakeupSnapshot(ctx, tx, target, includePipelines)
	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return false, "", nil, err
		}
		var pending errChatConditionPending
		if errors.As(err, &pending) {
			return false, "", nil, err
		}
		return false, "", nil, errChatConditionPending{msg: fmt.Sprintf(
			"GitLab status for %s on connection %s is unavailable: %v",
			chatGitLabTargetLabel(c), c.ConnectionID, err,
		)}
	}
	observed := map[string]any{
		"provider": "gitlab", "connection_id": c.ConnectionID,
		"repository": c.Repo, "number": c.Number,
		"pull_request": chatGitLabTargetLabel(c), "title": snapshot.Title,
		"url": snapshot.URL, "state": snapshot.State, "head_sha": snapshot.HeadSHA,
	}
	switch c.Event {
	case "merged":
		fingerprint := strings.Join([]string{
			"gitlab", string(target.Source), target.ConnectionID, target.MergeRequestID, "state", snapshot.State,
		}, ":")
		return snapshot.State == "merged", fingerprint, observed, nil
	case "closed":
		fingerprint := strings.Join([]string{
			"gitlab", string(target.Source), target.ConnectionID, target.MergeRequestID, "state", snapshot.State,
		}, ":")
		return snapshot.State == "merged" || snapshot.State == "closed", fingerprint, observed, nil
	case "checks_finished":
		observed["checks_baseline_available"] = c.GitLab.BaselineAvailable
		observed["checks_baseline_head_sha"] = c.GitLab.BaselineHeadSHA
		activatedAt, err := time.Parse(time.RFC3339Nano, c.GitLab.ActivatedAt)
		if err != nil {
			return false, "", nil, errChatConditionPending{msg: "GitLab checks baseline time is unavailable; update the wakeup to establish a fresh baseline"}
		}
		if len(snapshot.TerminalPipelines) == 0 {
			return false, "", observed, errChatConditionPending{msg: fmt.Sprintf(
				"GitLab checks for %s have no terminal pipeline for the current head",
				chatGitLabTargetLabel(c),
			)}
		}
		for i := len(snapshot.TerminalPipelines) - 1; i >= 0; i-- {
			pipeline := snapshot.TerminalPipelines[i]
			if !chatGitLabPipelineAfterBaseline(
				pipeline, snapshot.HeadSHA, c.GitLab.BaselineAvailable,
				c.GitLab.BaselinePipelines, activatedAt,
			) {
				continue
			}
			observed["pipeline_project_id"] = pipeline.PipelineProjectID
			observed["pipeline_project_path"] = pipeline.PipelineProjectPath
			observed["pipeline_ref"] = pipeline.Ref
			observed["pipeline_id"] = pipeline.PipelineID
			observed["pipeline_status"] = pipeline.Status
			observed["pipeline_updated_at"] = pipeline.UpdatedAt
			fingerprint := strings.Join([]string{
				"gitlab", string(target.Source), target.ConnectionID, target.MergeRequestID,
				pipeline.PipelineProjectID, pipeline.PipelineProjectPath, pipeline.Ref,
				pipeline.PipelineID, pipeline.SHA, pipeline.Status, pipeline.UpdatedAt,
			}, ":")
			return true, fingerprint, observed, nil
		}
		return false, "", observed, nil
	default:
		return false, "", nil, errChatConditionPending{msg: "GitLab pull request event is unsupported; update the wakeup"}
	}
}

func (s *ChatWakeupService) evaluateChatCondition(ctx context.Context, tx pgx.Tx, q *db.Queries, w db.ChatWakeup) (bool, string, map[string]any, error) {
	var c ChatWakeupCondition
	if err := json.Unmarshal(w.Condition, &c); err != nil {
		return false, "", nil, err
	}
	switch c.Type {
	case "issue":
		var status string
		err := tx.QueryRow(ctx, "SELECT status FROM issue WHERE id=$1 AND workspace_id=$2", c.IssueID, w.WorkspaceID).Scan(&status)
		if errors.Is(err, pgx.ErrNoRows) {
			return true, "deleted", map[string]any{"issue": c.Identifier, "deleted": true}, nil
		}
		if err != nil {
			return false, "", nil, err
		}
		observed := map[string]any{"issue": c.Identifier, "status": status}
		if c.Status != "" {
			return status == c.Status, "status:" + status, observed, nil
		}
		category, err := issuestatus.CategoryWithError(ctx, q, w.WorkspaceID, status)
		if err != nil {
			return false, "", nil, err
		}
		var met bool
		switch c.State {
		case "done":
			met = category == "done"
		case "ended":
			met = category == "done" || category == "closed"
		case "in_review", "blocked":
			met = status == c.State
		}
		return met, "status:" + status, observed, nil
	case "children_done":
		parent, err := util.ParseUUID(c.IssueID)
		if err != nil {
			return false, "", nil, err
		}
		children, err := loadSubIssues(ctx, tx, q, parent, w.WorkspaceID)
		if err != nil {
			return false, "", nil, err
		}
		met, fingerprint, observed := childrenDone(WakeupCondition{Stage: c.Stage}, children)
		observed["issue"] = c.Identifier
		return met, fingerprint, observed, nil
	case "pull_request":
		if c.Provider == "gitlab" {
			return evaluateGitLabChatCondition(ctx, tx, w, c)
		}
		if c.Provider != "" || c.ConnectionID != "" {
			return false, "", nil, errChatConditionPending{msg: "The pull request provider is unsupported; update the wakeup condition"}
		}
		owner, name, _ := strings.Cut(c.Repo, "/")
		var state, head, snapshotHead, checks, url, title string
		err := tx.QueryRow(ctx, `SELECT state, head_sha, snapshot_head_sha, COALESCE(checks_rollup_state,''), html_url, title
			FROM github_pull_request
			WHERE workspace_id=$1 AND lower(repo_owner)=lower($2) AND lower(repo_name)=lower($3) AND pr_number=$4
			ORDER BY updated_at DESC LIMIT 1`, w.WorkspaceID, owner, name, c.Number).Scan(&state, &head, &snapshotHead, &checks, &url, &title)
		if errors.Is(err, pgx.ErrNoRows) {
			return false, "", nil, errChatConditionPending{"The pull request is no longer mirrored in this workspace; check the GitHub integration"}
		}
		if err != nil {
			return false, "", nil, err
		}
		observed := map[string]any{"pull_request": fmt.Sprintf("%s#%d", c.Repo, c.Number), "title": title, "url": url, "state": state}
		switch c.Event {
		case "merged":
			return state == "merged", "pr:" + state, observed, nil
		case "closed":
			return state == "merged" || state == "closed", "pr:" + state, observed, nil
		case "checks_finished":
			if snapshotHead == "" || snapshotHead != head {
				return false, "", observed, errChatConditionPending{"Waiting for a check snapshot of the pull request's current head"}
			}
			observed["head_sha"], observed["checks"] = head, strings.ToLower(checks)
			finished := slices.Contains([]string{"SUCCESS", "FAILURE", "ERROR"}, checks)
			return finished, "checks:" + head + ":" + checks, observed, nil
		}
	case "run":
		var status string
		var issueID pgtype.UUID
		err := tx.QueryRow(ctx, "SELECT status, issue_id FROM agent_task_queue WHERE id=$1", c.TaskID).Scan(&status, &issueID)
		if errors.Is(err, pgx.ErrNoRows) {
			return true, "deleted", map[string]any{"run": c.TaskID, "deleted": true}, nil
		}
		if err != nil {
			return false, "", nil, err
		}
		observed := map[string]any{"run": c.TaskID, "status": status}
		if issueID.Valid {
			observed["issue_id"] = util.UUIDToString(issueID)
		}
		return slices.Contains([]string{"completed", "failed", "cancelled"}, status), "run:" + status, observed, nil
	case "issue_reply":
		since, err := time.Parse(time.RFC3339Nano, c.Since)
		if err != nil {
			return false, "", nil, err
		}
		var commentID pgtype.UUID
		var authorName, content string
		err = tx.QueryRow(ctx, `SELECT c.id, COALESCE(u.name,''), left(c.content, 500)
			FROM comment c LEFT JOIN "user" u ON u.id=c.author_id
			WHERE c.issue_id=$1 AND c.workspace_id=$2 AND c.deleted_at IS NULL
			  AND c.author_type='member' AND c.created_at > $3
			  AND ($4::text = '' OR c.author_id::text = $4::text)
			ORDER BY c.created_at ASC LIMIT 1`, c.IssueID, w.WorkspaceID, since, c.AuthorID).Scan(&commentID, &authorName, &content)
		if errors.Is(err, pgx.ErrNoRows) {
			return false, "", nil, nil
		}
		if err != nil {
			return false, "", nil, err
		}
		id := util.UUIDToString(commentID)
		return true, "comment:" + id, map[string]any{"issue": c.Identifier, "comment_id": id, "author": authorName, "excerpt": content}, nil
	}
	return false, "", nil, fmt.Errorf("unknown chat wakeup condition %q", c.Type)
}

// describeChatCondition renders a condition for the woken agent.
func describeChatCondition(raw []byte) string {
	var c ChatWakeupCondition
	if json.Unmarshal(raw, &c) != nil {
		return ""
	}
	switch c.Type {
	case "issue":
		if c.Status != "" {
			return fmt.Sprintf("%s reaches status %s", c.Identifier, c.Status)
		}
		return fmt.Sprintf("%s is %s", c.Identifier, strings.ReplaceAll(c.State, "_", " "))
	case "children_done":
		if c.Stage != nil {
			return fmt.Sprintf("the sub-issues of %s up to stage %d are closed", c.Identifier, *c.Stage)
		}
		return fmt.Sprintf("every sub-issue of %s is closed", c.Identifier)
	case "pull_request":
		event := map[string]string{"merged": "merges", "closed": "is merged or closed", "checks_finished": "finishes its checks"}[c.Event]
		if c.Provider == "gitlab" {
			return fmt.Sprintf("GitLab merge request %s!%d on connection %s %s", c.Repo, c.Number, c.ConnectionID, event)
		}
		return fmt.Sprintf("pull request %s#%d %s", c.Repo, c.Number, event)
	case "run":
		return fmt.Sprintf("run %s ends", c.TaskID)
	case "issue_reply":
		if c.AuthorName != "" {
			return fmt.Sprintf("%s replies on %s", c.AuthorName, c.Identifier)
		}
		return fmt.Sprintf("a person replies on %s", c.Identifier)
	}
	return c.Type
}
