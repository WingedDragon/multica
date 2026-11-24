package gitlabmigration

import (
	"encoding/base64"
	"errors"
	"fmt"
	"net/url"
	"os"
	"strings"

	"github.com/multica-ai/multica/server/internal/util/secretbox"
)

type Environment struct {
	GitLabBaseURL         string
	GitLabToken           string
	GitLabWebhookSecret   string
	GitLabProxyURL        string
	VCSIntegrationEnabled bool
	VCSSecretKey          string
	PublicURL             string
}

func EnvironmentFromOS() Environment {
	return Environment{
		GitLabBaseURL:         strings.TrimRight(strings.TrimSpace(os.Getenv("GITLAB_BASE_URL")), "/"),
		GitLabToken:           strings.TrimSpace(os.Getenv("GITLAB_TOKEN")),
		GitLabWebhookSecret:   strings.TrimSpace(os.Getenv("GITLAB_WEBHOOK_SECRET")),
		GitLabProxyURL:        strings.TrimSpace(os.Getenv("GITLAB_PROXY_URL")),
		VCSIntegrationEnabled: os.Getenv("MULTICA_VCS_INTEGRATION_ENABLED") == "true",
		VCSSecretKey:          strings.TrimSpace(os.Getenv("MULTICA_VCS_SECRET_KEY")),
		PublicURL:             strings.TrimRight(strings.TrimSpace(os.Getenv("MULTICA_PUBLIC_URL")), "/"),
	}
}

func (e Environment) credentialsFor(instanceURL string) (string, string, error) {
	if normalizeInstanceURL(e.GitLabBaseURL) != normalizeInstanceURL(instanceURL) || e.GitLabToken == "" || e.GitLabWebhookSecret == "" {
		return "", "", fmt.Errorf("legacy GitLab credentials are missing for exact instance %q", safeInstanceURL(instanceURL))
	}
	return e.GitLabToken, e.GitLabWebhookSecret, nil
}

func (e Environment) legacyCredentials(box *secretbox.Box, baseURL, encryptedToken, encryptedSecret string) (string, string, string, error) {
	token, secret := "", ""
	if encryptedToken != "" {
		value, err := openSecret(box, encryptedToken)
		if err != nil {
			return "", "", "", fmt.Errorf("legacy GitLab access token for %q could not be decrypted", safeInstanceURL(baseURL))
		}
		token = value
	}
	if encryptedSecret != "" {
		value, err := openSecret(box, encryptedSecret)
		if err != nil {
			return "", "", "", fmt.Errorf("legacy GitLab webhook secret for %q could not be decrypted", safeInstanceURL(baseURL))
		}
		secret = value
	}
	if token == "" || secret == "" {
		if normalizeInstanceURL(e.GitLabBaseURL) != normalizeInstanceURL(baseURL) {
			return "", "", "", fmt.Errorf("legacy GitLab credentials are incomplete and no exact-instance environment credentials are configured for %q", safeInstanceURL(baseURL))
		}
		if token == "" {
			token = e.GitLabToken
		}
		if secret == "" {
			secret = e.GitLabWebhookSecret
		}
	}
	if token == "" || secret == "" {
		return "", "", "", fmt.Errorf("legacy GitLab credentials are incomplete for %q", safeInstanceURL(baseURL))
	}
	if encryptedSecret == "" {
		sealed, err := sealSecret(box, secret)
		if err != nil {
			return "", "", "", err
		}
		encryptedSecret = sealed
	}
	return token, secret, encryptedSecret, nil
}

func (e Environment) box() (*secretbox.Box, error) {
	if e.VCSSecretKey == "" {
		return nil, errors.New("MULTICA_VCS_SECRET_KEY is required to encrypt migrated credentials")
	}
	key, err := base64.StdEncoding.DecodeString(e.VCSSecretKey)
	if err != nil || len(key) != secretbox.KeySize {
		return nil, errors.New("MULTICA_VCS_SECRET_KEY must be base64-encoded 32-byte key material")
	}
	box, err := secretbox.New(key)
	if err != nil {
		return nil, errors.New("MULTICA_VCS_SECRET_KEY is invalid")
	}
	return box, nil
}
func (e Environment) SecretBox() (*secretbox.Box, error) {
	return e.box()
}

func sealSecret(box *secretbox.Box, value string) (string, error) {
	sealed, err := box.Seal([]byte(value))
	if err != nil {
		return "", errors.New("failed to encrypt migrated GitLab credential")
	}
	return base64.StdEncoding.EncodeToString(sealed), nil
}

func openSecret(box *secretbox.Box, encoded string) (string, error) {
	ciphertext, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		return "", errors.New("stored VCS credential is not valid ciphertext")
	}
	plaintext, err := box.Open(ciphertext)
	if err != nil {
		return "", errors.New("stored VCS credential could not be decrypted")
	}
	return string(plaintext), nil
}

func normalizeInstanceURL(raw string) string {
	trimmed := strings.TrimRight(strings.TrimSpace(raw), "/")
	u, err := url.Parse(trimmed)
	if err != nil || u.Scheme == "" || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return trimmed
	}
	u.Scheme = strings.ToLower(u.Scheme)
	u.Host = strings.ToLower(u.Host)
	u.Path = strings.TrimRight(u.Path, "/")
	u.RawPath = ""
	return u.String()
}

func safeInstanceURL(raw string) string {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return "configured instance"
	}
	return strings.TrimRight(u.Scheme+"://"+u.Host+u.EscapedPath(), "/")
}
