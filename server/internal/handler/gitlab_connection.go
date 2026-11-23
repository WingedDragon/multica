package handler

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/integrations/gitlab"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

func (h *Handler) CreateGitLabConnection(w http.ResponseWriter, r *http.Request) {
	workspaceID := chi.URLParam(r, "id")
	wsUUID, ok := parseUUIDOrBadRequest(w, workspaceID, "workspace id")
	if !ok {
		return
	}
	if h.VCSSecretBox == nil {
		writeFeatureDisabled(w, "vcs_not_configured", "GitLab credential storage is not configured (MULTICA_VCS_SECRET_KEY unset)")
		return
	}

	var req createGitLabConnectionRequest
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	baseURL, host, ok := normalizeGitLabConnectionBaseURL(req.BaseURL)
	if !ok || strings.TrimSpace(req.AccessToken) == "" {
		writeError(w, http.StatusBadRequest, "base_url must be an absolute http(s) URL and access_token is required")
		return
	}

	token := strings.TrimSpace(req.AccessToken)
	client, err := newGitLabClient(gitlab.Config{
		BaseURL:  baseURL,
		Host:     host,
		Token:    token,
		ProxyURL: gitlab.LoadConfigFromEnv().ProxyURL,
	})
	if err != nil {
		writeError(w, http.StatusBadGateway, "failed to initialize GitLab client")
		return
	}
	if err := client.ValidateCredentials(r.Context()); err != nil {
		if errors.Is(err, gitlab.ErrUnauthorized) {
			writeError(w, http.StatusBadRequest, "GitLab rejected the access token")
		} else {
			writeError(w, http.StatusBadGateway, "failed to validate GitLab credentials")
		}
		return
	}
	tokenEncrypted, err := h.sealVCSSecret(token)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to encrypt GitLab access token")
		return
	}
	webhookSecret, err := newVCSWebhookSecret()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to generate GitLab webhook secret")
		return
	}
	webhookSecretEncrypted, err := h.sealVCSSecret(webhookSecret)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to encrypt GitLab webhook secret")
		return
	}

	connectedBy := pgtype.UUID{}
	if member, ok := ctxMember(r.Context()); ok {
		connectedBy = member.UserID
	} else if userID := requestUserID(r); userID != "" {
		if u, err := parseStrictUUID(userID); err == nil {
			connectedBy = u
		}
	}
	if h.TxStarter == nil {
		writeError(w, http.StatusInternalServerError, "database transactions are unavailable")
		return
	}
	tx, err := h.TxStarter.Begin(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to save GitLab connection")
		return
	}
	defer tx.Rollback(r.Context())
	q := h.Queries.WithTx(tx)
	connection, err := q.UpsertGitLabConnection(r.Context(), db.UpsertGitLabConnectionParams{
		WorkspaceID:   wsUUID,
		BaseUrl:       baseURL,
		Host:          host,
		ConnectedByID: connectedBy,
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			writeError(w, http.StatusConflict, "a GitLab connection already exists for this host with a different base URL")
		} else {
			writeError(w, http.StatusInternalServerError, "failed to save GitLab connection")
		}
		return
	}
	if connection.WebhookSecretEncrypted == "" {
		hasProjects, err := q.GitLabConnectionHasProjectBindings(r.Context(), db.GitLabConnectionHasProjectBindingsParams{
			WorkspaceID:  wsUUID,
			ConnectionID: connection.ID,
		})
		if err != nil {
			writeError(w, http.StatusInternalServerError, "failed to check existing GitLab projects")
			return
		}
		if hasProjects {
			writeError(w, http.StatusConflict, "this GitLab connection already has projects using the deployment webhook; remove or migrate those project hooks before connecting it with a per-instance webhook")
			return
		}
	}
	stored, err := q.StoreGitLabConnectionCredentials(r.Context(), db.StoreGitLabConnectionCredentialsParams{
		ID:                     connection.ID,
		WorkspaceID:            wsUUID,
		AccessTokenEncrypted:   tokenEncrypted,
		WebhookSecretEncrypted: webhookSecretEncrypted,
	})
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to save GitLab credentials")
		return
	}
	if err := tx.Commit(r.Context()); err != nil {
		writeError(w, http.StatusInternalServerError, "failed to save GitLab connection")
		return
	}

	connectionID := uuidToString(stored.ID)
	response := map[string]any{
		"connection": GitLabConnectionResponse{
			ID:          connectionID,
			WorkspaceID: uuidToString(stored.WorkspaceID),
			BaseURL:     stored.BaseUrl,
			Host:        stored.Host,
		},
		"webhook_url":  h.gitLabWebhookURL(connectionID),
		"webhook_path": h.gitLabWebhookPath(connectionID),
	}
	if stored.WebhookSecretCreated {
		response["webhook_secret"] = webhookSecret
	}
	writeJSON(w, http.StatusOK, response)
}

func (h *Handler) gitLabConfigForConnection(ctx context.Context, workspaceID, connectionID pgtype.UUID) (gitlab.Config, error) {
	connection, err := h.Queries.GetGitLabConnectionByID(ctx, db.GetGitLabConnectionByIDParams{
		ID:          connectionID,
		WorkspaceID: workspaceID,
	})
	if err != nil {
		return gitlab.Config{}, err
	}
	return h.gitLabConfigForConnectionRow(connection)
}

func gitLabConnectionServerConfigured(connection db.GitlabConnection) bool {
	env := gitlab.LoadConfigFromEnv()
	return connection.AccessTokenEncrypted == "" &&
		connection.WebhookSecretEncrypted == "" &&
		env.Configured() &&
		env.BaseURL == strings.TrimRight(connection.BaseUrl, "/") &&
		strings.EqualFold(env.Host, connection.Host)
}

func (h *Handler) gitLabConfigForConnectionRow(connection db.GitlabConnection) (gitlab.Config, error) {
	if h.VCSSecretBox == nil && (connection.AccessTokenEncrypted != "" || connection.WebhookSecretEncrypted != "") {
		return gitlab.Config{}, errors.New("GitLab connection credentials are unavailable")
	}
	env := gitlab.LoadConfigFromEnv()
	config := gitlab.Config{
		BaseURL:  connection.BaseUrl,
		Host:     connection.Host,
		ProxyURL: env.ProxyURL,
	}
	envMatches := env.BaseURL == strings.TrimRight(connection.BaseUrl, "/") && strings.EqualFold(env.Host, connection.Host)
	if connection.AccessTokenEncrypted != "" {
		var err error
		config.Token, err = h.openVCSSecret(connection.AccessTokenEncrypted)
		if err != nil {
			return gitlab.Config{}, errors.New("cannot decrypt GitLab connection credentials")
		}
	} else if envMatches {
		config.Token = env.Token
	}
	if connection.WebhookSecretEncrypted != "" {
		var err error
		config.WebhookSecret, err = h.openVCSSecret(connection.WebhookSecretEncrypted)
		if err != nil {
			return gitlab.Config{}, errors.New("cannot decrypt GitLab webhook credentials")
		}
	} else if envMatches {
		config.WebhookSecret = env.WebhookSecret
	}
	if config.Token == "" {
		return gitlab.Config{}, errors.New("GitLab connection credentials are unavailable")
	}
	return config, nil
}

func (h *Handler) gitLabWebhookConnectionConfig(ctx context.Context, connectionID pgtype.UUID) (db.GitlabConnection, gitlab.Config, error) {
	connection, err := h.Queries.GetGitLabConnectionForWebhook(ctx, connectionID)
	if err != nil {
		return db.GitlabConnection{}, gitlab.Config{}, err
	}
	if h.VCSSecretBox == nil {
		return db.GitlabConnection{}, gitlab.Config{}, errors.New("GitLab webhook credentials are unavailable")
	}
	config, err := h.gitLabConfigForConnectionRow(connection)
	if err != nil {
		return db.GitlabConnection{}, gitlab.Config{}, err
	}
	if config.WebhookSecret == "" {
		return db.GitlabConnection{}, gitlab.Config{}, errors.New("GitLab webhook credentials are unavailable")
	}
	return connection, config, nil
}

func gitLabConnectionNotFound(err error) bool {
	return errors.Is(err, pgx.ErrNoRows)
}

func (h *Handler) gitLabConfigForMergeRequest(ctx context.Context, mr db.GitlabMergeRequest) (gitlab.Config, error) {
	binding, err := h.Queries.GetGitLabProjectBinding(ctx, db.GetGitLabProjectBindingParams{
		ID:          mr.ProjectBindingID,
		WorkspaceID: mr.WorkspaceID,
	})
	if err != nil {
		return gitlab.Config{}, err
	}
	return h.gitLabConfigForConnection(ctx, mr.WorkspaceID, binding.ConnectionID)
}
