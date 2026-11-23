package vcs

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"
)

const (
	gitLabWakeupPipelinePageLimit = 10
	gitLabWakeupPipelineTimeout   = 5 * time.Second
)

func (gitlabProvider) ReadGitLabWakeupMergeRequest(ctx context.Context, instanceURL, projectPath, token string, iid int32) (GitLabWakeupMergeRequestSnapshot, error) {
	if token == "" || strings.TrimSpace(projectPath) == "" || iid <= 0 {
		return GitLabWakeupMergeRequestSnapshot{}, errors.New("gitlab: incomplete MR refresh target")
	}
	baseURL := NormalizeInstanceURL(instanceURL)
	project, err := getGitLabWakeupProject(ctx, baseURL, projectPath, token)
	if err != nil {
		return GitLabWakeupMergeRequestSnapshot{}, err
	}
	var mr struct {
		IID                 int32  `json:"iid"`
		State               string `json:"state"`
		SHA                 string `json:"sha"`
		SourceBranch        string `json:"source_branch"`
		SourceProjectID     int64  `json:"source_project_id"`
		DetailedMergeStatus string `json:"detailed_merge_status"`
		SourceProject       struct {
			ID                int64  `json:"id"`
			PathWithNamespace string `json:"path_with_namespace"`
		} `json:"source_project"`
		HasConflicts *bool  `json:"has_conflicts"`
		UpdatedAt    string `json:"updated_at"`
		MergedAt     string `json:"merged_at"`
		ClosedAt     string `json:"closed_at"`
	}
	path := fmt.Sprintf("/api/v4/projects/%d/merge_requests/%d", project.ID, iid)
	if err := getGitLabWakeupJSON(ctx, baseURL, token, path, nil, &mr); err != nil {
		return GitLabWakeupMergeRequestSnapshot{}, err
	}
	if mr.IID != iid || mr.SHA == "" || mr.SourceBranch == "" || normalizeGitLabTime(mr.UpdatedAt) == "" {
		return GitLabWakeupMergeRequestSnapshot{}, fmt.Errorf("gitlab: MR !%d response is missing its current head, source branch, or update timestamp", iid)
	}
	sourceProjectID := mr.SourceProjectID
	if sourceProjectID == 0 {
		sourceProjectID = mr.SourceProject.ID
	}
	sourcePath := strings.TrimSpace(mr.SourceProject.PathWithNamespace)
	var pipelineError error
	if mr.SourceProject.ID != 0 && sourceProjectID != 0 && mr.SourceProject.ID != sourceProjectID {
		pipelineError = fmt.Errorf("gitlab: MR !%d source project identity is inconsistent", iid)
		sourcePath = ""
	}
	if sourceProjectID == project.ID && sourcePath == "" {
		sourcePath = project.PathWithNamespace
	}
	return GitLabWakeupMergeRequestSnapshot{
		MergeRequest: GitLabWakeupMergeRequest{
			State: normalizeGitLabMRState(mr.State, false), HeadSHA: mr.SHA,
			SourceBranch: mr.SourceBranch, SourceProjectID: sourceProjectID,
			SourceProjectPath: sourcePath,
			HasConflicts:      gitLabConflictState(mr.HasConflicts, mr.DetailedMergeStatus),
			UpdatedAt:         normalizeGitLabTime(mr.UpdatedAt),
			MergedAt:          normalizeGitLabTime(mr.MergedAt), ClosedAt: normalizeGitLabTime(mr.ClosedAt),
		},
		TargetProjectID:   project.ID,
		TargetProjectPath: project.PathWithNamespace,
		pipelineError:     pipelineError,
	}, nil
}

func (gitlabProvider) ReadGitLabWakeupPipelines(ctx context.Context, instanceURL, token string, iid int32, mrSnapshot GitLabWakeupMergeRequestSnapshot) GitLabWakeupPipelineSnapshot {
	if token == "" || iid <= 0 || mrSnapshot.TargetProjectID <= 0 {
		return GitLabWakeupPipelineSnapshot{PipelineError: errors.New("gitlab: incomplete Pipeline refresh target")}
	}
	baseURL := NormalizeInstanceURL(instanceURL)
	project := struct {
		ID                int64
		PathWithNamespace string
	}{ID: mrSnapshot.TargetProjectID, PathWithNamespace: mrSnapshot.TargetProjectPath}
	mr := struct {
		SHA             string
		SourceBranch    string
		SourceProjectID int64
		SourceProject   struct {
			ID                int64
			PathWithNamespace string
		}
	}{
		SHA:             mrSnapshot.MergeRequest.HeadSHA,
		SourceBranch:    mrSnapshot.MergeRequest.SourceBranch,
		SourceProjectID: mrSnapshot.MergeRequest.SourceProjectID,
	}
	mr.SourceProject.ID = mrSnapshot.MergeRequest.SourceProjectID
	mr.SourceProject.PathWithNamespace = mrSnapshot.MergeRequest.SourceProjectPath
	pipelineCtx, cancelPipeline := context.WithTimeout(ctx, gitLabWakeupPipelineTimeout)
	defer cancelPipeline()
	sourceProjectID := mr.SourceProjectID
	pipelineErrors := []error(nil)
	if mrSnapshot.pipelineError != nil {
		pipelineErrors = append(pipelineErrors, mrSnapshot.pipelineError)
	}
	sourcePath := strings.TrimSpace(mr.SourceProject.PathWithNamespace)
	if sourceProjectID == 0 {
		pipelineErrors = append(pipelineErrors, fmt.Errorf("gitlab: MR !%d source project identity is unavailable", iid))
	} else if sourcePath == "" {
		if sourceProjectID == project.ID {
			sourcePath = project.PathWithNamespace
		} else if len(pipelineErrors) == 0 {
			var sourceProject struct {
				ID                int64  `json:"id"`
				PathWithNamespace string `json:"path_with_namespace"`
			}
			if err := getGitLabWakeupJSON(pipelineCtx, baseURL, token, "/api/v4/projects/"+strconv.FormatInt(sourceProjectID, 10), nil, &sourceProject); err != nil {
				pipelineErrors = append(pipelineErrors, fmt.Errorf("gitlab: read MR !%d source project: %w", iid, err))
			} else if sourceProject.ID != sourceProjectID || sourceProject.PathWithNamespace == "" {
				pipelineErrors = append(pipelineErrors, fmt.Errorf("gitlab: MR !%d source project response is incomplete", iid))
			} else {
				sourcePath = sourceProject.PathWithNamespace
			}
		}
	}

	var mrPipelines []gitLabWakeupPipelinePayload
	mrPipelinePath := fmt.Sprintf("/api/v4/projects/%d/merge_requests/%d/pipelines", project.ID, iid)
	if err := listGitLabWakeupPipelines(pipelineCtx, baseURL, token, mrPipelinePath, nil, &mrPipelines); err != nil {
		pipelineErrors = append(pipelineErrors, fmt.Errorf("gitlab: list MR !%d pipelines: %w", iid, err))
	}
	var sourcePipelines []gitLabWakeupPipelinePayload
	if sourceProjectID > 0 && sourcePath != "" {
		filters := url.Values{}
		filters.Set("sha", mr.SHA)
		filters.Set("ref", mr.SourceBranch)
		sourcePipelinePath := "/api/v4/projects/" + strconv.FormatInt(sourceProjectID, 10) + "/pipelines"
		if err := listGitLabWakeupPipelines(pipelineCtx, baseURL, token, sourcePipelinePath, filters, &sourcePipelines); err != nil {
			pipelineErrors = append(pipelineErrors, fmt.Errorf("gitlab: list source-branch pipelines for MR !%d: %w", iid, err))
		}
	}

	type versionedPipeline struct {
		pipeline               GitLabWakeupPipeline
		updatedAt              time.Time
		isMergeRequestPipeline bool
	}
	type pipelineIdentity struct {
		projectID  int64
		pipelineID int64
	}
	pipelines := make(map[pipelineIdentity]versionedPipeline, len(mrPipelines)+len(sourcePipelines))
	add := func(p gitLabWakeupPipelinePayload, defaultProjectID int64, branchPipeline bool) error {
		if p.ID <= 0 || p.SHA != mr.SHA || (branchPipeline && p.Ref != mr.SourceBranch) {
			return nil
		}
		projectID := p.ProjectID
		if projectID <= 0 {
			projectID = defaultProjectID
		}
		if projectID != sourceProjectID && projectID != project.ID {
			return fmt.Errorf("gitlab: pipeline %d belongs to an unexpected project", p.ID)
		}
		if !branchPipeline || p.ProjectID <= 0 || p.UpdatedAt == "" || p.Status == "" {
			projectIDs := []int64{projectID}
			if sourceProjectID > 0 && sourceProjectID != projectID {
				projectIDs = append(projectIDs, sourceProjectID)
			}
			if project.ID != projectID && project.ID != sourceProjectID {
				projectIDs = append(projectIDs, project.ID)
			}
			detail, detailProjectID, err := getGitLabWakeupPipelineDetails(pipelineCtx, baseURL, token, projectIDs, p.ID)
			if err != nil {
				return err
			}
			projectID = detailProjectID
			if detail.ProjectID > 0 {
				projectID = detail.ProjectID
			}
			p.SHA = gitLabFirstValue(detail.SHA, p.SHA)
			p.Ref = gitLabFirstValue(detail.Ref, p.Ref)
			p.Status = gitLabFirstValue(detail.Status, p.Status)
			p.WebURL = gitLabFirstValue(detail.WebURL, p.WebURL)
			p.UpdatedAt = gitLabFirstValue(detail.UpdatedAt, p.UpdatedAt)
			p.FinishedAt = gitLabFirstValue(detail.FinishedAt, p.FinishedAt)
			p.CreatedAt = gitLabFirstValue(detail.CreatedAt, p.CreatedAt)
		}
		if projectID != sourceProjectID && projectID != project.ID {
			return fmt.Errorf("gitlab: pipeline %d belongs to an unexpected project", p.ID)
		}
		if p.SHA != mr.SHA || (branchPipeline && p.Ref != mr.SourceBranch) {
			return nil
		}
		if p.Status == "" {
			return fmt.Errorf("gitlab: pipeline %d details are missing status", p.ID)
		}
		updatedAt := normalizeGitLabTime(p.UpdatedAt)
		if updatedAt == "" {
			updatedAt = normalizeGitLabTime(p.FinishedAt)
		}
		if updatedAt == "" {
			return fmt.Errorf("gitlab: pipeline %d details have no state-version timestamp", p.ID)
		}
		parsedUpdatedAt, err := time.Parse(time.RFC3339Nano, updatedAt)
		if err != nil {
			return fmt.Errorf("gitlab: pipeline %d has invalid state-version timestamp", p.ID)
		}
		projectPath := sourcePath
		if projectID == project.ID {
			projectPath = project.PathWithNamespace
		}
		if projectPath == "" {
			return fmt.Errorf("gitlab: pipeline %d project path is unavailable", p.ID)
		}
		candidate := GitLabWakeupPipeline{
			ID: p.ID, ProjectID: projectID, ProjectPath: projectPath, SHA: p.SHA,
			Ref: p.Ref, Status: normalizeGitLabPipelineState(p.Status), WebURL: p.WebURL,
			UpdatedAt: updatedAt,
		}
		identity := pipelineIdentity{projectID: projectID, pipelineID: p.ID}
		if old, ok := pipelines[identity]; !ok || parsedUpdatedAt.After(old.updatedAt) {
			isMergeRequestPipeline := !branchPipeline
			if ok && old.isMergeRequestPipeline {
				isMergeRequestPipeline = true
			}
			pipelines[identity] = versionedPipeline{
				pipeline:               candidate,
				updatedAt:              parsedUpdatedAt,
				isMergeRequestPipeline: isMergeRequestPipeline,
			}
		}
		return nil
	}
	for _, p := range mrPipelines {
		if err := add(p, project.ID, false); err != nil {
			pipelineErrors = append(pipelineErrors, err)
		}
	}
	for _, p := range sourcePipelines {
		if err := add(p, sourceProjectID, true); err != nil {
			pipelineErrors = append(pipelineErrors, err)
		}
	}
	result := GitLabWakeupPipelineSnapshot{
		Pipelines:     make([]GitLabWakeupPipeline, 0, len(pipelines)),
		PipelineError: errors.Join(pipelineErrors...),
	}
	ordered := make([]versionedPipeline, 0, len(pipelines))
	for _, pipeline := range pipelines {
		ordered = append(ordered, pipeline)
	}
	sort.Slice(ordered, func(i, j int) bool {
		if ordered[i].isMergeRequestPipeline != ordered[j].isMergeRequestPipeline {
			return ordered[i].isMergeRequestPipeline
		}
		if ordered[i].pipeline.ProjectPath != ordered[j].pipeline.ProjectPath {
			return ordered[i].pipeline.ProjectPath < ordered[j].pipeline.ProjectPath
		}
		return ordered[i].pipeline.ID < ordered[j].pipeline.ID
	})
	for _, pipeline := range ordered {
		result.Pipelines = append(result.Pipelines, pipeline.pipeline)
	}
	return result
}

func getGitLabWakeupPipelineDetails(ctx context.Context, baseURL, token string, projectIDs []int64, pipelineID int64) (gitLabWakeupPipelinePayload, int64, error) {
	var lastErr error
	for _, projectID := range projectIDs {
		var pipeline gitLabWakeupPipelinePayload
		path := fmt.Sprintf("/api/v4/projects/%d/pipelines/%d", projectID, pipelineID)
		if err := getGitLabWakeupJSON(ctx, baseURL, token, path, nil, &pipeline); err != nil {
			lastErr = err
			continue
		}
		if pipeline.ID != pipelineID || (pipeline.ProjectID != 0 && pipeline.ProjectID != projectID) {
			lastErr = fmt.Errorf("gitlab: pipeline detail response does not match project %d pipeline %d", projectID, pipelineID)
			continue
		}
		if pipeline.ProjectID == 0 {
			pipeline.ProjectID = projectID
		}
		return pipeline, projectID, nil
	}
	if lastErr == nil {
		lastErr = errors.New("gitlab: no project supplied for pipeline detail lookup")
	}
	return gitLabWakeupPipelinePayload{}, 0, fmt.Errorf("gitlab: read pipeline %d details: %w", pipelineID, lastErr)
}

func gitLabFirstValue(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}

type gitLabWakeupProject struct {
	ID                int64  `json:"id"`
	PathWithNamespace string `json:"path_with_namespace"`
}

type gitLabWakeupPipelinePayload struct {
	ID         int64  `json:"id"`
	ProjectID  int64  `json:"project_id"`
	SHA        string `json:"sha"`
	Ref        string `json:"ref"`
	Status     string `json:"status"`
	WebURL     string `json:"web_url"`
	CreatedAt  string `json:"created_at"`
	FinishedAt string `json:"finished_at"`
	UpdatedAt  string `json:"updated_at"`
}

func getGitLabWakeupProject(ctx context.Context, baseURL, projectPath, token string) (gitLabWakeupProject, error) {
	var project gitLabWakeupProject
	endpoint := "/api/v4/projects/" + url.PathEscape(projectPath)
	if err := getGitLabWakeupJSON(ctx, baseURL, token, endpoint, nil, &project); err != nil {
		return project, err
	}
	if project.ID <= 0 || !strings.EqualFold(project.PathWithNamespace, strings.Trim(projectPath, "/")) {
		return project, errors.New("gitlab: project response does not match target path")
	}
	return project, nil
}

func getGitLabWakeupJSON(ctx context.Context, baseURL, token, path string, query url.Values, out any) error {
	u, err := url.Parse(strings.TrimRight(baseURL, "/") + path)
	if err != nil {
		return fmt.Errorf("gitlab: build request: %w", err)
	}
	if query != nil {
		u.RawQuery = query.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return fmt.Errorf("gitlab: build request: %w", err)
	}
	req.Header.Set("PRIVATE-TOKEN", token)
	req.Header.Set("Accept", "application/json")
	resp, err := httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("gitlab: request: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		return fmt.Errorf("gitlab API returned HTTP %d", resp.StatusCode)
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 4<<20)).Decode(out); err != nil {
		return fmt.Errorf("gitlab: decode %s: %w", path, err)
	}
	return nil
}

func listGitLabWakeupPipelines(ctx context.Context, baseURL, token, path string, filters url.Values, out *[]gitLabWakeupPipelinePayload) error {
	var pipelines []gitLabWakeupPipelinePayload
	query := url.Values{}
	for key, values := range filters {
		query[key] = append([]string(nil), values...)
	}
	for page := 1; page <= gitLabWakeupPipelinePageLimit; page++ {
		query.Set("page", strconv.Itoa(page))
		query.Set("per_page", "100")
		u, err := url.Parse(strings.TrimRight(baseURL, "/") + path)
		if err != nil {
			return fmt.Errorf("gitlab: build pipeline request: %w", err)
		}
		u.RawQuery = query.Encode()
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
		if err != nil {
			return fmt.Errorf("gitlab: build pipeline request: %w", err)
		}
		req.Header.Set("PRIVATE-TOKEN", token)
		req.Header.Set("Accept", "application/json")
		resp, err := httpClient.Do(req)
		if err != nil {
			return fmt.Errorf("gitlab: pipeline request: %w", err)
		}
		var pageItems []gitLabWakeupPipelinePayload
		if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
			resp.Body.Close()
			return fmt.Errorf("gitlab API returned HTTP %d", resp.StatusCode)
		}
		err = json.NewDecoder(io.LimitReader(resp.Body, 4<<20)).Decode(&pageItems)
		nextPage := strings.TrimSpace(resp.Header.Get("X-Next-Page"))
		resp.Body.Close()
		if err != nil {
			return fmt.Errorf("gitlab: decode %s: %w", path, err)
		}
		pipelines = append(pipelines, pageItems...)
		if nextPage == "" {
			*out = pipelines
			return nil
		}
		if page == gitLabWakeupPipelinePageLimit {
			return fmt.Errorf("gitlab: pipeline pagination exceeded %d pages", gitLabWakeupPipelinePageLimit)
		}
	}
	*out = pipelines
	return nil
}
