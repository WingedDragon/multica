package gitlab

import (
	"net/url"
	"os"
	"strings"
)

type Config struct {
	BaseURL       string
	Host          string
	Token         string
	WebhookSecret string
	ProxyURL      string
}

func LoadConfigFromEnv() Config {
	base := strings.TrimRight(strings.TrimSpace(os.Getenv("GITLAB_BASE_URL")), "/")
	host := ""
	if u, err := url.Parse(base); err == nil {
		host = strings.ToLower(u.Host)
	}

	return Config{
		BaseURL:       base,
		Host:          host,
		Token:         strings.TrimSpace(os.Getenv("GITLAB_TOKEN")),
		WebhookSecret: strings.TrimSpace(os.Getenv("GITLAB_WEBHOOK_SECRET")),
		ProxyURL:      strings.TrimSpace(os.Getenv("GITLAB_PROXY_URL")),
	}
}

func (c Config) Configured() bool {
	return c.BaseURL != "" && c.Token != "" && c.WebhookSecret != ""
}
