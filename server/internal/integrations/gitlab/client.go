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
	"strings"
	"time"

	xproxy "golang.org/x/net/proxy"
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

func (c *Client) CreateProjectHook(ctx context.Context, projectID int64, webhookURL, secret string) (ProjectHook, error) {
	body := map[string]any{
		"url":                   webhookURL,
		"token":                 secret,
		"merge_requests_events": true,
		"pipeline_events":       true,
	}
	var out ProjectHook
	err := c.doJSON(ctx, http.MethodPost, fmt.Sprintf("/api/v4/projects/%d/hooks", projectID), body, &out)
	return out, err
}

func (c *Client) DeleteProjectHook(ctx context.Context, projectID, hookID int64) error {
	return c.doJSON(ctx, http.MethodDelete, fmt.Sprintf("/api/v4/projects/%d/hooks/%d", projectID, hookID), nil, nil)
}

func (c *Client) doJSON(ctx context.Context, method, path string, in any, out any) error {
	var body io.Reader
	if in != nil {
		var buf bytes.Buffer
		if err := json.NewEncoder(&buf).Encode(in); err != nil {
			return err
		}
		body = &buf
	}

	req, err := http.NewRequestWithContext(ctx, method, strings.TrimRight(c.cfg.BaseURL, "/")+path, body)
	if err != nil {
		return err
	}
	req.Header.Set("PRIVATE-TOKEN", c.cfg.Token)
	req.Header.Set("Accept", "application/json")
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("gitlab unreachable: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		snippet, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		switch resp.StatusCode {
		case http.StatusUnauthorized:
			return errors.New("gitlab token invalid")
		case http.StatusForbidden:
			return errors.New("gitlab token lacks required project permission")
		case http.StatusNotFound:
			return errors.New("gitlab project not found")
		default:
			return fmt.Errorf("gitlab api error %d: %s", resp.StatusCode, strings.TrimSpace(string(snippet)))
		}
	}

	if out == nil {
		return nil
	}
	return json.NewDecoder(resp.Body).Decode(out)
}
