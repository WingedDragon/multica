package gitlab

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

// ListGroupProjects returns projects visible in a GitLab group and its nested
// subgroups. It follows GitLab's X-Next-Page header until the final page.
func (c *Client) ListGroupProjects(ctx context.Context, groupPath string) ([]Project, error) {
	if strings.TrimSpace(groupPath) == "" {
		return nil, fmt.Errorf("gitlab group path is required")
	}

	path := "/api/v4/groups/" + url.PathEscape(groupPath) + "/projects"
	query := url.Values{
		"include_subgroups": {"true"},
		"with_shared":       {"false"},
		"per_page":          {"100"},
	}
	projects := make([]Project, 0)
	page := 1
	for {
		query.Set("page", strconv.Itoa(page))
		resp, err := c.doRaw(ctx, http.MethodGet, path+"?"+query.Encode(), nil)
		if err != nil {
			return nil, err
		}

		var pageProjects []Project
		decodeErr := json.NewDecoder(resp.Body).Decode(&pageProjects)
		nextPage := strings.TrimSpace(resp.Header.Get("X-Next-Page"))
		_ = resp.Body.Close()
		if decodeErr != nil {
			return nil, fmt.Errorf("decode GitLab group projects page %d: %w", page, decodeErr)
		}
		projects = append(projects, pageProjects...)

		if nextPage == "" {
			return projects, nil
		}
		next, err := strconv.Atoi(nextPage)
		if err != nil || next <= page {
			return nil, fmt.Errorf("GitLab returned invalid next page %q", nextPage)
		}
		page = next
	}
}
