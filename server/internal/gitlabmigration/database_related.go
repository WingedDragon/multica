package gitlabmigration

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

func (r *Runner) importPipelines(ctx context.Context, tx pgx.Tx, connections map[string]connectionMapping, projects map[string]projectMapping, mrMappings map[string]string) error {
	rows, err := tx.Query(ctx, `
		SELECT old.workspace_id::text, old.id::text, old.mr_iid, old.sha,
		       old.source_project_id, COALESCE(old.source_project_path,''), COALESCE(old.source_branch,''),
		       pb.id::text, pb.connection_id::text, pb.gitlab_project_id, pb.path_with_namespace,
		       p.pipeline_id, p.pipeline_project_id, COALESCE(p.ref,''),
		       p.sha, p.status, p.web_url, p.pipeline_updated_at, p.created_at, p.updated_at
		FROM gitlab_mr_pipeline p
		JOIN gitlab_merge_request old ON old.id=p.merge_request_id
		JOIN gitlab_project_binding pb ON pb.id=old.project_binding_id AND pb.workspace_id=old.workspace_id
		JOIN gitlab_connection gc ON gc.id=pb.connection_id AND gc.workspace_id=pb.workspace_id
		ORDER BY old.workspace_id,old.id,p.pipeline_updated_at,p.pipeline_project_id,p.pipeline_id
	`)
	if err != nil {
		return fmt.Errorf("read legacy GitLab pipelines: %w", err)
	}
	type legacyPipeline struct {
		workspaceID, legacyMRID, headSHA, sourcePath, sourceBranch string
		mrNumber                                                   int32
		sourceProjectID                                            pgtype.Int8
		bindingID, legacyConnectionID, projectPath                 string
		projectID, pipelineID, pipelineProjectID                   int64
		ref, sha, rawStatus                                        string
		webURL                                                     pgtype.Text
		pipelineUpdatedAt, createdAt, updatedAt                    time.Time
	}
	var pipelines []legacyPipeline
	for rows.Next() {
		var pipeline legacyPipeline
		if err := rows.Scan(
			&pipeline.workspaceID, &pipeline.legacyMRID, &pipeline.mrNumber, &pipeline.headSHA, &pipeline.sourceProjectID,
			&pipeline.sourcePath, &pipeline.sourceBranch, &pipeline.bindingID, &pipeline.legacyConnectionID,
			&pipeline.projectID, &pipeline.projectPath, &pipeline.pipelineID, &pipeline.pipelineProjectID,
			&pipeline.ref, &pipeline.sha, &pipeline.rawStatus, &pipeline.webURL, &pipeline.pipelineUpdatedAt,
			&pipeline.createdAt, &pipeline.updatedAt,
		); err != nil {
			rows.Close()
			return fmt.Errorf("read legacy GitLab pipeline row: %w", err)
		}
		pipelines = append(pipelines, pipeline)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	rows.Close()
	for _, pipeline := range pipelines {
		connection, ok := connections[workspaceIdentity(pipeline.workspaceID, pipeline.legacyConnectionID)]
		if !ok || connection.Legacy.WorkspaceID != pipeline.workspaceID {
			return fmt.Errorf("legacy GitLab pipeline %d has no workspace-scoped connection mapping", pipeline.pipelineID)
		}
		project, ok := projects[workspaceIdentity(pipeline.workspaceID, pipeline.bindingID)]
		if !ok || project.WorkspaceID != pipeline.workspaceID || project.ProjectID != pipeline.projectID || project.Path != pipeline.projectPath {
			return fmt.Errorf("legacy GitLab pipeline %d has no exact project mapping", pipeline.pipelineID)
		}
		pullRequestID := mrMappings[workspaceIdentity(pipeline.workspaceID, pipeline.legacyMRID)]
		if pullRequestID == "" {
			return fmt.Errorf("legacy GitLab pipeline %d has no unified MR ID mapping", pipeline.pipelineID)
		}
		pipelineProjectID, pipelinePath, ok, err := resolvePipelineProjectIdentity(
			ctx, connection.API, pipeline.pipelineProjectID, pipeline.projectID, pipeline.projectPath,
			pipeline.sourceProjectID, pipeline.sourcePath, pipeline.pipelineID, pipeline.sha, pipeline.webURL)
		if err != nil {
			return fmt.Errorf("resolve GitLab pipeline %d project identity: %w", pipeline.pipelineID, err)
		}
		if !ok {
			return fmt.Errorf("GitLab pipeline %d on MR %s!%d has unknown project identity; resolve it before migration", pipeline.pipelineID, pipeline.projectPath, pipeline.mrNumber)
		}
		if err := upsertGitLabPipeline(ctx, tx, pipeline.workspaceID, connection.ID, pullRequestID, pipeline.pipelineID,
			pipelineProjectID, pipelinePath, pipeline.sha, pipeline.ref, validPipelineStatus(pipeline.rawStatus),
			pipeline.webURL, pipeline.pipelineUpdatedAt, pipeline.createdAt, pipeline.updatedAt); err != nil {
			return err
		}
	}
	return nil
}

type gitLabProjectIdentity struct {
	ID                int64  `json:"id"`
	PathWithNamespace string `json:"path_with_namespace"`
}

type gitLabPipelineIdentity struct {
	ID        int64  `json:"id"`
	ProjectID int64  `json:"project_id"`
	SHA       string `json:"sha"`
	Ref       string `json:"ref"`
	WebURL    string `json:"web_url"`
}

type pipelineProjectCandidate struct {
	id   int64
	path string
}

func resolvePipelineProjectIdentity(ctx context.Context, api *gitlabAPI, pipelineProjectID, targetProjectID int64, targetPath string,
	sourceProjectID pgtype.Int8, sourcePath string, pipelineID int64, sha string, webURL pgtype.Text,
) (int64, string, bool, error) {
	if pipelineID <= 0 || targetProjectID <= 0 || strings.TrimSpace(targetPath) == "" {
		return 0, "", false, nil
	}
	if sourceProjectID.Valid && sourceProjectID.Int64 > 0 &&
		((sourceProjectID.Int64 == targetProjectID && sourcePath != "" && sourcePath != targetPath) ||
			(sourcePath == targetPath && sourcePath != "" && sourceProjectID.Int64 != targetProjectID)) {
		return 0, "", false, errors.New("merge request source and target project identities conflict")
	}
	if pipelineProjectID > 0 {
		if id, path, ok := resolvePipelineProjectIdentityFromStoredFields(
			pipelineProjectID, targetProjectID, targetPath, sourceProjectID, sourcePath,
		); ok {
			return id, path, true, nil
		}
	}

	urlPresent := webURL.Valid && strings.TrimSpace(webURL.String) != ""
	if urlPresent {
		if api == nil {
			return 0, "", false, errors.New("GitLab connection API is unavailable to validate pipeline URL")
		}
		urlProjectPath, ok := pipelineProjectPathFromURL(api.baseURL, webURL.String, pipelineID)
		if !ok {
			return 0, "", false, errors.New("stored pipeline URL does not identify this pipeline on the connected GitLab instance")
		}
		if pipelineProjectID > 0 {
			project, err := gitLabProjectByID(ctx, api, pipelineProjectID)
			if err != nil {
				return 0, "", false, fmt.Errorf("verify pipeline project %d from its URL: %w", pipelineProjectID, err)
			}
			if project.PathWithNamespace != urlProjectPath {
				return 0, "", false, fmt.Errorf("pipeline URL project %q conflicts with API project %q", urlProjectPath, project.PathWithNamespace)
			}
			if err := validateProjectAgainstMR(project, targetProjectID, targetPath, sourceProjectID, sourcePath); err != nil {
				return 0, "", false, err
			}
			return project.ID, project.PathWithNamespace, true, nil
		}
		if urlProjectPath == targetPath {
			return targetProjectID, targetPath, true, nil
		}
		if sourcePath != "" && urlProjectPath == sourcePath {
			if sourceProjectID.Valid && sourceProjectID.Int64 > 0 {
				return sourceProjectID.Int64, sourcePath, true, nil
			}
			project, err := gitLabProjectByPath(ctx, api, sourcePath)
			if err != nil {
				return 0, "", false, fmt.Errorf("resolve pipeline source project %q: %w", sourcePath, err)
			}
			return project.ID, project.PathWithNamespace, true, nil
		}
		return 0, "", false, fmt.Errorf("pipeline URL project %q is neither the MR target nor its recorded source project", urlProjectPath)
	}

	if id, path, ok := resolvePipelineProjectIdentityFromStoredFields(
		pipelineProjectID, targetProjectID, targetPath, sourceProjectID, sourcePath,
	); ok {
		return id, path, true, nil
	}
	if api == nil {
		return 0, "", false, errors.New("GitLab connection API is unavailable to resolve pipeline project")
	}
	if pipelineProjectID > 0 {
		project, err := gitLabProjectByID(ctx, api, pipelineProjectID)
		if err != nil {
			return 0, "", false, fmt.Errorf("resolve pipeline project %d: %w", pipelineProjectID, err)
		}
		if err := validateProjectAgainstMR(project, targetProjectID, targetPath, sourceProjectID, sourcePath); err != nil {
			return 0, "", false, err
		}
		candidate := pipelineProjectCandidate{id: project.ID, path: project.PathWithNamespace}
		if err := verifyGitLabPipelineIdentity(ctx, api, candidate, pipelineID, sha); err != nil {
			return 0, "", false, err
		}
		return candidate.id, candidate.path, true, nil
	}

	targetProject, err := gitLabProjectByID(ctx, api, targetProjectID)
	if err != nil {
		return 0, "", false, fmt.Errorf("verify target project %d for URL-less pipeline: %w", targetProjectID, err)
	}
	if targetProject.PathWithNamespace != targetPath {
		return 0, "", false, fmt.Errorf("merge request target path %q conflicts with API project %q", targetPath, targetProject.PathWithNamespace)
	}
	candidates := []pipelineProjectCandidate{{id: targetProject.ID, path: targetProject.PathWithNamespace}}
	addCandidate := func(candidate pipelineProjectCandidate) error {
		for _, existing := range candidates {
			if existing.id == candidate.id && existing.path == candidate.path {
				return nil
			}
			if existing.id == candidate.id || existing.path == candidate.path {
				return fmt.Errorf("candidate project identities conflict: %d (%s) and %d (%s)",
					existing.id, existing.path, candidate.id, candidate.path)
			}
		}
		candidates = append(candidates, candidate)
		return nil
	}
	if sourceProjectID.Valid && sourceProjectID.Int64 > 0 {
		if sourceProjectID.Int64 != targetProjectID {
			project, err := gitLabProjectByID(ctx, api, sourceProjectID.Int64)
			if err != nil {
				return 0, "", false, fmt.Errorf("resolve merge request source project %d: %w", sourceProjectID.Int64, err)
			}
			if sourcePath != "" && project.PathWithNamespace != sourcePath {
				return 0, "", false, fmt.Errorf("merge request source path %q conflicts with API project %q", sourcePath, project.PathWithNamespace)
			}
			if err := addCandidate(pipelineProjectCandidate{id: project.ID, path: project.PathWithNamespace}); err != nil {
				return 0, "", false, err
			}
		}
	} else if sourcePath != "" {
		project, err := gitLabProjectByPath(ctx, api, sourcePath)
		if err != nil {
			return 0, "", false, fmt.Errorf("resolve merge request source project path %q: %w", sourcePath, err)
		}
		if err := addCandidate(pipelineProjectCandidate{id: project.ID, path: project.PathWithNamespace}); err != nil {
			return 0, "", false, err
		}
	}

	var matches []pipelineProjectCandidate
	var failures []error
	for _, candidate := range candidates {
		if err := verifyGitLabPipelineIdentity(ctx, api, candidate, pipelineID, sha); err != nil {
			failures = append(failures, fmt.Errorf("candidate project %d (%s): %w", candidate.id, candidate.path, err))
			continue
		}
		matches = append(matches, candidate)
	}
	if len(failures) != 0 {
		return 0, "", false, fmt.Errorf("pipeline project remains ambiguous because candidate verification failed: %w", errors.Join(failures...))
	}
	if len(matches) != 1 {
		if len(matches) > 1 {
			return 0, "", false, errors.New("pipeline ID and SHA exist in multiple candidate projects")
		}
		return 0, "", false, nil
	}
	return matches[0].id, matches[0].path, true, nil
}

func resolvePipelineProjectIdentityFromStoredFields(pipelineProjectID, targetProjectID int64, targetPath string,
	sourceProjectID pgtype.Int8, sourcePath string,
) (int64, string, bool) {
	if pipelineProjectID == targetProjectID && pipelineProjectID > 0 && strings.TrimSpace(targetPath) != "" {
		return pipelineProjectID, targetPath, true
	}
	if sourceProjectID.Valid && pipelineProjectID == sourceProjectID.Int64 && sourceProjectID.Int64 > 0 && strings.TrimSpace(sourcePath) != "" {
		return sourceProjectID.Int64, sourcePath, true
	}
	return 0, "", false
}

func pipelineProjectPathFromURL(instanceURL, rawURL string, pipelineID int64) (string, bool) {
	instance, err := url.Parse(strings.TrimSpace(instanceURL))
	if err != nil || instance.Scheme == "" || instance.Host == "" || instance.User != nil {
		return "", false
	}
	pipeline, err := url.Parse(strings.TrimSpace(rawURL))
	if err != nil || pipeline.Scheme == "" || pipeline.Host == "" || pipeline.User != nil ||
		pipeline.RawQuery != "" || pipeline.Fragment != "" ||
		!strings.EqualFold(instance.Scheme, pipeline.Scheme) || !strings.EqualFold(instance.Host, pipeline.Host) {
		return "", false
	}
	path := pipeline.EscapedPath()
	basePath := strings.TrimSuffix(instance.EscapedPath(), "/")
	if basePath != "" {
		if !strings.HasPrefix(path, basePath+"/") {
			return "", false
		}
		path = strings.TrimPrefix(path, basePath)
	}
	if strings.Contains(strings.ToLower(path), "%2f") || pipelineID <= 0 {
		return "", false
	}
	suffix := fmt.Sprintf("/-/pipelines/%d", pipelineID)
	if !strings.HasSuffix(path, suffix) {
		return "", false
	}
	escapedProjectPath := strings.TrimPrefix(strings.TrimSuffix(path, suffix), "/")
	if escapedProjectPath == "" || strings.Contains(escapedProjectPath, "//") {
		return "", false
	}
	projectPath, err := url.PathUnescape(escapedProjectPath)
	if err != nil || strings.TrimSpace(projectPath) == "" {
		return "", false
	}
	for _, segment := range strings.Split(projectPath, "/") {
		if segment == "" || segment == "." || segment == ".." {
			return "", false
		}
	}
	return projectPath, true
}

func gitLabProjectByID(ctx context.Context, api *gitlabAPI, projectID int64) (gitLabProjectIdentity, error) {
	var project gitLabProjectIdentity
	if projectID <= 0 {
		return project, errors.New("GitLab project ID is invalid")
	}
	_, err := api.request(ctx, http.MethodGet, "/projects/"+strconv.FormatInt(projectID, 10), nil, &project)
	if err != nil {
		return project, err
	}
	if project.ID != projectID || strings.TrimSpace(project.PathWithNamespace) == "" {
		return project, fmt.Errorf("GitLab returned an inconsistent project identity for ID %d", projectID)
	}
	return project, nil
}

func gitLabProjectByPath(ctx context.Context, api *gitlabAPI, projectPath string) (gitLabProjectIdentity, error) {
	var project gitLabProjectIdentity
	if strings.TrimSpace(projectPath) == "" {
		return project, errors.New("GitLab project path is empty")
	}
	_, err := api.request(ctx, http.MethodGet, "/projects/"+url.PathEscape(projectPath), nil, &project)
	if err != nil {
		return project, err
	}
	if project.ID <= 0 || project.PathWithNamespace != projectPath {
		return project, fmt.Errorf("GitLab returned project path %q for requested path %q", project.PathWithNamespace, projectPath)
	}
	return project, nil
}

func validateProjectAgainstMR(project gitLabProjectIdentity, targetProjectID int64, targetPath string, sourceProjectID pgtype.Int8, sourcePath string) error {
	if project.ID == targetProjectID && project.PathWithNamespace == targetPath {
		return nil
	}
	if sourceProjectID.Valid && sourceProjectID.Int64 > 0 && project.ID == sourceProjectID.Int64 &&
		(sourcePath == "" || project.PathWithNamespace == sourcePath) {
		return nil
	}
	if !sourceProjectID.Valid && sourcePath != "" && project.PathWithNamespace == sourcePath {
		return nil
	}
	if project.ID == targetProjectID || project.PathWithNamespace == targetPath {
		return fmt.Errorf("GitLab project identity (%d,%q) conflicts with MR target (%d,%q)", project.ID, project.PathWithNamespace, targetProjectID, targetPath)
	}
	if (sourceProjectID.Valid && sourceProjectID.Int64 > 0 && project.ID == sourceProjectID.Int64) ||
		(sourcePath != "" && project.PathWithNamespace == sourcePath) {
		return fmt.Errorf("GitLab project identity (%d,%q) conflicts with MR source (%d,%q)", project.ID, project.PathWithNamespace, sourceProjectID.Int64, sourcePath)
	}
	return fmt.Errorf("GitLab project identity (%d,%q) is neither the MR target nor its recorded source project", project.ID, project.PathWithNamespace)
}

func verifyGitLabPipelineIdentity(ctx context.Context, api *gitlabAPI, candidate pipelineProjectCandidate, pipelineID int64, sha string) error {
	var pipeline gitLabPipelineIdentity
	path := fmt.Sprintf("/projects/%d/pipelines/%d", candidate.id, pipelineID)
	if _, err := api.request(ctx, http.MethodGet, path, nil, &pipeline); err != nil {
		return err
	}
	if pipeline.ID != pipelineID || pipeline.ProjectID != candidate.id || strings.TrimSpace(sha) == "" ||
		strings.TrimSpace(pipeline.SHA) == "" || pipeline.SHA != sha {
		return errors.New("GitLab pipeline response does not match stored pipeline ID, project ID, and SHA")
	}
	if pipeline.WebURL != "" {
		projectPath, ok := pipelineProjectPathFromURL(api.baseURL, pipeline.WebURL, pipelineID)
		if !ok || projectPath != candidate.path {
			return fmt.Errorf("GitLab pipeline response URL does not match project %q and pipeline %d", candidate.path, pipelineID)
		}
	}
	// GitLab may normalize an MR pipeline ref to refs/merge-requests/<iid>/head;
	// ref is not project identity and is intentionally not compared.
	return nil
}

func upsertGitLabPipeline(ctx context.Context, tx pgx.Tx, workspaceID, connectionID, pullRequestID string, pipelineID, projectID int64, projectPath, sha, ref, status string, webURL pgtype.Text, updatedAt, createdAt, rowUpdatedAt time.Time) error {
	var currentWorkspaceID, currentConnectionID string
	var currentProjectID int64
	var currentSHA, currentRef, currentStatus string
	var currentURL pgtype.Text
	var currentPipelineUpdatedAt time.Time
	err := tx.QueryRow(ctx, `
		SELECT workspace_id::text,connection_id::text,pipeline_project_id,sha,ref,status,web_url,pipeline_updated_at
		FROM vcs_gitlab_pipeline
		WHERE pull_request_id=$1 AND pipeline_project_path=$2 AND pipeline_id=$3
		FOR UPDATE
	`, pullRequestID, projectPath, pipelineID).Scan(
		&currentWorkspaceID, &currentConnectionID, &currentProjectID, &currentSHA, &currentRef,
		&currentStatus, &currentURL, &currentPipelineUpdatedAt)
	if err == nil {
		if currentWorkspaceID != workspaceID || currentConnectionID != connectionID {
			return fmt.Errorf("GitLab pipeline %d path %q is stored in a different workspace or connection", pipelineID, projectPath)
		}
		if currentProjectID != projectID {
			return fmt.Errorf("GitLab pipeline %d path %q has conflicting project IDs %d and %d", pipelineID, projectPath, currentProjectID, projectID)
		}
		if currentPipelineUpdatedAt.Equal(updatedAt) && (currentSHA != sha || currentRef != ref || currentStatus != status || currentURL != webURL) {
			return fmt.Errorf("GitLab pipeline %d has conflicting same-version data", pipelineID)
		}
		if currentPipelineUpdatedAt.After(updatedAt) {
			return nil
		}
	} else if !errors.Is(err, pgx.ErrNoRows) {
		return err
	}
	recordKey := pullRequestID + "|" + projectPath + "|" + strconv.FormatInt(pipelineID, 10)
	if err := snapshotRow(ctx, tx, "pipeline", workspaceID, recordKey,
		`SELECT to_jsonb(p) FROM vcs_gitlab_pipeline p
		 WHERE workspace_id=$1 AND connection_id=$2 AND pull_request_id=$3
		   AND pipeline_project_path=$4 AND pipeline_id=$5 FOR UPDATE`,
		workspaceID, connectionID, pullRequestID, projectPath, pipelineID); err != nil {
		return fmt.Errorf("snapshot GitLab pipeline %d: %w", pipelineID, err)
	}
	_, err = tx.Exec(ctx, `
		INSERT INTO vcs_gitlab_pipeline (
			workspace_id,connection_id,pull_request_id,pipeline_id,pipeline_project_id,pipeline_project_path,
			sha,ref,status,web_url,pipeline_updated_at,created_at,updated_at
		) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13)
		ON CONFLICT (pull_request_id,pipeline_project_path,pipeline_id) DO UPDATE SET
			workspace_id=EXCLUDED.workspace_id,connection_id=EXCLUDED.connection_id,
			pipeline_project_id=EXCLUDED.pipeline_project_id,sha=EXCLUDED.sha,ref=EXCLUDED.ref,
			status=EXCLUDED.status,web_url=EXCLUDED.web_url,pipeline_updated_at=EXCLUDED.pipeline_updated_at,
			created_at=LEAST(vcs_gitlab_pipeline.created_at,EXCLUDED.created_at),
			updated_at=GREATEST(vcs_gitlab_pipeline.updated_at,EXCLUDED.updated_at)
		WHERE EXCLUDED.pipeline_updated_at >= vcs_gitlab_pipeline.pipeline_updated_at
	`, workspaceID, connectionID, pullRequestID, pipelineID, projectID, projectPath, sha, ref, status, webURL, updatedAt, createdAt, rowUpdatedAt)
	if err != nil {
		return fmt.Errorf("import GitLab pipeline %d for unified MR %s: %w", pipelineID, pullRequestID, err)
	}
	return nil
}

func (r *Runner) importIssueLinks(ctx context.Context, tx pgx.Tx, mrMappings map[string]string) error {
	rows, err := tx.Query(ctx, `
		SELECT workspace_id::text,issue_id::text,merge_request_id::text,close_intent,
		       COALESCE(linked_by_type,''),COALESCE(linked_by_id::text,''),linked_at
		FROM issue_gitlab_merge_request ORDER BY workspace_id,issue_id,merge_request_id
	`)
	if err != nil {
		return fmt.Errorf("read legacy GitLab issue links: %w", err)
	}
	type legacyIssueLink struct {
		workspaceID, issueID, oldMRID, linkedByType, linkedByID string
		closeIntent                                             bool
		linkedAt                                                time.Time
	}
	var links []legacyIssueLink
	for rows.Next() {
		var link legacyIssueLink
		if err := rows.Scan(&link.workspaceID, &link.issueID, &link.oldMRID, &link.closeIntent, &link.linkedByType, &link.linkedByID, &link.linkedAt); err != nil {
			rows.Close()
			return err
		}
		links = append(links, link)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	rows.Close()
	for _, link := range links {
		workspaceID, issueID, oldMRID := link.workspaceID, link.issueID, link.oldMRID
		linkedByType, linkedByID, closeIntent, linkedAt := link.linkedByType, link.linkedByID, link.closeIntent, link.linkedAt
		pullRequestID := mrMappings[workspaceIdentity(workspaceID, oldMRID)]
		if pullRequestID == "" {
			return fmt.Errorf("legacy issue link %s → %s has no workspace-scoped MR mapping", issueID, oldMRID)
		}
		var scoped bool
		if err := tx.QueryRow(ctx, `
			SELECT EXISTS (
				SELECT 1 FROM issue i JOIN vcs_pull_request pr ON pr.workspace_id=i.workspace_id
				WHERE i.id=$1 AND i.workspace_id=$3 AND pr.id=$2 AND pr.provider='gitlab'
			)
		`, issueID, pullRequestID, workspaceID).Scan(&scoped); err != nil {
			return fmt.Errorf("verify workspace scope for GitLab issue link %s → %s: %w", issueID, oldMRID, err)
		}
		if !scoped {
			return fmt.Errorf("legacy issue link %s → %s resolves outside its workspace", issueID, oldMRID)
		}
		if err := snapshotRow(ctx, tx, "issue_link", workspaceID, issueID+"|"+pullRequestID,
			`SELECT to_jsonb(l) FROM issue_vcs_pull_request l
			 JOIN issue i ON i.id=l.issue_id AND i.workspace_id=$3
			 JOIN vcs_pull_request pr ON pr.id=l.pull_request_id AND pr.workspace_id=$3 AND pr.provider='gitlab'
			 WHERE l.issue_id=$1 AND l.pull_request_id=$2 FOR UPDATE OF l`,
			issueID, pullRequestID, workspaceID); err != nil {
			return fmt.Errorf("snapshot GitLab issue link %s → %s: %w", issueID, oldMRID, err)
		}
		incoming := issueLinkState{
			GitLabCloseIntent:  closeIntent,
			GitLabLinkedByType: linkedByType,
			GitLabLinkedByID:   linkedByID,
			GitLabLinkedAt:     pgtype.Timestamptz{Time: linkedAt, Valid: true},
		}
		var current issueLinkState
		err := tx.QueryRow(ctx, `
			SELECT close_intent,COALESCE(linked_by_type,''),COALESCE(linked_by_id::text,''),linked_at,
			       vcs_linked,gitlab_close_intent,COALESCE(gitlab_linked_by_type,''),COALESCE(gitlab_linked_by_id::text,''),gitlab_linked_at
			FROM issue_vcs_pull_request WHERE issue_id=$1 AND pull_request_id=$2
		`, issueID, pullRequestID).Scan(
			&current.CloseIntent, &current.LinkedByType, &current.LinkedByID, &current.LinkedAt,
			&current.VCSLinked, &current.GitLabCloseIntent, &current.GitLabLinkedByType, &current.GitLabLinkedByID, &current.GitLabLinkedAt)
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		if errors.Is(err, pgx.ErrNoRows) {
			_, err = tx.Exec(ctx, `
				INSERT INTO issue_vcs_pull_request (
					issue_id,pull_request_id,close_intent,linked_by_type,linked_by_id,linked_at,
					vcs_linked,gitlab_close_intent,gitlab_linked_by_type,gitlab_linked_by_id,gitlab_linked_at
				) VALUES ($1,$2,false,NULL,NULL,$3,false,$4,NULLIF($5,''),NULLIF($6,'')::uuid,$3)
			`, issueID, pullRequestID, linkedAt, closeIntent, linkedByType, linkedByID)
			if err != nil {
				return fmt.Errorf("import GitLab issue link %s → %s: %w", issueID, oldMRID, err)
			}
			continue
		}
		merged, err := mergeIssueLink(current, incoming)
		if err != nil {
			return fmt.Errorf("issue link %s → MR %s: %w", issueID, oldMRID, err)
		}
		_, err = tx.Exec(ctx, `
			UPDATE issue_vcs_pull_request SET
				close_intent=$3,linked_by_type=NULLIF($4,''),linked_by_id=NULLIF($5,'')::uuid,linked_at=$6,
				vcs_linked=$7,gitlab_close_intent=$8,gitlab_linked_by_type=NULLIF($9,''),
				gitlab_linked_by_id=NULLIF($10,'')::uuid,gitlab_linked_at=$11
			WHERE issue_id=$1 AND pull_request_id=$2
		`, issueID, pullRequestID, merged.CloseIntent, merged.LinkedByType, merged.LinkedByID, merged.LinkedAt,
			merged.VCSLinked, merged.GitLabCloseIntent, merged.GitLabLinkedByType, merged.GitLabLinkedByID, merged.GitLabLinkedAt)
		if err != nil {
			return fmt.Errorf("merge issue links for %s → MR %s: %w", issueID, oldMRID, err)
		}
	}
	return nil
}

func (r *Runner) importWakeups(ctx context.Context, tx pgx.Tx, mrMappings map[string]string) error {
	rows, err := tx.Query(ctx, `
		SELECT id::text,workspace_id::text,condition::text,condition_state
		FROM chat_wakeup
		WHERE condition->>'provider'='gitlab'
		  AND condition->'gitlab'->>'source'='gitlab_connection'
		ORDER BY workspace_id,id
	`)
	if err != nil {
		return fmt.Errorf("read legacy GitLab chat wakeups: %w", err)
	}
	type legacyWakeup struct{ id, workspaceID, raw, conditionState string }
	var wakeups []legacyWakeup
	for rows.Next() {
		var wakeup legacyWakeup
		if err := rows.Scan(&wakeup.id, &wakeup.workspaceID, &wakeup.raw, &wakeup.conditionState); err != nil {
			rows.Close()
			return err
		}
		wakeups = append(wakeups, wakeup)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	rows.Close()
	connectionMappings := make(map[string]string)
	mappingRows, err := tx.Query(ctx, `
		SELECT workspace_id::text,legacy_id::text,unified_id::text
		FROM vcs_gitlab_migration_id_map WHERE entity_kind='connection'
	`)
	if err != nil {
		return err
	}
	for mappingRows.Next() {
		var workspaceID, legacyID, unifiedID string
		if err := mappingRows.Scan(&workspaceID, &legacyID, &unifiedID); err != nil {
			mappingRows.Close()
			return err
		}
		connectionMappings[workspaceIdentity(workspaceID, legacyID)] = unifiedID
	}
	if err := mappingRows.Err(); err != nil {
		mappingRows.Close()
		return err
	}
	mappingRows.Close()
	for _, wakeup := range wakeups {
		id, workspaceID, raw := wakeup.id, wakeup.workspaceID, wakeup.raw
		var condition map[string]json.RawMessage
		if err := json.Unmarshal([]byte(raw), &condition); err != nil {
			return fmt.Errorf("chat wakeup %s has invalid condition JSON", id)
		}
		var legacyConnectionID string
		if err := json.Unmarshal(condition["connection_id"], &legacyConnectionID); err != nil {
			return fmt.Errorf("chat wakeup %s has no connection ID", id)
		}
		var state map[string]json.RawMessage
		if err := json.Unmarshal(condition["gitlab"], &state); err != nil {
			return fmt.Errorf("chat wakeup %s has no persisted GitLab target", id)
		}
		var legacyMRID string
		if err := json.Unmarshal(state["merge_request_id"], &legacyMRID); err != nil {
			return fmt.Errorf("chat wakeup %s has no MR ID", id)
		}
		mapping := idMappings{
			Connections:  map[string]string{legacyConnectionID: connectionMappings[workspaceIdentity(workspaceID, legacyConnectionID)]},
			PullRequests: map[string]string{legacyMRID: mrMappings[workspaceIdentity(workspaceID, legacyMRID)]},
		}
		updated, changed, err := rewriteChatWakeupCondition([]byte(raw), mapping)
		if err != nil {
			return fmt.Errorf("migrate chat wakeup %s: %w", id, err)
		}
		if !changed {
			return fmt.Errorf("chat wakeup %s did not resolve to a legacy target", id)
		}
		updatedConditionState, err := rewriteChatWakeupConditionState(wakeup.conditionState, "gitlab_connection", "vcs_connection", mapping)
		if err != nil {
			return fmt.Errorf("migrate chat wakeup %s condition state: %w", id, err)
		}
		if err := snapshotRow(ctx, tx, "chat_wakeup", workspaceID, id,
			`SELECT jsonb_build_object('condition',condition,'condition_state',condition_state) FROM chat_wakeup WHERE id=$1 AND workspace_id=$2 FOR UPDATE`,
			id, workspaceID); err != nil {
			return fmt.Errorf("snapshot chat wakeup %s: %w", id, err)
		}
		if _, err := tx.Exec(ctx, `UPDATE chat_wakeup SET condition=$2::jsonb,condition_state=$3 WHERE id=$1 AND workspace_id=$4`, id, string(updated), updatedConditionState, workspaceID); err != nil {
			return fmt.Errorf("rewrite chat wakeup %s: %w", id, err)
		}
	}
	return nil
}

func (r *Runner) attachGitLabDetails(ctx context.Context, tx pgx.Tx) error {
	tables := []string{"gitlab_mr_approval_state", "gitlab_mr_discussion", "gitlab_mr_note", "gitlab_pipeline_job"}
	for _, table := range tables {
		unmappedSQL := fmt.Sprintf(`SELECT count(*) FROM %s d WHERE d.merge_request_id IS NOT NULL AND NOT EXISTS (
			SELECT 1 FROM vcs_gitlab_migration_id_map m
			WHERE m.workspace_id=d.workspace_id AND m.entity_kind='pull_request' AND m.legacy_id=d.merge_request_id
		)`, table)
		var unmapped int64
		if err := tx.QueryRow(ctx, unmappedSQL).Scan(&unmapped); err != nil {
			return fmt.Errorf("check %s MR detail identities: %w", table, err)
		}
		if unmapped != 0 {
			return fmt.Errorf("%s contains %d rows without a workspace-scoped MR mapping", table, unmapped)
		}
		targetSQL := fmt.Sprintf(`SELECT count(*) FROM %s d
			WHERE d.vcs_pull_request_id IS NOT NULL AND NOT EXISTS (
				SELECT 1 FROM vcs_pull_request pr
				WHERE pr.id=d.vcs_pull_request_id AND pr.workspace_id=d.workspace_id AND pr.provider='gitlab'
			)`, table)
		if err := tx.QueryRow(ctx, targetSQL).Scan(&unmapped); err != nil {
			return fmt.Errorf("verify existing %s unified MR identities: %w", table, err)
		}
		if unmapped != 0 {
			return fmt.Errorf("%s contains %d rows attached outside its GitLab workspace", table, unmapped)
		}
		mismatchSQL := fmt.Sprintf(`SELECT count(*) FROM %s d
			JOIN vcs_gitlab_migration_id_map m
			  ON m.workspace_id=d.workspace_id AND m.entity_kind='pull_request' AND m.legacy_id=d.merge_request_id
			LEFT JOIN vcs_pull_request pr
			  ON pr.id=m.unified_id AND pr.workspace_id=m.workspace_id AND pr.provider='gitlab'
			WHERE pr.id IS NULL OR (d.vcs_pull_request_id IS NOT NULL AND d.vcs_pull_request_id<>m.unified_id)`, table)
		if err := tx.QueryRow(ctx, mismatchSQL).Scan(&unmapped); err != nil {
			return fmt.Errorf("verify %s GitLab MR attachment targets: %w", table, err)
		}
		if unmapped != 0 {
			return fmt.Errorf("%s contains %d rows already attached to a different or invalid unified MR", table, unmapped)
		}
		if err := snapshotGitLabDetailRows(ctx, tx, table); err != nil {
			return err
		}
	}
	mrMappings, err := loadGitLabMRMappings(ctx, tx)
	if err != nil {
		return err
	}
	for _, table := range tables {
		if table == "gitlab_mr_approval_state" {
			updateSQL := fmt.Sprintf(`UPDATE %s d SET vcs_pull_request_id=m.unified_id
				FROM vcs_gitlab_migration_id_map m
				WHERE m.workspace_id=d.workspace_id AND m.entity_kind='pull_request' AND m.legacy_id=d.merge_request_id
				  AND d.vcs_pull_request_id IS NULL`, table)
			if _, err := tx.Exec(ctx, updateSQL); err != nil {
				return fmt.Errorf("attach %s rows to unified MRs: %w", table, err)
			}
		} else if err := mergeGitLabDetailRows(ctx, tx, table, mrMappings); err != nil {
			return fmt.Errorf("merge %s rows onto unified MRs: %w", table, err)
		}
		verifySQL := fmt.Sprintf(`SELECT count(*) FROM %s d JOIN vcs_gitlab_migration_id_map m
			ON m.workspace_id=d.workspace_id AND m.entity_kind='pull_request' AND m.legacy_id=d.merge_request_id
			WHERE d.vcs_pull_request_id IS DISTINCT FROM m.unified_id`, table)
		var unmapped int64
		if err := tx.QueryRow(ctx, verifySQL).Scan(&unmapped); err != nil {
			return fmt.Errorf("verify %s GitLab MR attachments: %w", table, err)
		}
		if unmapped != 0 {
			return fmt.Errorf("%s contains %d rows attached to a different unified MR", table, unmapped)
		}
	}
	return nil
}

func snapshotGitLabDetailRows(ctx context.Context, tx pgx.Tx, table string) error {
	keyExpr := "d.id::text"
	rowPredicate := "id=$2"
	if table == "gitlab_mr_approval_state" {
		keyExpr = "COALESCE(d.merge_request_id,d.vcs_pull_request_id)::text"
		rowPredicate = "(merge_request_id::text=$2 OR vcs_pull_request_id::text=$2)"
	}
	keysSQL := fmt.Sprintf(`SELECT d.workspace_id::text,%s
		FROM %s d
		WHERE d.merge_request_id IS NOT NULL OR EXISTS (
			SELECT 1 FROM vcs_gitlab_migration_id_map m
			WHERE m.workspace_id=d.workspace_id AND m.entity_kind='pull_request' AND m.unified_id=d.vcs_pull_request_id
		)
		ORDER BY d.workspace_id,%s`, keyExpr, table, keyExpr)
	rows, err := tx.Query(ctx, keysSQL)
	if err != nil {
		return fmt.Errorf("find %s rows affected by GitLab migration: %w", table, err)
	}
	type detailKey struct{ workspaceID, recordKey string }
	var affected []detailKey
	for rows.Next() {
		var key detailKey
		if err := rows.Scan(&key.workspaceID, &key.recordKey); err != nil {
			rows.Close()
			return err
		}
		affected = append(affected, key)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	rows.Close()
	for _, key := range affected {
		beforeSQL := fmt.Sprintf(`SELECT to_jsonb(d) FROM %s d WHERE d.workspace_id=$1 AND %s FOR UPDATE`, table, rowPredicate)
		if err := snapshotRow(ctx, tx, "legacy_detail:"+table, key.workspaceID, key.recordKey,
			beforeSQL, key.workspaceID, key.recordKey); err != nil {
			return fmt.Errorf("snapshot %s row %s: %w", table, key.recordKey, err)
		}
	}
	return nil
}

func loadGitLabMRMappings(ctx context.Context, tx pgx.Tx) (map[string]string, error) {
	rows, err := tx.Query(ctx, `
		SELECT workspace_id::text,legacy_id::text,unified_id::text
		FROM vcs_gitlab_migration_id_map WHERE entity_kind='pull_request'
	`)
	if err != nil {
		return nil, fmt.Errorf("read workspace-scoped GitLab MR mappings: %w", err)
	}
	defer rows.Close()
	mappings := make(map[string]string)
	for rows.Next() {
		var workspaceID, legacyID, unifiedID string
		if err := rows.Scan(&workspaceID, &legacyID, &unifiedID); err != nil {
			return nil, err
		}
		mappings[workspaceIdentity(workspaceID, legacyID)] = unifiedID
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return mappings, nil
}

type gitLabDetailRow struct {
	id, workspaceID, externalID  string
	legacyMRID, vcsPullRequestID pgtype.Text
	pullRequestID                string
	createdAt, updatedAt         time.Time
	raw                          []byte
}

type gitLabDetailMergePlan struct {
	table, workspaceID, pullRequestID, survivorID string
	rows                                          []gitLabDetailRow
	latest                                        gitLabDetailRow
	createdAt, updatedAt                          time.Time
}

func mergeGitLabDetailRows(ctx context.Context, tx pgx.Tx, table string, mrMappings map[string]string) error {
	externalColumns := map[string]string{
		"gitlab_mr_discussion": "gitlab_discussion_id",
		"gitlab_mr_note":       "gitlab_note_id",
		"gitlab_pipeline_job":  "job_id",
	}
	externalColumn, ok := externalColumns[table]
	if !ok {
		return fmt.Errorf("unsupported mergeable GitLab detail table %q", table)
	}
	query := fmt.Sprintf(`SELECT d.id::text,d.workspace_id::text,d.merge_request_id::text,
		d.vcs_pull_request_id::text,d.%s::text,d.created_at,d.updated_at,to_jsonb(d)
		FROM %s d
		WHERE d.merge_request_id IS NOT NULL OR EXISTS (
			SELECT 1 FROM vcs_gitlab_migration_id_map m
			WHERE m.workspace_id=d.workspace_id AND m.entity_kind='pull_request' AND m.unified_id=d.vcs_pull_request_id
		)
		ORDER BY d.workspace_id,d.%s,d.id`, externalColumn, table, externalColumn)
	rows, err := tx.Query(ctx, query)
	if err != nil {
		return fmt.Errorf("read %s rows for merge: %w", table, err)
	}
	var details []gitLabDetailRow
	for rows.Next() {
		var detail gitLabDetailRow
		if err := rows.Scan(&detail.id, &detail.workspaceID, &detail.legacyMRID, &detail.vcsPullRequestID,
			&detail.externalID, &detail.createdAt, &detail.updatedAt, &detail.raw); err != nil {
			rows.Close()
			return err
		}
		details = append(details, detail)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	rows.Close()

	groups := make(map[string][]gitLabDetailRow)
	for _, detail := range details {
		if detail.legacyMRID.Valid {
			detail.pullRequestID = mrMappings[workspaceIdentity(detail.workspaceID, detail.legacyMRID.String)]
			if detail.pullRequestID == "" {
				return fmt.Errorf("%s row %s has no workspace-scoped unified MR mapping", table, detail.id)
			}
			if detail.vcsPullRequestID.Valid && detail.vcsPullRequestID.String != detail.pullRequestID {
				return fmt.Errorf("%s row %s is attached to a conflicting unified MR", table, detail.id)
			}
		} else if detail.vcsPullRequestID.Valid {
			detail.pullRequestID = detail.vcsPullRequestID.String
		} else {
			continue
		}
		if detail.externalID == "" {
			return fmt.Errorf("%s row %s has an empty external identity", table, detail.id)
		}
		groupKey := workspaceIdentity(detail.workspaceID, detail.pullRequestID) + "\x00" + detail.externalID
		groups[groupKey] = append(groups[groupKey], detail)
	}
	keys := make([]string, 0, len(groups))
	for key := range groups {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	plans := make([]gitLabDetailMergePlan, 0, len(keys))
	for _, key := range keys {
		plan, err := buildGitLabDetailMergePlan(table, groups[key])
		if err != nil {
			return err
		}
		plans = append(plans, plan)
	}
	for _, plan := range plans {
		if err := updateGitLabDetailSurvivor(ctx, tx, plan); err != nil {
			return err
		}
	}
	if table == "gitlab_mr_discussion" {
		for _, plan := range plans {
			for _, detail := range plan.rows {
				if detail.id == plan.survivorID {
					continue
				}
				if _, err := tx.Exec(ctx, `
					UPDATE gitlab_mr_note SET discussion_id=$3
					WHERE workspace_id=$1 AND discussion_id=$2
				`, plan.workspaceID, detail.id, plan.survivorID); err != nil {
					return fmt.Errorf("reparent notes from duplicate GitLab discussion %s: %w", detail.id, err)
				}
			}
		}
	}
	for _, plan := range plans {
		for _, detail := range plan.rows {
			if detail.id == plan.survivorID {
				continue
			}
			tag, err := tx.Exec(ctx, fmt.Sprintf(`DELETE FROM %s WHERE workspace_id=$1 AND id=$2`, table), plan.workspaceID, detail.id)
			if err != nil {
				return fmt.Errorf("delete duplicate %s row %s: %w", table, detail.id, err)
			}
			if tag.RowsAffected() != 1 {
				return fmt.Errorf("expected to delete one duplicate %s row %s, deleted %d", table, detail.id, tag.RowsAffected())
			}
		}
	}
	return nil
}

func buildGitLabDetailMergePlan(table string, rows []gitLabDetailRow) (gitLabDetailMergePlan, error) {
	plan := gitLabDetailMergePlan{
		table: table, workspaceID: rows[0].workspaceID, pullRequestID: rows[0].pullRequestID,
		rows: rows, latest: rows[0], createdAt: rows[0].createdAt, updatedAt: rows[0].updatedAt,
	}
	canonicalID := ""
	for _, row := range rows {
		if row.pullRequestID != plan.pullRequestID || row.workspaceID != plan.workspaceID {
			return gitLabDetailMergePlan{}, fmt.Errorf("%s merge group crosses a workspace or MR boundary", table)
		}
		if row.vcsPullRequestID.Valid && row.vcsPullRequestID.String == plan.pullRequestID {
			if canonicalID != "" && canonicalID != row.id {
				return gitLabDetailMergePlan{}, fmt.Errorf("%s has multiple canonical rows for MR %s external ID %s", table, plan.pullRequestID, row.externalID)
			}
			canonicalID = row.id
		}
		if row.createdAt.Before(plan.createdAt) {
			plan.createdAt = row.createdAt
		}
		if row.updatedAt.After(plan.updatedAt) {
			plan.updatedAt = row.updatedAt
		}
		if row.updatedAt.After(plan.latest.updatedAt) {
			plan.latest = row
			continue
		}
		if row.updatedAt.Equal(plan.latest.updatedAt) {
			same, err := sameGitLabDetailContent(table, plan.latest.raw, row.raw)
			if err != nil {
				return gitLabDetailMergePlan{}, err
			}
			if !same {
				return gitLabDetailMergePlan{}, fmt.Errorf("%s rows for MR %s external ID %s have conflicting same-version content: %w", table, plan.pullRequestID, row.externalID, errAmbiguousMR)
			}
			if row.id < plan.latest.id {
				plan.latest = row
			}
		}
	}
	if canonicalID != "" {
		plan.survivorID = canonicalID
	} else {
		plan.survivorID = plan.latest.id
	}
	return plan, nil
}

func sameGitLabDetailContent(table string, a, b []byte) (bool, error) {
	var left, right map[string]json.RawMessage
	if err := json.Unmarshal(a, &left); err != nil {
		return false, err
	}
	if err := json.Unmarshal(b, &right); err != nil {
		return false, err
	}
	for _, field := range []string{"id", "workspace_id", "merge_request_id", "vcs_pull_request_id", "created_at", "updated_at"} {
		delete(left, field)
		delete(right, field)
	}
	if table == "gitlab_mr_note" {
		delete(left, "discussion_id")
		delete(right, "discussion_id")
	}
	leftJSON, err := json.Marshal(left)
	if err != nil {
		return false, err
	}
	rightJSON, err := json.Marshal(right)
	if err != nil {
		return false, err
	}
	return string(leftJSON) == string(rightJSON), nil
}

func updateGitLabDetailSurvivor(ctx context.Context, tx pgx.Tx, plan gitLabDetailMergePlan) error {
	var query string
	switch plan.table {
	case "gitlab_mr_discussion":
		query = `UPDATE gitlab_mr_discussion AS target SET
			vcs_pull_request_id=$3::uuid,
			individual_note=(jsonb_populate_record(NULL::gitlab_mr_discussion,$4::jsonb)).individual_note,
			resolved=(jsonb_populate_record(NULL::gitlab_mr_discussion,$4::jsonb)).resolved,
			discussion_created_at=(jsonb_populate_record(NULL::gitlab_mr_discussion,$4::jsonb)).discussion_created_at,
			discussion_updated_at=(jsonb_populate_record(NULL::gitlab_mr_discussion,$4::jsonb)).discussion_updated_at,
			fetched_at=(jsonb_populate_record(NULL::gitlab_mr_discussion,$4::jsonb)).fetched_at,
			created_at=$5,updated_at=$6
			WHERE target.workspace_id=$1 AND target.id=$2`
	case "gitlab_mr_note":
		query = `UPDATE gitlab_mr_note AS target SET
			vcs_pull_request_id=$3::uuid,
			author_username=(jsonb_populate_record(NULL::gitlab_mr_note,$4::jsonb)).author_username,
			author_avatar_url=(jsonb_populate_record(NULL::gitlab_mr_note,$4::jsonb)).author_avatar_url,
			body=(jsonb_populate_record(NULL::gitlab_mr_note,$4::jsonb)).body,
			system=(jsonb_populate_record(NULL::gitlab_mr_note,$4::jsonb)).system,
			resolved=(jsonb_populate_record(NULL::gitlab_mr_note,$4::jsonb)).resolved,
			resolvable=(jsonb_populate_record(NULL::gitlab_mr_note,$4::jsonb)).resolvable,
			note_created_at=(jsonb_populate_record(NULL::gitlab_mr_note,$4::jsonb)).note_created_at,
			note_updated_at=(jsonb_populate_record(NULL::gitlab_mr_note,$4::jsonb)).note_updated_at,
			created_at=$5,updated_at=$6
			WHERE target.workspace_id=$1 AND target.id=$2`
	case "gitlab_pipeline_job":
		query = `UPDATE gitlab_pipeline_job AS target SET
			vcs_pull_request_id=$3::uuid,
			pipeline_id=(jsonb_populate_record(NULL::gitlab_pipeline_job,$4::jsonb)).pipeline_id,
			name=(jsonb_populate_record(NULL::gitlab_pipeline_job,$4::jsonb)).name,
			stage=(jsonb_populate_record(NULL::gitlab_pipeline_job,$4::jsonb)).stage,
			status=(jsonb_populate_record(NULL::gitlab_pipeline_job,$4::jsonb)).status,
			ref=(jsonb_populate_record(NULL::gitlab_pipeline_job,$4::jsonb)).ref,
			sha=(jsonb_populate_record(NULL::gitlab_pipeline_job,$4::jsonb)).sha,
			web_url=(jsonb_populate_record(NULL::gitlab_pipeline_job,$4::jsonb)).web_url,
			started_at=(jsonb_populate_record(NULL::gitlab_pipeline_job,$4::jsonb)).started_at,
			finished_at=(jsonb_populate_record(NULL::gitlab_pipeline_job,$4::jsonb)).finished_at,
			duration_seconds=(jsonb_populate_record(NULL::gitlab_pipeline_job,$4::jsonb)).duration_seconds,
			queued_duration_seconds=(jsonb_populate_record(NULL::gitlab_pipeline_job,$4::jsonb)).queued_duration_seconds,
			failure_reason=(jsonb_populate_record(NULL::gitlab_pipeline_job,$4::jsonb)).failure_reason,
			allow_failure=(jsonb_populate_record(NULL::gitlab_pipeline_job,$4::jsonb)).allow_failure,
			artifacts_file_name=(jsonb_populate_record(NULL::gitlab_pipeline_job,$4::jsonb)).artifacts_file_name,
			artifacts_file_size=(jsonb_populate_record(NULL::gitlab_pipeline_job,$4::jsonb)).artifacts_file_size,
			artifacts_expire_at=(jsonb_populate_record(NULL::gitlab_pipeline_job,$4::jsonb)).artifacts_expire_at,
			trace_summary=(jsonb_populate_record(NULL::gitlab_pipeline_job,$4::jsonb)).trace_summary,
			trace_truncated=(jsonb_populate_record(NULL::gitlab_pipeline_job,$4::jsonb)).trace_truncated,
			trace_fetched_at=(jsonb_populate_record(NULL::gitlab_pipeline_job,$4::jsonb)).trace_fetched_at,
			fetched_at=(jsonb_populate_record(NULL::gitlab_pipeline_job,$4::jsonb)).fetched_at,
			created_at=$5,updated_at=$6
			WHERE target.workspace_id=$1 AND target.id=$2`
	default:
		return fmt.Errorf("unsupported GitLab detail merge table %q", plan.table)
	}
	tag, err := tx.Exec(ctx, query, plan.workspaceID, plan.survivorID, plan.pullRequestID,
		string(plan.latest.raw), plan.createdAt, plan.updatedAt)
	if err != nil {
		return fmt.Errorf("update %s survivor row %s: %w", plan.table, plan.survivorID, err)
	}
	if tag.RowsAffected() != 1 {
		return fmt.Errorf("expected one %s survivor row %s, updated %d", plan.table, plan.survivorID, tag.RowsAffected())
	}
	return nil
}

func verifyImportedRows(ctx context.Context, tx pgx.Tx) error {
	checks := []struct {
		name string
		sql  string
	}{
		{"legacy merge requests", `SELECT count(*) FROM gitlab_merge_request mr WHERE NOT EXISTS (
			SELECT 1 FROM vcs_gitlab_migration_id_map m WHERE m.workspace_id=mr.workspace_id AND m.entity_kind='pull_request' AND m.legacy_id=mr.id)`},
		{"legacy issue links", `SELECT count(*) FROM issue_gitlab_merge_request old WHERE NOT EXISTS (
			SELECT 1 FROM vcs_gitlab_migration_id_map m WHERE m.workspace_id=old.workspace_id AND m.entity_kind='pull_request' AND m.legacy_id=old.merge_request_id)`},
		{"unmapped active chat conditions", `SELECT count(*) FROM chat_wakeup WHERE status='active' AND condition->>'provider'='gitlab' AND condition->'gitlab'->>'source'='gitlab_connection'`},
		{"approval detail rows", `SELECT count(*) FROM gitlab_mr_approval_state WHERE vcs_pull_request_id IS NULL`},
		{"discussion detail rows", `SELECT count(*) FROM gitlab_mr_discussion WHERE vcs_pull_request_id IS NULL`},
		{"note detail rows", `SELECT count(*) FROM gitlab_mr_note WHERE vcs_pull_request_id IS NULL`},
		{"job detail rows", `SELECT count(*) FROM gitlab_pipeline_job WHERE vcs_pull_request_id IS NULL`},
	}
	for _, check := range checks {
		var count int64
		if err := tx.QueryRow(ctx, check.sql).Scan(&count); err != nil {
			return fmt.Errorf("verify %s: %w", check.name, err)
		}
		if count != 0 {
			return fmt.Errorf("migration verification found %d %s without canonical identity", count, check.name)
		}
	}
	return nil
}

func projectGroupAncestor(groupPath, projectPath string) bool {
	groupPath = strings.Trim(strings.ToLower(groupPath), "/")
	projectPath = strings.Trim(strings.ToLower(projectPath), "/")
	return groupPath != "" && strings.HasPrefix(projectPath, groupPath+"/")
}

func manualHookURL(publicURL, connectionID, scopeType, scopeID string, hookID int64) (string, error) {
	base := strings.TrimRight(strings.TrimSpace(publicURL), "/")
	if base == "" || hookID < 0 {
		return "", errors.New("public webhook URL or Hook identity is invalid")
	}
	if scopeType != "project" && scopeType != "group" && scopeType != "system" {
		return "", fmt.Errorf("unsupported Hook scope %q", scopeType)
	}
	if _, err := strconv.ParseInt(scopeID, 10, 64); err != nil {
		return "", errors.New("Hook scope ID must be a decimal GitLab ID")
	}
	values := url.Values{}
	values.Set("scope_type", scopeType)
	values.Set("scope_id", scopeID)
	values.Set("hook_id", strconv.FormatInt(hookID, 10))
	return base + "/api/webhooks/vcs/" + connectionID + "?" + values.Encode(), nil
}

func timeAfter(a, b time.Time) bool { return a.After(b) }
