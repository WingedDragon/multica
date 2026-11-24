package gitlabmigration

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

const maxGitLabResponseBytes = 32 << 20

type gitlabAPI struct {
	baseURL            string
	token              string
	client             *http.Client
	currentUserID      int64
	currentUserIsAdmin bool
}

type gitlabAPIError struct {
	path       string
	statusCode int
}

func (e *gitlabAPIError) Error() string {
	return fmt.Sprintf("GitLab API %s returned HTTP %d", e.path, e.statusCode)
}

func gitLabHTTPStatus(err error) int {
	var apiErr *gitlabAPIError
	if errors.As(err, &apiErr) {
		return apiErr.statusCode
	}
	return 0
}

func newGitLabAPI(baseURL, token, proxyURL string) (*gitlabAPI, error) {
	parsed, err := url.Parse(strings.TrimRight(strings.TrimSpace(baseURL), "/"))
	if err != nil || (parsed.Scheme != "https" && parsed.Scheme != "http") || parsed.Host == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return nil, errors.New("GitLab instance URL is invalid")
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	if strings.TrimSpace(proxyURL) != "" {
		proxy, err := url.Parse(strings.TrimSpace(proxyURL))
		if err != nil || proxy.Scheme == "" || proxy.Host == "" {
			return nil, errors.New("GitLab proxy URL is invalid")
		}
		transport.Proxy = http.ProxyURL(proxy)
	}
	return &gitlabAPI{
		baseURL: strings.TrimRight(baseURL, "/"),
		token:   token,
		client:  &http.Client{Transport: transport, Timeout: 20 * time.Second},
	}, nil
}

func (c *gitlabAPI) request(ctx context.Context, method, path string, body any, out any) (http.Header, error) {
	var requestBody io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			return nil, fmt.Errorf("encode GitLab API request: %w", err)
		}
		requestBody = bytes.NewReader(encoded)
	}
	request, err := http.NewRequestWithContext(ctx, method, c.baseURL+"/api/v4"+path, requestBody)
	if err != nil {
		return nil, fmt.Errorf("build GitLab API request: %w", err)
	}
	request.Header.Set("PRIVATE-TOKEN", c.token)
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	response, err := c.client.Do(request)
	if err != nil {
		return nil, fmt.Errorf("GitLab API %s failed: %w", path, err)
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return response.Header, &gitlabAPIError{path: path, statusCode: response.StatusCode}
	}
	if out != nil {
		if err := json.NewDecoder(io.LimitReader(response.Body, maxGitLabResponseBytes)).Decode(out); err != nil {
			return response.Header, fmt.Errorf("decode GitLab API %s response: %w", path, err)
		}
	} else if _, err := io.Copy(io.Discard, io.LimitReader(response.Body, maxGitLabResponseBytes)); err != nil {
		return response.Header, fmt.Errorf("read GitLab API %s response: %w", path, err)
	}
	return response.Header, nil
}

func (c *gitlabAPI) currentUser(ctx context.Context) (string, error) {
	var user struct {
		ID       int64  `json:"id"`
		Username string `json:"username"`
		IsAdmin  bool   `json:"is_admin"`
	}
	if _, err := c.request(ctx, http.MethodGet, "/user", nil, &user); err != nil {
		return "", err
	}
	if strings.TrimSpace(user.Username) == "" {
		return "", errors.New("GitLab returned an empty username")
	}
	c.currentUserID = user.ID
	c.currentUserIsAdmin = user.IsAdmin
	return user.Username, nil
}

func (c *gitlabAPI) groupHooksUnsupportedByMetadata(ctx context.Context) bool {
	var metadata struct {
		Enterprise *bool  `json:"enterprise"`
		Plan       string `json:"plan"`
	}
	if _, err := c.request(ctx, http.MethodGet, "/metadata", nil, &metadata); err != nil {
		// Older GitLab versions do not expose metadata. Permission and transport
		// errors are inconclusive; the group-hook API must still be checked.
		return false
	}
	return strings.EqualFold(strings.TrimSpace(metadata.Plan), "free") ||
		(metadata.Enterprise != nil && !*metadata.Enterprise)
}

func (c *gitlabAPI) canManageGroupHooks(ctx context.Context, groupID int64) (bool, error) {
	if c.currentUserID <= 0 {
		if _, err := c.currentUser(ctx); err != nil {
			return false, fmt.Errorf("read GitLab identity before checking group Hook permissions: %w", err)
		}
	}
	if c.currentUserID <= 0 {
		return false, errors.New("GitLab user identity is missing")
	}
	if c.currentUserIsAdmin {
		return true, nil
	}
	var member struct {
		AccessLevel *int `json:"access_level"`
	}
	path := fmt.Sprintf("/groups/%d/members/all/%d", groupID, c.currentUserID)
	if _, err := c.request(ctx, http.MethodGet, path, nil, &member); err != nil {
		return false, err
	}
	if member.AccessLevel == nil {
		return false, errors.New("GitLab did not return the effective group access level")
	}
	return *member.AccessLevel >= 50, nil
}

func (c *gitlabAPI) groupHooksUnsupportedForGroup(ctx context.Context, groupID int64, hookErr error) (bool, error) {
	if !isGitLabNotFound(hookErr) {
		return false, nil
	}
	canManage, err := c.canManageGroupHooks(ctx, groupID)
	if err != nil {
		return false, fmt.Errorf("group Hook API returned HTTP 404 but access could not be confirmed: %w", err)
	}
	if !canManage {
		return false, errors.New("group Hook API returned HTTP 404 and this connection is not a confirmed group Owner or instance administrator")
	}
	return true, nil
}

func (c *gitlabAPI) getProject(ctx context.Context, projectID int64) (map[string]any, error) {
	var project map[string]any
	_, err := c.request(ctx, http.MethodGet, "/projects/"+strconv.FormatInt(projectID, 10), nil, &project)
	return project, err
}

func (c *gitlabAPI) getHook(ctx context.Context, scopeType string, scopeID, hookID int64) (map[string]any, error) {
	path, err := hookPath(scopeType, scopeID, hookID)
	if err != nil {
		return nil, err
	}
	var hook map[string]any
	_, err = c.request(ctx, http.MethodGet, path, nil, &hook)
	return hook, err
}

func (c *gitlabAPI) updateHook(ctx context.Context, scopeType string, scopeID, hookID int64, body map[string]any) (map[string]any, error) {
	path, err := hookPath(scopeType, scopeID, hookID)
	if err != nil {
		return nil, err
	}
	var hook map[string]any
	_, err = c.request(ctx, http.MethodPut, path, body, &hook)
	return hook, err
}

func (c *gitlabAPI) createProjectHook(ctx context.Context, projectID int64, body map[string]any) (map[string]any, error) {
	var hook map[string]any
	_, err := c.request(ctx, http.MethodPost, "/projects/"+strconv.FormatInt(projectID, 10)+"/hooks", body, &hook)
	return hook, err
}

func (c *gitlabAPI) createHook(ctx context.Context, scopeType string, scopeID int64, body map[string]any) (map[string]any, error) {
	path, err := hooksCollectionPath(scopeType, scopeID)
	if err != nil {
		return nil, err
	}
	var hook map[string]any
	_, err = c.request(ctx, http.MethodPost, path, body, &hook)
	return hook, err
}

func (c *gitlabAPI) deleteHook(ctx context.Context, scopeType string, scopeID, hookID int64) error {
	path, err := hookPath(scopeType, scopeID, hookID)
	if err != nil {
		return err
	}
	_, err = c.request(ctx, http.MethodDelete, path, nil, nil)
	return err
}

func hookPath(scopeType string, scopeID, hookID int64) (string, error) {
	if scopeID <= 0 || hookID <= 0 {
		return "", errors.New("GitLab Hook identity is invalid")
	}
	switch scopeType {
	case "project":
		return fmt.Sprintf("/projects/%d/hooks/%d", scopeID, hookID), nil
	case "group":
		return fmt.Sprintf("/groups/%d/hooks/%d", scopeID, hookID), nil
	case "system":
		return fmt.Sprintf("/hooks/%d", hookID), nil
	default:
		return "", fmt.Errorf("unsupported GitLab Hook scope %q", scopeType)
	}
}

func (c *gitlabAPI) listHooks(ctx context.Context, scopeType string, scopeID int64) ([]map[string]any, error) {
	path, err := hooksCollectionPath(scopeType, scopeID)
	if err != nil {
		return nil, err
	}
	var all []map[string]any
	for page := 1; page <= 1000; page++ {
		var current []map[string]any
		header, err := c.request(ctx, http.MethodGet, path+"?per_page=100&page="+strconv.Itoa(page), nil, &current)
		if err != nil {
			return nil, err
		}
		all = append(all, current...)
		if strings.TrimSpace(header.Get("X-Next-Page")) == "" {
			return all, nil
		}
	}
	return nil, errors.New("GitLab Hook listing exceeded the pagination limit")
}

func hooksCollectionPath(scopeType string, scopeID int64) (string, error) {
	switch scopeType {
	case "project":
		if scopeID <= 0 {
			return "", errors.New("GitLab project identity is invalid")
		}
		return fmt.Sprintf("/projects/%d/hooks", scopeID), nil
	case "group":
		if scopeID <= 0 {
			return "", errors.New("GitLab group identity is invalid")
		}
		return fmt.Sprintf("/groups/%d/hooks", scopeID), nil
	case "system":
		return "/hooks", nil
	default:
		return "", fmt.Errorf("unsupported GitLab Hook scope %q", scopeType)
	}
}

func (c *gitlabAPI) listGroups(ctx context.Context) ([]map[string]any, error) {
	var all []map[string]any
	for page := 1; page <= 1000; page++ {
		var current []map[string]any
		header, err := c.request(ctx, http.MethodGet, "/groups?all_available=true&per_page=100&page="+strconv.Itoa(page), nil, &current)
		if err != nil {
			return nil, err
		}
		all = append(all, current...)
		if strings.TrimSpace(header.Get("X-Next-Page")) == "" {
			return all, nil
		}
	}
	return nil, errors.New("GitLab group listing exceeded the pagination limit")
}

func jsonSafeHookConfig(hook map[string]any) (json.RawMessage, error) {
	filtered := make(map[string]any, len(hook))
	for key, value := range hook {
		lower := strings.ToLower(key)
		if strings.Contains(lower, "token") || strings.Contains(lower, "secret") || strings.Contains(lower, "header") {
			continue
		}
		filtered[key] = value
	}
	encoded, err := json.Marshal(filtered)
	if err != nil {
		return nil, err
	}
	return encoded, nil
}

func hookUpdateBody(hook map[string]any, targetURL string) map[string]any {
	body := map[string]any{"url": targetURL}
	for _, key := range []string{
		"merge_requests_events", "job_events", "pipeline_events", "note_events", "confidential_note_events", "wiki_page_events",
		"deployment_events", "feature_flag_events", "releases_events", "subgroup_events",
		"enable_ssl_verification", "push_events_branch_filter",
	} {
		if value, ok := hook[key]; ok {
			body[key] = value
		}
	}
	return body
}

func hookID(hook map[string]any) (int64, error) {
	value, ok := hook["id"]
	if !ok {
		return 0, errors.New("GitLab Hook response has no ID")
	}
	switch id := value.(type) {
	case float64:
		if id <= 0 || id != float64(int64(id)) {
			return 0, errors.New("GitLab Hook response has an invalid ID")
		}
		return int64(id), nil
	case json.Number:
		return strconv.ParseInt(string(id), 10, 64)
	default:
		return 0, errors.New("GitLab Hook response has an invalid ID")
	}
}

func hookURL(hook map[string]any) string {
	value, _ := hook["url"].(string)
	return value
}

func hookEnabled(hook map[string]any, event string) bool {
	value, ok := hook[event].(bool)
	return ok && value
}
