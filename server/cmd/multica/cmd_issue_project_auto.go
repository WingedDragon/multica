package main

import (
	"context"
	"fmt"
	"net/url"
	"os/exec"
	"strings"

	"github.com/multica-ai/multica/server/internal/cli"
)

// resolveProjectFromCurrentGitRemote implements the repo-bound default for
// `multica issue create`: when the current checkout's origin URL uniquely
// matches a project resource URL, new top-level issues should land in that
// project without requiring every agent/skill prompt to remember --project.
//
// Rebase note: keep the "unique match only" rule. If future project-resource
// code changes this shape, do not fall back to the first match; an ambiguous
// repo URL must remain unbound unless the caller passes --project explicitly.
func resolveProjectFromCurrentGitRemote(ctx context.Context, client *cli.APIClient) (resolvedID, bool, error) {
	if client == nil || strings.TrimSpace(client.WorkspaceID) == "" {
		return resolvedID{}, false, nil
	}

	remoteURL, ok := currentGitOriginURL(ctx)
	if !ok {
		return resolvedID{}, false, nil
	}
	remoteKey, ok := normalizeGitRepoURLKey(remoteURL)
	if !ok {
		return resolvedID{}, false, nil
	}

	params := url.Values{"workspace_id": {client.WorkspaceID}}
	var projectsResp map[string]any
	if err := client.GetJSON(ctx, "/api/projects?"+params.Encode(), &projectsResp); err != nil {
		return resolvedID{}, false, fmt.Errorf("list projects for automatic project binding: %w", err)
	}

	projects, _ := projectsResp["projects"].([]any)
	matches := map[string]resolvedID{}
	for _, raw := range projects {
		project, ok := raw.(map[string]any)
		if !ok || !positiveJSONNumber(project["resource_count"]) {
			continue
		}
		projectID := strVal(project, "id")
		if projectID == "" {
			continue
		}
		if projectHasRepoResource(ctx, client, projectID, remoteKey) {
			matches[projectID] = resolvedID{
				ID:      projectID,
				Display: strVal(project, "title"),
			}
		}
	}

	switch len(matches) {
	case 0:
		return resolvedID{}, false, nil
	case 1:
		for _, match := range matches {
			return match, true, nil
		}
	}
	return resolvedID{}, false, fmt.Errorf("current git remote matches multiple project resources; pass --project explicitly")
}

func currentGitOriginURL(ctx context.Context) (string, bool) {
	out, err := exec.CommandContext(ctx, "git", "remote", "get-url", "origin").Output()
	if err != nil {
		return "", false
	}
	remote := strings.TrimSpace(string(out))
	if remote == "" {
		return "", false
	}
	return remote, true
}

func projectHasRepoResource(ctx context.Context, client *cli.APIClient, projectID, remoteKey string) bool {
	var resourcesResp map[string]any
	if err := client.GetJSON(ctx, "/api/projects/"+url.PathEscape(projectID)+"/resources", &resourcesResp); err != nil {
		return false
	}
	resources, _ := resourcesResp["resources"].([]any)
	for _, raw := range resources {
		resource, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		ref, ok := resource["resource_ref"].(map[string]any)
		if !ok {
			continue
		}
		resourceURL, _ := ref["url"].(string)
		resourceKey, ok := normalizeGitRepoURLKey(resourceURL)
		if ok && resourceKey == remoteKey {
			return true
		}
	}
	return false
}

func positiveJSONNumber(raw any) bool {
	switch n := raw.(type) {
	case float64:
		return n > 0
	case int:
		return n > 0
	case int64:
		return n > 0
	default:
		return false
	}
}

// normalizeGitRepoURLKey compares repository identity, not transport syntax.
// It intentionally drops scheme and username so these all match the same
// project resource during issue creation:
//   - git@code.example.com:org/repo.git
//   - ssh://git@code.example.com/org/repo.git
//   - https://code.example.com/org/repo.git
//
// Rebase note: keep host lowercase and path case-preserving. Git hosts are
// generally case-insensitive, but repo paths may be case-sensitive on custom
// servers, so only the host is normalized.
func normalizeGitRepoURLKey(raw string) (string, bool) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", false
	}
	if host, repoPath, ok := splitSCPLikeGitURL(raw); ok {
		return finishGitRepoURLKey(host, repoPath)
	}

	parsed, err := url.Parse(raw)
	if err != nil || parsed.Host == "" {
		return "", false
	}
	host := parsed.Hostname()
	if port := parsed.Port(); port != "" {
		host += ":" + port
	}
	return finishGitRepoURLKey(host, parsed.Path)
}

func splitSCPLikeGitURL(raw string) (host, repoPath string, ok bool) {
	if strings.Contains(raw, "://") {
		return "", "", false
	}
	left, right, found := strings.Cut(raw, ":")
	if !found || strings.Contains(left, "/") || strings.TrimSpace(right) == "" {
		return "", "", false
	}
	if at := strings.LastIndex(left, "@"); at >= 0 {
		left = left[at+1:]
	}
	if strings.TrimSpace(left) == "" {
		return "", "", false
	}
	return left, right, true
}

func finishGitRepoURLKey(host, repoPath string) (string, bool) {
	host = strings.ToLower(strings.TrimSpace(host))
	repoPath = strings.Trim(strings.TrimSpace(repoPath), "/")
	repoPath = strings.TrimSuffix(repoPath, ".git")
	repoPath = strings.Trim(repoPath, "/")
	if host == "" || repoPath == "" {
		return "", false
	}
	return host + "/" + repoPath, true
}
