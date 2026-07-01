package gitlab

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestLoadConfigFromEnv(t *testing.T) {
	t.Setenv("GITLAB_BASE_URL", "https://code.mlamp.cn/")
	t.Setenv("GITLAB_TOKEN", "secret-token")
	t.Setenv("GITLAB_WEBHOOK_SECRET", "hook-secret")
	t.Setenv("GITLAB_PROXY_URL", "socks5h://127.0.0.1:18080")

	cfg := LoadConfigFromEnv()
	if cfg.BaseURL != "https://code.mlamp.cn" {
		t.Fatalf("BaseURL = %q", cfg.BaseURL)
	}
	if !cfg.Configured() {
		t.Fatal("expected configured")
	}
	if cfg.ProxyURL != "socks5h://127.0.0.1:18080" {
		t.Fatalf("ProxyURL = %q", cfg.ProxyURL)
	}
}

func TestNewClientRejectsUnsupportedProxyScheme(t *testing.T) {
	_, err := NewClient(Config{
		BaseURL:  "https://code.mlamp.cn",
		Token:    "token",
		ProxyURL: "ftp://127.0.0.1:18080",
	})
	if err == nil || !strings.Contains(err.Error(), "unsupported gitlab proxy scheme") {
		t.Fatalf("error = %v", err)
	}
}

func TestNewClientRejectsInvalidBaseURL(t *testing.T) {
	tests := []struct {
		name    string
		baseURL string
		wantErr string
	}{
		{
			name:    "missing scheme",
			baseURL: "code.mlamp.cn",
			wantErr: "invalid gitlab base url",
		},
		{
			name:    "parse error",
			baseURL: "://bad",
			wantErr: "invalid gitlab base url",
		},
		{
			name:    "unsupported scheme",
			baseURL: "ftp://code.mlamp.cn",
			wantErr: "unsupported gitlab base url scheme",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := NewClient(Config{BaseURL: tt.baseURL, Token: "token"})
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("error = %v, want substring %q", err, tt.wantErr)
			}
		})
	}
}

func TestGetProjectEncodesPathWithNamespace(t *testing.T) {
	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.EscapedPath()
		if got := r.Header.Get("PRIVATE-TOKEN"); got != "token" {
			t.Fatalf("PRIVATE-TOKEN = %q", got)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"id":42,"path_with_namespace":"group/repo","web_url":"https://code.mlamp.cn/group/repo"}`))
	}))
	defer srv.Close()

	c, err := NewClient(Config{BaseURL: srv.URL, Token: "token"})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	project, err := c.GetProject(context.Background(), "group/repo")
	if err != nil {
		t.Fatalf("GetProject: %v", err)
	}
	if gotPath != "/api/v4/projects/group%2Frepo" {
		t.Fatalf("path = %q", gotPath)
	}
	if project.ID != 42 || project.PathWithNamespace != "group/repo" {
		t.Fatalf("project = %+v", project)
	}
}

func TestClientIgnoresProcessProxyEnvWithoutConfiguredProxy(t *testing.T) {
	t.Setenv("HTTP_PROXY", "http://127.0.0.1:1")
	t.Setenv("HTTPS_PROXY", "http://127.0.0.1:1")

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"id":42,"path_with_namespace":"group/repo","web_url":"https://code.mlamp.cn/group/repo"}`))
	}))
	defer srv.Close()

	c, err := NewClient(Config{BaseURL: srv.URL, Token: "token"})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	tr, ok := c.httpClient.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("transport = %T", c.httpClient.Transport)
	}
	if tr.Proxy != nil {
		t.Fatal("expected transport proxy to be nil")
	}

	project, err := c.GetProject(context.Background(), "group/repo")
	if err != nil {
		t.Fatalf("GetProject: %v", err)
	}
	if project.ID != 42 || project.PathWithNamespace != "group/repo" {
		t.Fatalf("project = %+v", project)
	}
}
