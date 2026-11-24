package vcs

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	xproxy "golang.org/x/net/proxy"
)

const gitLabProviderHTTPTimeout = 15 * time.Second

var gitLabProviderHTTPClients sync.Map

// gitLabProviderHTTPClient returns a client dedicated to GitLab provider calls.
// It deliberately does not inherit process-wide HTTP_PROXY settings: the
// independent GitLab client is configured only by GITLAB_PROXY_URL.
func gitLabProviderHTTPClient() (*http.Client, error) {
	proxyURL := strings.TrimSpace(os.Getenv("GITLAB_PROXY_URL"))
	if cached, ok := gitLabProviderHTTPClients.Load(proxyURL); ok {
		return cached.(*http.Client), nil
	}
	client, err := newGitLabProviderHTTPClient(proxyURL)
	if err != nil {
		return nil, err
	}
	cached, loaded := gitLabProviderHTTPClients.LoadOrStore(proxyURL, client)
	if loaded {
		return cached.(*http.Client), nil
	}
	return client, nil
}

func newGitLabProviderHTTPClient(proxyURL string) (*http.Client, error) {
	transport := &http.Transport{Proxy: nil}
	if proxyURL != "" {
		proxyAddress, err := url.Parse(proxyURL)
		if err != nil {
			return nil, fmt.Errorf("parse gitlab proxy url: %w", err)
		}
		switch proxyAddress.Scheme {
		case "socks5", "socks5h":
			dialer, err := xproxy.SOCKS5("tcp", proxyAddress.Host, nil, xproxy.Direct)
			if err != nil {
				return nil, fmt.Errorf("create gitlab socks proxy dialer: %w", err)
			}
			transport.DialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
				type contextDialer interface {
					DialContext(context.Context, string, string) (net.Conn, error)
				}
				if d, ok := dialer.(contextDialer); ok {
					return d.DialContext(ctx, network, address)
				}
				return dialer.Dial(network, address)
			}
		case "http", "https":
			transport.Proxy = http.ProxyURL(proxyAddress)
		default:
			return nil, fmt.Errorf("unsupported gitlab proxy scheme %q", proxyAddress.Scheme)
		}
	}
	return &http.Client{Timeout: gitLabProviderHTTPTimeout, Transport: transport}, nil
}
