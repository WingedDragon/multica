package gitlabmigration

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

type legacyMR struct {
	ID, WorkspaceID, BindingID, ConnectionID         string
	ProjectID                                        int64
	ProjectPath, BaseURL                             string
	Number                                           int32
	Title, State, WebURL, HeadSHA                    string
	Branch, TargetBranch, AuthorLogin, AuthorAvatar  pgtype.Text
	MergedAt, ClosedAt                               pgtype.Timestamptz
	MRCreatedAt, MRUpdatedAt                         time.Time
	CreatedAt, UpdatedAt                             time.Time
	Additions, Deletions, ChangedFiles               int32
	Description, MergeCommitSHA, DetailedMergeStatus pgtype.Text
	HasConflicts                                     pgtype.Bool
	ConflictSHA                                      string
	SourceProjectID                                  pgtype.Int8
	SourceProjectPath                                pgtype.Text
	Reviewers, Assignees, Labels                     []byte
	LastRefreshedAt, PipelineSyncAt                  pgtype.Timestamptz
	LastRefreshError, PipelineSyncError              pgtype.Text
	WakeupGeneration                                 int64
}

type canonicalMR struct {
	ID, WorkspaceID, ConnectionID                  string
	RepoOwner, RepoName                            string
	Number                                         int32
	Title, State, WebURL, HeadSHA                  string
	Branch, AuthorLogin, AuthorAvatar              pgtype.Text
	MergedAt, ClosedAt                             pgtype.Timestamptz
	CreatedAt, UpdatedAt, PRCreatedAt, PRUpdatedAt time.Time
	Additions, Deletions, ChangedFiles             int32
	HasConflicts                                   pgtype.Bool
	ConflictHeadSHA                                string
	SourceProjectID                                pgtype.Int8
	SourceProjectPath                              pgtype.Text
	StatusSyncAt, PipelineSyncAt                   pgtype.Timestamptz
	StatusSyncError, PipelineSyncError             pgtype.Text
	WakeupGeneration                               int64
	TargetBranch                                   pgtype.Text
}

func (r *Runner) importMergeRequests(ctx context.Context, tx pgx.Tx, connections map[string]connectionMapping, projects map[string]projectMapping) (map[string]string, error) {
	rows, err := tx.Query(ctx, `
		SELECT mr.id::text, mr.workspace_id::text, mr.project_binding_id::text, pb.connection_id::text,
		       mr.gitlab_project_id, pb.path_with_namespace, gc.base_url, mr.mr_iid,
		       mr.title, mr.state, mr.web_url, mr.sha, mr.source_branch, mr.target_branch,
		       mr.author_username, mr.author_avatar_url, mr.merged_at, mr.closed_at,
		       mr.mr_created_at, mr.mr_updated_at, mr.created_at, mr.updated_at,
		       mr.additions, mr.deletions, mr.changed_files,
		       mr.description, mr.merge_commit_sha, mr.detailed_merge_status, mr.has_conflicts,
		       mr.conflict_sha, mr.source_project_id, mr.source_project_path,
		       mr.reviewers, mr.assignees, mr.labels, mr.last_refreshed_at, mr.last_refresh_error,
		       mr.pipeline_sync_at, mr.pipeline_sync_error, mr.wakeup_sync_generation
		FROM gitlab_merge_request mr
		JOIN gitlab_project_binding pb ON pb.workspace_id=mr.workspace_id AND pb.id=mr.project_binding_id
		JOIN gitlab_connection gc ON gc.workspace_id=pb.workspace_id AND gc.id=pb.connection_id
		ORDER BY mr.workspace_id, mr.id
	`)
	if err != nil {
		return nil, fmt.Errorf("read legacy GitLab merge requests: %w", err)
	}
	var legacyRows []legacyMR
	for rows.Next() {
		var old legacyMR
		if err := rows.Scan(
			&old.ID, &old.WorkspaceID, &old.BindingID, &old.ConnectionID,
			&old.ProjectID, &old.ProjectPath, &old.BaseURL, &old.Number,
			&old.Title, &old.State, &old.WebURL, &old.HeadSHA, &old.Branch, &old.TargetBranch,
			&old.AuthorLogin, &old.AuthorAvatar, &old.MergedAt, &old.ClosedAt,
			&old.MRCreatedAt, &old.MRUpdatedAt, &old.CreatedAt, &old.UpdatedAt,
			&old.Additions, &old.Deletions, &old.ChangedFiles,
			&old.Description, &old.MergeCommitSHA, &old.DetailedMergeStatus, &old.HasConflicts,
			&old.ConflictSHA, &old.SourceProjectID, &old.SourceProjectPath,
			&old.Reviewers, &old.Assignees, &old.Labels, &old.LastRefreshedAt, &old.LastRefreshError,
			&old.PipelineSyncAt, &old.PipelineSyncError, &old.WakeupGeneration,
		); err != nil {
			rows.Close()
			return nil, fmt.Errorf("read legacy GitLab MR row: %w", err)
		}
		legacyRows = append(legacyRows, old)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	rows.Close()
	mappings := make(map[string]string, len(legacyRows))
	for _, old := range legacyRows {
		connection, ok := connections[workspaceIdentity(old.WorkspaceID, old.ConnectionID)]
		if !ok || connection.Legacy.WorkspaceID != old.WorkspaceID {
			return nil, fmt.Errorf("legacy GitLab MR %s has no workspace-scoped connection mapping", old.ID)
		}
		project, ok := projects[workspaceIdentity(old.WorkspaceID, old.BindingID)]
		if !ok || project.WorkspaceID != old.WorkspaceID || project.ProjectID != old.ProjectID || project.Path != old.ProjectPath {
			return nil, fmt.Errorf("legacy GitLab MR %s has no exact workspace/project mapping", old.ID)
		}
		owner, name, ok := splitProjectPath(old.ProjectPath)
		if !ok {
			return nil, fmt.Errorf("GitLab project path %q cannot be split into full owner path and project name", old.ProjectPath)
		}
		old.ConnectionID = connection.ID
		canonical, err := r.upsertCanonicalMR(ctx, tx, old, owner, name)
		if err != nil {
			return nil, err
		}
		if err := upsertIDMapping(ctx, tx, old.WorkspaceID, "pull_request", old.ID, canonical.ID); err != nil {
			return nil, err
		}
		mappings[workspaceIdentity(old.WorkspaceID, old.ID)] = canonical.ID
		if err := r.upsertMRDetail(ctx, tx, old, canonical.ID); err != nil {
			return nil, err
		}
	}
	return mappings, nil
}

func (r *Runner) upsertCanonicalMR(ctx context.Context, tx pgx.Tx, old legacyMR, owner, name string) (canonicalMR, error) {
	current, err := loadCanonicalMR(ctx, tx, old.WorkspaceID, old.ConnectionID, owner, name, old.Number)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return canonicalMR{}, err
	}
	incoming := canonicalMR{
		WorkspaceID: old.WorkspaceID, ConnectionID: old.ConnectionID, RepoOwner: owner, RepoName: name, Number: old.Number,
		Title: old.Title, State: old.State, WebURL: old.WebURL, HeadSHA: old.HeadSHA, Branch: old.Branch,
		AuthorLogin: old.AuthorLogin, AuthorAvatar: old.AuthorAvatar, MergedAt: old.MergedAt, ClosedAt: old.ClosedAt,
		CreatedAt: old.CreatedAt, UpdatedAt: old.UpdatedAt, PRCreatedAt: old.MRCreatedAt, PRUpdatedAt: old.MRUpdatedAt,
		Additions: old.Additions, Deletions: old.Deletions, ChangedFiles: old.ChangedFiles,
		StatusSyncAt: old.LastRefreshedAt, StatusSyncError: old.LastRefreshError,
		PipelineSyncAt: old.PipelineSyncAt, PipelineSyncError: old.PipelineSyncError,
		WakeupGeneration: old.WakeupGeneration, TargetBranch: old.TargetBranch,
		SourceProjectID: old.SourceProjectID, SourceProjectPath: old.SourceProjectPath,
	}
	if old.ConflictSHA == old.HeadSHA && old.HasConflicts.Valid {
		incoming.HasConflicts = old.HasConflicts
		incoming.ConflictHeadSHA = old.ConflictSHA
	}
	if current.ID != "" {
		merged, err := mergeMRSnapshot(
			mrSnapshot{
				UpdatedAt: current.PRUpdatedAt, State: current.State, Title: current.Title, HeadSHA: current.HeadSHA,
				URL: current.WebURL, Branch: current.Branch.String, TargetBranch: current.TargetBranch.String,
				AuthorLogin: current.AuthorLogin.String, AuthorAvatar: current.AuthorAvatar.String,
				MergedAt: current.MergedAt, ClosedAt: current.ClosedAt,
				Additions: current.Additions, Deletions: current.Deletions, ChangedFiles: current.ChangedFiles,
			},
			mrSnapshot{
				UpdatedAt: incoming.PRUpdatedAt, State: incoming.State, Title: incoming.Title, HeadSHA: incoming.HeadSHA,
				URL: incoming.WebURL, Branch: incoming.Branch.String, TargetBranch: incoming.TargetBranch.String,
				AuthorLogin: incoming.AuthorLogin.String, AuthorAvatar: incoming.AuthorAvatar.String,
				MergedAt: incoming.MergedAt, ClosedAt: incoming.ClosedAt,
				Additions: incoming.Additions, Deletions: incoming.Deletions, ChangedFiles: incoming.ChangedFiles,
			},
		)
		if err != nil {
			return canonicalMR{}, fmt.Errorf("GitLab MR identity conflict for %s!%d: %w", owner+"/"+name, old.Number, err)
		}
		legacyIsNewer := old.MRUpdatedAt.After(current.PRUpdatedAt)
		incoming.ID = current.ID
		if !legacyIsNewer {
			incoming = current
			if incoming.AuthorLogin.String == "" {
				incoming.AuthorLogin = old.AuthorLogin
			}
			if incoming.AuthorAvatar.String == "" {
				incoming.AuthorAvatar = old.AuthorAvatar
			}
			if incoming.SourceProjectPath.String == "" && current.HeadSHA == old.HeadSHA {
				incoming.SourceProjectID, incoming.SourceProjectPath = old.SourceProjectID, old.SourceProjectPath
			}
			if !incoming.HasConflicts.Valid && old.ConflictSHA == incoming.HeadSHA {
				incoming.HasConflicts, incoming.ConflictHeadSHA = old.HasConflicts, old.ConflictSHA
			}
		} else {
			if incoming.AuthorLogin.String == "" {
				incoming.AuthorLogin = current.AuthorLogin
			}
			if incoming.AuthorAvatar.String == "" {
				incoming.AuthorAvatar = current.AuthorAvatar
			}
			if incoming.SourceProjectPath.String == "" && current.HeadSHA == incoming.HeadSHA {
				incoming.SourceProjectID, incoming.SourceProjectPath = current.SourceProjectID, current.SourceProjectPath
			}
			if !incoming.HasConflicts.Valid && current.ConflictHeadSHA == incoming.HeadSHA {
				incoming.HasConflicts, incoming.ConflictHeadSHA = current.HasConflicts, current.ConflictHeadSHA
			}
		}
		incoming.Title, incoming.State, incoming.HeadSHA, incoming.WebURL = merged.Title, merged.State, merged.HeadSHA, merged.URL
		incoming.MergedAt, incoming.ClosedAt = merged.MergedAt, merged.ClosedAt
		incoming.Additions, incoming.Deletions, incoming.ChangedFiles = merged.Additions, merged.Deletions, merged.ChangedFiles
		if merged.Branch != "" {
			incoming.Branch = pgtype.Text{String: merged.Branch, Valid: true}
		}
		if merged.TargetBranch != "" {
			incoming.TargetBranch = pgtype.Text{String: merged.TargetBranch, Valid: true}
		}
		if merged.AuthorLogin != "" {
			incoming.AuthorLogin = pgtype.Text{String: merged.AuthorLogin, Valid: true}
		}
		if merged.AuthorAvatar != "" {
			incoming.AuthorAvatar = pgtype.Text{String: merged.AuthorAvatar, Valid: true}
		}
		if current.PRCreatedAt.Before(incoming.PRCreatedAt) {
			incoming.PRCreatedAt = current.PRCreatedAt
		}
		if current.CreatedAt.Before(incoming.CreatedAt) {
			incoming.CreatedAt = current.CreatedAt
		}
		if current.UpdatedAt.After(incoming.UpdatedAt) {
			incoming.UpdatedAt = current.UpdatedAt
		}
		incoming.WakeupGeneration = max(current.WakeupGeneration, old.WakeupGeneration)
		incoming.StatusSyncAt, incoming.StatusSyncError, err = newerSync(current.StatusSyncAt, current.StatusSyncError, old.LastRefreshedAt, old.LastRefreshError)
		if err != nil {
			return canonicalMR{}, fmt.Errorf("GitLab MR %s!%d has conflicting status-sync versions: %w", owner+"/"+name, old.Number, err)
		}
		incoming.PipelineSyncAt, incoming.PipelineSyncError, err = newerSync(current.PipelineSyncAt, current.PipelineSyncError, old.PipelineSyncAt, old.PipelineSyncError)
		if err != nil {
			return canonicalMR{}, fmt.Errorf("GitLab MR %s!%d has conflicting pipeline-sync versions: %w", owner+"/"+name, old.Number, err)
		}
	} else {
		incoming.ID = old.ID
		var used bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM vcs_pull_request WHERE id=$1)`, incoming.ID).Scan(&used); err != nil {
			return canonicalMR{}, err
		}
		if used {
			if err := tx.QueryRow(ctx, `SELECT gen_random_uuid()::text`).Scan(&incoming.ID); err != nil {
				return canonicalMR{}, err
			}
		}
	}
	if incoming.ID == "" {
		return canonicalMR{}, errors.New("unified GitLab MR has no canonical ID")
	}
	if err := snapshotRow(ctx, tx, "pull_request", incoming.WorkspaceID, incoming.ID,
		`SELECT to_jsonb(pr) FROM vcs_pull_request pr WHERE workspace_id=$1 AND id=$2 FOR UPDATE`,
		incoming.WorkspaceID, incoming.ID); err != nil {
		return canonicalMR{}, fmt.Errorf("snapshot unified GitLab MR %s!%d: %w", owner+"/"+name, old.Number, err)
	}
	if current.ID == "" {
		_, err = tx.Exec(ctx, `
			INSERT INTO vcs_pull_request (
				id,workspace_id,connection_id,provider,repo_owner,repo_name,pr_number,title,state,html_url,
				branch,author_login,author_avatar_url,merged_at,closed_at,pr_created_at,pr_updated_at,
				additions,deletions,changed_files,head_sha,has_conflicts,conflict_head_sha,source_project_id,
				source_project_path,status_sync_at,status_sync_error,pipeline_sync_at,pipeline_sync_error,
				wakeup_sync_generation,created_at,updated_at
			) VALUES ($1,$2,$3,'gitlab',$4,$5,$6,$7,$8,$9,NULLIF($10,''),NULLIF($11,''),NULLIF($12,''),
				$13,$14,$15,$16,$17,$18,$19,$20,$21,$22,$23,NULLIF($24,''),$25,$26,$27,$28,$29,$30,$31)
		`, incoming.ID, incoming.WorkspaceID, incoming.ConnectionID, owner, name, incoming.Number, incoming.Title, incoming.State, incoming.WebURL,
			incoming.Branch.String, incoming.AuthorLogin.String, incoming.AuthorAvatar.String, incoming.MergedAt, incoming.ClosedAt,
			incoming.PRCreatedAt, incoming.PRUpdatedAt, incoming.Additions, incoming.Deletions, incoming.ChangedFiles, incoming.HeadSHA,
			nullableBool(incoming.HasConflicts), incoming.ConflictHeadSHA, nullableInt(incoming.SourceProjectID), nullableText(incoming.SourceProjectPath),
			incoming.StatusSyncAt, incoming.StatusSyncError, incoming.PipelineSyncAt, incoming.PipelineSyncError, incoming.WakeupGeneration, incoming.CreatedAt, incoming.UpdatedAt)
		if err != nil {
			return canonicalMR{}, fmt.Errorf("insert unified GitLab MR %s!%d: %w", owner+"/"+name, old.Number, err)
		}
	} else {
		_, err = tx.Exec(ctx, `
			UPDATE vcs_pull_request SET
				title=$2,state=$3,html_url=$4,branch=$5,author_login=$6,author_avatar_url=$7,
				merged_at=$8,closed_at=$9,pr_created_at=$10,pr_updated_at=$11,additions=$12,deletions=$13,
				changed_files=$14,head_sha=$15,has_conflicts=$16,conflict_head_sha=$17,source_project_id=$18,
				source_project_path=$19,status_sync_at=$20,status_sync_error=$21,pipeline_sync_at=$22,
				pipeline_sync_error=$23,wakeup_sync_generation=$24,updated_at=$25
			WHERE id=$1 AND workspace_id=$26 AND connection_id=$27 AND provider='gitlab'
		`, incoming.ID, incoming.Title, incoming.State, incoming.WebURL, incoming.Branch, incoming.AuthorLogin, incoming.AuthorAvatar,
			incoming.MergedAt, incoming.ClosedAt, incoming.PRCreatedAt, incoming.PRUpdatedAt, incoming.Additions, incoming.Deletions,
			incoming.ChangedFiles, incoming.HeadSHA, nullableBool(incoming.HasConflicts), incoming.ConflictHeadSHA,
			nullableInt(incoming.SourceProjectID), nullableText(incoming.SourceProjectPath), incoming.StatusSyncAt, incoming.StatusSyncError,
			incoming.PipelineSyncAt, incoming.PipelineSyncError, incoming.WakeupGeneration, incoming.UpdatedAt,
			incoming.WorkspaceID, incoming.ConnectionID)
		if err != nil {
			return canonicalMR{}, fmt.Errorf("merge unified GitLab MR %s!%d: %w", owner+"/"+name, old.Number, err)
		}
	}
	return incoming, nil
}

func loadCanonicalMR(ctx context.Context, tx pgx.Tx, workspaceID, connectionID, owner, name string, number int32) (canonicalMR, error) {
	var current canonicalMR
	err := tx.QueryRow(ctx, `
		SELECT pr.id::text, pr.workspace_id::text, pr.connection_id::text, pr.repo_owner, pr.repo_name, pr.pr_number,
		       pr.title, pr.state, pr.html_url, pr.head_sha, pr.branch, pr.author_login, pr.author_avatar_url,
		       pr.merged_at, pr.closed_at, pr.created_at, pr.updated_at, pr.pr_created_at, pr.pr_updated_at,
		       pr.additions, pr.deletions, pr.changed_files, pr.has_conflicts, pr.conflict_head_sha,
		       pr.source_project_id, pr.source_project_path, pr.status_sync_at, pr.status_sync_error,
		       pr.pipeline_sync_at, pr.pipeline_sync_error, pr.wakeup_sync_generation, detail.target_branch
		FROM vcs_pull_request pr
		LEFT JOIN vcs_gitlab_mr_detail detail ON detail.vcs_pull_request_id=pr.id
		WHERE pr.workspace_id=$1 AND pr.connection_id=$2 AND pr.provider='gitlab'
		  AND pr.repo_owner=$3 AND pr.repo_name=$4 AND pr.pr_number=$5
	`, workspaceID, connectionID, owner, name, number).Scan(
		&current.ID, &current.WorkspaceID, &current.ConnectionID, &current.RepoOwner, &current.RepoName, &current.Number,
		&current.Title, &current.State, &current.WebURL, &current.HeadSHA, &current.Branch, &current.AuthorLogin, &current.AuthorAvatar,
		&current.MergedAt, &current.ClosedAt, &current.CreatedAt, &current.UpdatedAt, &current.PRCreatedAt, &current.PRUpdatedAt,
		&current.Additions, &current.Deletions, &current.ChangedFiles, &current.HasConflicts, &current.ConflictHeadSHA,
		&current.SourceProjectID, &current.SourceProjectPath, &current.StatusSyncAt, &current.StatusSyncError,
		&current.PipelineSyncAt, &current.PipelineSyncError, &current.WakeupGeneration, &current.TargetBranch)
	return current, err
}

func (r *Runner) upsertMRDetail(ctx context.Context, tx pgx.Tx, old legacyMR, pullRequestID string) error {
	var currentDescription, currentMergeCommit, currentMergeStatus, currentTarget pgtype.Text
	var currentReviewers, currentAssignees, currentLabels []byte
	var currentLastRefresh pgtype.Timestamptz
	var currentLastRefreshError pgtype.Text
	var currentConflictSHA string
	var currentPipelineSync pgtype.Timestamptz
	var currentPipelineSyncError pgtype.Text
	var currentWakeupGeneration int64
	var currentCreatedAt, currentUpdatedAt time.Time
	err := tx.QueryRow(ctx, `
		SELECT description,merge_commit_sha,detailed_merge_status,target_branch,reviewers,assignees,labels,
		       last_refreshed_at,last_refresh_error,conflict_sha,pipeline_sync_at,pipeline_sync_error,
		       wakeup_sync_generation,created_at,updated_at
		FROM vcs_gitlab_mr_detail WHERE vcs_pull_request_id=$1 FOR UPDATE
	`, pullRequestID).Scan(
		&currentDescription, &currentMergeCommit, &currentMergeStatus, &currentTarget, &currentReviewers, &currentAssignees, &currentLabels,
		&currentLastRefresh, &currentLastRefreshError, &currentConflictSHA, &currentPipelineSync, &currentPipelineSyncError,
		&currentWakeupGeneration, &currentCreatedAt, &currentUpdatedAt,
	)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("read current GitLab details for %s: %w", pullRequestID, err)
	}
	if err == nil {
		if currentUpdatedAt.Equal(old.UpdatedAt) &&
			(!sameText(currentDescription, old.Description) || !sameText(currentMergeCommit, old.MergeCommitSHA) ||
				!sameText(currentMergeStatus, old.DetailedMergeStatus) || !sameText(currentTarget, old.TargetBranch) ||
				!jsonEqual(json.RawMessage(currentReviewers), json.RawMessage(old.Reviewers)) ||
				!jsonEqual(json.RawMessage(currentAssignees), json.RawMessage(old.Assignees)) ||
				!jsonEqual(json.RawMessage(currentLabels), json.RawMessage(old.Labels)) ||
				currentConflictSHA != old.ConflictSHA) {
			return fmt.Errorf("GitLab MR details for %s have conflicting same-version snapshots: %w", pullRequestID, errAmbiguousMR)
		}
	}
	mergedLastRefreshAt, mergedLastRefreshError := old.LastRefreshedAt, old.LastRefreshError
	mergedPipelineSyncAt, mergedPipelineSyncError := old.PipelineSyncAt, old.PipelineSyncError
	if err == nil {
		mergedLastRefreshAt, mergedLastRefreshError, err = newerSync(currentLastRefresh, currentLastRefreshError, old.LastRefreshedAt, old.LastRefreshError)
		if err != nil {
			return fmt.Errorf("GitLab MR details for %s have conflicting refresh versions: %w", pullRequestID, err)
		}
		mergedPipelineSyncAt, mergedPipelineSyncError, err = newerSync(currentPipelineSync, currentPipelineSyncError, old.PipelineSyncAt, old.PipelineSyncError)
		if err != nil {
			return fmt.Errorf("GitLab MR details for %s have conflicting pipeline-sync versions: %w", pullRequestID, err)
		}
	}
	if err := snapshotRow(ctx, tx, "gitlab_detail", old.WorkspaceID, pullRequestID,
		`SELECT to_jsonb(d) FROM vcs_gitlab_mr_detail d WHERE vcs_pull_request_id=$1 FOR UPDATE`, pullRequestID); err != nil {
		return fmt.Errorf("snapshot GitLab details for %s: %w", pullRequestID, err)
	}
	_, err = tx.Exec(ctx, `
		INSERT INTO vcs_gitlab_mr_detail (
			vcs_pull_request_id,description,merge_commit_sha,detailed_merge_status,target_branch,
			reviewers,assignees,labels,last_refreshed_at,last_refresh_error,conflict_sha,
			pipeline_sync_at,pipeline_sync_error,wakeup_sync_generation,created_at,updated_at
		) VALUES ($1,$2,$3,$4,$5,COALESCE($6::jsonb,'[]'::jsonb),COALESCE($7::jsonb,'[]'::jsonb),
			COALESCE($8::jsonb,'[]'::jsonb),$9,$10,$11,$12,$13,$14,$15,$16)
		ON CONFLICT (vcs_pull_request_id) DO UPDATE SET
			description=CASE WHEN EXCLUDED.updated_at >= vcs_gitlab_mr_detail.updated_at THEN EXCLUDED.description ELSE vcs_gitlab_mr_detail.description END,
			merge_commit_sha=CASE WHEN EXCLUDED.updated_at >= vcs_gitlab_mr_detail.updated_at THEN EXCLUDED.merge_commit_sha ELSE vcs_gitlab_mr_detail.merge_commit_sha END,
			detailed_merge_status=CASE WHEN EXCLUDED.updated_at >= vcs_gitlab_mr_detail.updated_at THEN EXCLUDED.detailed_merge_status ELSE vcs_gitlab_mr_detail.detailed_merge_status END,
			target_branch=CASE WHEN EXCLUDED.updated_at >= vcs_gitlab_mr_detail.updated_at THEN EXCLUDED.target_branch ELSE vcs_gitlab_mr_detail.target_branch END,
			reviewers=CASE WHEN EXCLUDED.updated_at >= vcs_gitlab_mr_detail.updated_at THEN EXCLUDED.reviewers ELSE vcs_gitlab_mr_detail.reviewers END,
			assignees=CASE WHEN EXCLUDED.updated_at >= vcs_gitlab_mr_detail.updated_at THEN EXCLUDED.assignees ELSE vcs_gitlab_mr_detail.assignees END,
			labels=CASE WHEN EXCLUDED.updated_at >= vcs_gitlab_mr_detail.updated_at THEN EXCLUDED.labels ELSE vcs_gitlab_mr_detail.labels END,
			last_refreshed_at=EXCLUDED.last_refreshed_at,
			last_refresh_error=EXCLUDED.last_refresh_error,
			conflict_sha=CASE WHEN EXCLUDED.updated_at >= vcs_gitlab_mr_detail.updated_at THEN EXCLUDED.conflict_sha ELSE vcs_gitlab_mr_detail.conflict_sha END,
			pipeline_sync_at=EXCLUDED.pipeline_sync_at,
			pipeline_sync_error=EXCLUDED.pipeline_sync_error,
			wakeup_sync_generation=GREATEST(EXCLUDED.wakeup_sync_generation,vcs_gitlab_mr_detail.wakeup_sync_generation),
			created_at=LEAST(EXCLUDED.created_at,vcs_gitlab_mr_detail.created_at),
			updated_at=GREATEST(EXCLUDED.updated_at,vcs_gitlab_mr_detail.updated_at)
	`, pullRequestID, old.Description, old.MergeCommitSHA, old.DetailedMergeStatus, old.TargetBranch,
		old.Reviewers, old.Assignees, old.Labels, mergedLastRefreshAt, mergedLastRefreshError, old.ConflictSHA,
		mergedPipelineSyncAt, mergedPipelineSyncError, old.WakeupGeneration, old.CreatedAt, old.UpdatedAt)
	if err != nil {
		return fmt.Errorf("migrate GitLab details for unified MR %s: %w", pullRequestID, err)
	}
	return nil
}

func sameText(a, b pgtype.Text) bool {
	return a.Valid == b.Valid && (!a.Valid || a.String == b.String)
}

func splitProjectPath(path string) (string, string, bool) {
	trimmed := strings.Trim(path, "/")
	index := strings.LastIndexByte(trimmed, '/')
	if index <= 0 || index == len(trimmed)-1 {
		return "", "", false
	}
	return trimmed[:index], trimmed[index+1:], true
}

func workspaceIdentity(workspaceID, id string) string { return workspaceID + "\x00" + id }

func max(a, b int64) int64 {
	if a > b {
		return a
	}
	return b
}
