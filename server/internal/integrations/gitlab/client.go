package gitlab

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"reflect"
	"strconv"
	"strings"
	"time"

	xproxy "golang.org/x/net/proxy"
)

const (
	gitLabMaxPages         = 10
	gitLabPageSize         = 100
	gitLabMaxTraceReadSize = 1 << 20
)

type Client struct {
	cfg        Config
	httpClient *http.Client
}

func NewClient(cfg Config) (*Client, error) {
	baseURL := strings.TrimRight(strings.TrimSpace(cfg.BaseURL), "/")
	if baseURL == "" {
		return nil, errors.New("gitlab base url is required")
	}
	u, err := url.Parse(baseURL)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return nil, fmt.Errorf("invalid gitlab base url %q", cfg.BaseURL)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return nil, fmt.Errorf("unsupported gitlab base url scheme %q", u.Scheme)
	}
	cfg.BaseURL = baseURL

	tr := &http.Transport{Proxy: nil}
	if cfg.ProxyURL != "" {
		u, err := url.Parse(cfg.ProxyURL)
		if err != nil {
			return nil, fmt.Errorf("parse gitlab proxy url: %w", err)
		}
		switch u.Scheme {
		case "socks5", "socks5h":
			dialer, err := xproxy.SOCKS5("tcp", u.Host, nil, xproxy.Direct)
			if err != nil {
				return nil, fmt.Errorf("create gitlab socks proxy dialer: %w", err)
			}
			tr.DialContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
				type contextDialer interface {
					DialContext(context.Context, string, string) (net.Conn, error)
				}
				if d, ok := dialer.(contextDialer); ok {
					return d.DialContext(ctx, network, addr)
				}
				return dialer.Dial(network, addr)
			}
		case "http", "https":
			tr.Proxy = http.ProxyURL(u)
		default:
			return nil, fmt.Errorf("unsupported gitlab proxy scheme %q", u.Scheme)
		}
	}

	return &Client{
		cfg: cfg,
		httpClient: &http.Client{
			Timeout:   15 * time.Second,
			Transport: tr,
		},
	}, nil
}

func (c *Client) GetProject(ctx context.Context, projectPath string) (Project, error) {
	var out Project
	err := c.doJSON(ctx, http.MethodGet, "/api/v4/projects/"+url.PathEscape(projectPath), nil, &out)
	return out, err
}

func (c *Client) GetMergeRequest(ctx context.Context, projectID int64, iid int32) (MergeRequest, error) {
	var out MergeRequest
	err := c.doJSON(ctx, http.MethodGet, fmt.Sprintf("/api/v4/projects/%d/merge_requests/%d", projectID, iid), nil, &out)
	return out, err
}

func (c *Client) MergeMergeRequest(ctx context.Context, projectID int64, iid int32, opts MergeRequestMergeOptions) (MergeRequest, error) {
	var out MergeRequest
	err := c.doJSON(ctx, http.MethodPut, fmt.Sprintf("/api/v4/projects/%d/merge_requests/%d/merge", projectID, iid), opts, &out)
	return out, err
}

func (c *Client) GetMergeRequestChanges(ctx context.Context, projectID int64, iid int32) (MergeRequestChanges, error) {
	var out MergeRequestChanges
	err := c.doJSON(ctx, http.MethodGet, fmt.Sprintf("/api/v4/projects/%d/merge_requests/%d/changes", projectID, iid), nil, &out)
	return out, err
}

func (c *Client) GetMergeRequestApprovalState(ctx context.Context, projectID int64, iid int32) (ApprovalState, error) {
	var out ApprovalState
	if err := c.doJSON(ctx, http.MethodGet, fmt.Sprintf("/api/v4/projects/%d/merge_requests/%d/approvals", projectID, iid), nil, &out); err != nil {
		return out, err
	}
	var rules struct {
		Rules []ApprovalRule `json:"rules"`
	}
	if err := c.doJSON(ctx, http.MethodGet, fmt.Sprintf("/api/v4/projects/%d/merge_requests/%d/approval_state", projectID, iid), nil, &rules); err == nil {
		out.Rules = rules.Rules
	}
	return out, nil
}

func (c *Client) ListMergeRequestDiscussions(ctx context.Context, projectID int64, iid int32) ([]Discussion, error) {
	var out []Discussion
	err := c.doPaginatedJSON(ctx, fmt.Sprintf("/api/v4/projects/%d/merge_requests/%d/discussions", projectID, iid), nil, &out)
	return out, err
}

func (c *Client) ListProjectPipelines(ctx context.Context, projectID int64, filters PipelineFilters) ([]Pipeline, error) {
	values := url.Values{}
	if filters.SHA != "" {
		values.Set("sha", filters.SHA)
	}
	if filters.Ref != "" {
		values.Set("ref", filters.Ref)
	}
	var out []Pipeline
	err := c.doPaginatedJSON(ctx, fmt.Sprintf("/api/v4/projects/%d/pipelines", projectID), values, &out)
	return out, err
}

func (c *Client) GetPipeline(ctx context.Context, projectID, pipelineID int64) (Pipeline, error) {
	var out Pipeline
	err := c.doJSON(ctx, http.MethodGet, fmt.Sprintf("/api/v4/projects/%d/pipelines/%d", projectID, pipelineID), nil, &out)
	return out, err
}

func (c *Client) ListPipelineJobs(ctx context.Context, projectID, pipelineID int64) ([]Job, error) {
	var out []Job
	err := c.doPaginatedJSON(ctx, fmt.Sprintf("/api/v4/projects/%d/pipelines/%d/jobs", projectID, pipelineID), nil, &out)
	return out, err
}

func (c *Client) GetJobTrace(ctx context.Context, projectID, jobID int64, limit int) (JobTrace, error) {
	if limit <= 0 {
		limit = 4096
	}
	resp, err := c.doRaw(ctx, http.MethodGet, fmt.Sprintf("/api/v4/projects/%d/jobs/%d/trace", projectID, jobID), nil)
	if err != nil {
		return JobTrace{}, err
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(io.LimitReader(resp.Body, gitLabMaxTraceReadSize+1))
	if err != nil {
		return JobTrace{}, err
	}
	truncated := len(raw) > limit
	if len(raw) > gitLabMaxTraceReadSize {
		truncated = true
		raw = raw[:gitLabMaxTraceReadSize]
	}
	if len(raw) > limit {
		raw = raw[len(raw)-limit:]
	}
	return JobTrace{Text: string(raw), Truncated: truncated}, nil
}

func (c *Client) DownloadJobArtifacts(ctx context.Context, projectID, jobID int64) (*http.Response, error) {
	return c.doRaw(ctx, http.MethodGet, fmt.Sprintf("/api/v4/projects/%d/jobs/%d/artifacts", projectID, jobID), nil)
}

func (c *Client) CreateProjectHook(ctx context.Context, projectID int64, webhookURL, secret string) (ProjectHook, error) {
	body := map[string]any{
		"url":                   webhookURL,
		"token":                 secret,
		"merge_requests_events": true,
		"pipeline_events":       true,
		"job_events":            true,
		"note_events":           true,
	}
	var out ProjectHook
	err := c.doJSON(ctx, http.MethodPost, fmt.Sprintf("/api/v4/projects/%d/hooks", projectID), body, &out)
	return out, err
}

func (c *Client) DeleteProjectHook(ctx context.Context, projectID, hookID int64) error {
	return c.doJSON(ctx, http.MethodDelete, fmt.Sprintf("/api/v4/projects/%d/hooks/%d", projectID, hookID), nil, nil)
}

func (c *Client) doPaginatedJSON(ctx context.Context, path string, query url.Values, out any) error {
	outValue := reflect.ValueOf(out)
	if outValue.Kind() != reflect.Ptr || outValue.Elem().Kind() != reflect.Slice {
		return errors.New("gitlab pagination output must be a pointer to slice")
	}
	sliceValue := outValue.Elem()
	pageType := reflect.SliceOf(sliceValue.Type().Elem())

	if query == nil {
		query = url.Values{}
	} else {
		query = cloneValues(query)
	}
	for page := 1; page <= gitLabMaxPages; page++ {
		query.Set("page", strconv.Itoa(page))
		query.Set("per_page", strconv.Itoa(gitLabPageSize))
		pageItems := reflect.New(pageType)
		resp, err := c.doRaw(ctx, http.MethodGet, path+"?"+query.Encode(), nil)
		if err != nil {
			return err
		}
		if err := json.NewDecoder(resp.Body).Decode(pageItems.Interface()); err != nil {
			_ = resp.Body.Close()
			return err
		}
		nextPage := strings.TrimSpace(resp.Header.Get("X-Next-Page"))
		_ = resp.Body.Close()
		sliceValue.Set(reflect.AppendSlice(sliceValue, pageItems.Elem()))
		if nextPage == "" {
			return nil
		}
		next, err := strconv.Atoi(nextPage)
		if err != nil || next <= page {
			return fmt.Errorf("gitlab pagination returned invalid next page %q", nextPage)
		}
		page = next - 1
	}
	return errors.New("gitlab pagination limit exceeded")
}

func (c *Client) doJSON(ctx context.Context, method, path string, in any, out any) error {
	resp, err := c.doRaw(ctx, method, path, in)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if out == nil {
		return nil
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

func (c *Client) doRaw(ctx context.Context, method, path string, in any) (*http.Response, error) {
	var body io.Reader
	if in != nil {
		var buf bytes.Buffer
		if err := json.NewEncoder(&buf).Encode(in); err != nil {
			return nil, err
		}
		body = &buf
	}

	req, err := http.NewRequestWithContext(ctx, method, strings.TrimRight(c.cfg.BaseURL, "/")+path, body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("PRIVATE-TOKEN", c.cfg.Token)
	req.Header.Set("Accept", "application/json")
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("gitlab unreachable: %w", err)
	}

	if resp.StatusCode >= 400 {
		snippet, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		_ = resp.Body.Close()
		switch resp.StatusCode {
		case http.StatusUnauthorized:
			return nil, errors.New("gitlab token invalid")
		case http.StatusForbidden:
			return nil, errors.New("gitlab token lacks required project permission")
		case http.StatusNotFound:
			return nil, errors.New("gitlab resource not found")
		case http.StatusTooManyRequests:
			return nil, errors.New("gitlab rate limited")
		default:
			return nil, fmt.Errorf("gitlab api error %d: %s", resp.StatusCode, strings.TrimSpace(string(snippet)))
		}
	}

	return resp, nil
}

func cloneValues(values url.Values) url.Values {
	out := make(url.Values, len(values))
	for k, v := range values {
		out[k] = append([]string(nil), v...)
	}
	return out
}
