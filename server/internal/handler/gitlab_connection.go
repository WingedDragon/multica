package handler

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/integrations/gitlab"
	"github.com/multica-ai/multica/server/internal/integrations/vcs"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

var (
	errGitLabConnectionConflict        = errors.New("GitLab connection conflicts with another provider")
	errGitLabConnectionUnavailable     = errors.New("GitLab connection credentials are unavailable")
	errGitLabLegacyConnectionAmbiguous = errors.New("multiple legacy GitLab connections are available")
	errGitLabTokenValidation           = errors.New("GitLab token validation failed")
)

var validateGitLabVCSConnectionToken = func(ctx context.Context, instanceURL, token string) (vcs.Account, error) {
	provider, ok := vcs.For(string(vcs.KindGitLab))
	if !ok {
		return vcs.Account{}, errors.New("GitLab provider is unavailable")
	}
	return provider.ValidateToken(ctx, instanceURL, token)
}

func (h *Handler) CreateGitLabConnection(w http.ResponseWriter, r *http.Request) {
	workspaceID := chi.URLParam(r, "id")
	wsUUID, ok := parseUUIDOrBadRequest(w, workspaceID, "workspace id")
	if !ok {
		return
	}
	if !h.isVCSAvailable() {
		writeError(w, http.StatusNotFound, "vcs integration is not available on this deployment")
		return
	}
	if !h.isVCSConfigured() {
		writeFeatureDisabled(w, "vcs_not_configured", "vcs integration not configured (MULTICA_VCS_SECRET_KEY unset)")
		return
	}
	var req createGitLabConnectionRequest
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	baseURL, _, ok := normalizeGitLabConnectionBaseURL(req.BaseURL)
	if !ok || strings.TrimSpace(req.AccessToken) == "" {
		writeError(w, http.StatusBadRequest, "base_url must be an absolute http(s) URL and access_token is required")
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
	connection, webhookSecret, err := h.createGitLabVCSConnection(
		r.Context(), wsUUID, baseURL, strings.TrimSpace(req.AccessToken), connectedBy, "",
	)
	if err != nil {
		h.writeGitLabConnectionError(w, err)
		return
	}
	if err := h.Queries.EnsureLegacyGitLabVCSConnectionAlias(r.Context(), db.EnsureLegacyGitLabVCSConnectionAliasParams{
		WorkspaceID:  wsUUID,
		ConnectionID: connection.ID,
	}); err != nil {
		writeError(w, http.StatusInternalServerError, "failed to register legacy GitLab connection")
		return
	}
	connectionResponse := h.vcsGitLabConnectionToResponse(connection, true)
	response := map[string]any{
		"connection":   connectionResponse,
		"webhook_url":  connectionResponse.WebhookURL,
		"webhook_path": connectionResponse.WebhookPath,
	}
	if webhookSecret != "" {
		response["webhook_secret"] = webhookSecret
	}
	writeJSON(w, http.StatusOK, response)
}

func (h *Handler) resolveGitLabVCSConnection(ctx context.Context, workspaceID, connectionID pgtype.UUID) (db.VcsConnection, error) {
	canonicalID, err := h.Queries.GetGitLabVCSConnectionIDByLegacyID(ctx, db.GetGitLabVCSConnectionIDByLegacyIDParams{
		WorkspaceID: workspaceID,
		LegacyID:    connectionID,
	})
	if err == nil {
		connectionID = canonicalID
	} else if !errors.Is(err, pgx.ErrNoRows) {
		return db.VcsConnection{}, err
	}
	return h.Queries.GetVCSGitLabConnectionByIDAndWorkspace(ctx, db.GetVCSGitLabConnectionByIDAndWorkspaceParams{
		ID:          connectionID,
		WorkspaceID: workspaceID,
	})
}
func (h *Handler) resolveLegacyGitLabVCSConnection(ctx context.Context, workspaceID, requestedID pgtype.UUID) (db.VcsConnection, error) {
	if !requestedID.Valid {
		connections, err := h.Queries.ListLegacyGitLabVCSConnectionsByWorkspace(ctx, workspaceID)
		if err != nil {
			return db.VcsConnection{}, err
		}
		switch len(connections) {
		case 0:
			return db.VcsConnection{}, pgx.ErrNoRows
		case 1:
			return connections[0], nil
		default:
			return db.VcsConnection{}, errGitLabLegacyConnectionAmbiguous
		}
	}

	canonicalID, err := h.Queries.GetGitLabVCSConnectionIDByLegacyID(ctx, db.GetGitLabVCSConnectionIDByLegacyIDParams{
		WorkspaceID: workspaceID,
		LegacyID:    requestedID,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		canonicalID, err = h.Queries.GetLegacyGitLabVCSConnectionIDByCanonicalID(ctx, db.GetLegacyGitLabVCSConnectionIDByCanonicalIDParams{
			WorkspaceID:  workspaceID,
			ConnectionID: requestedID,
		})
	}
	if err != nil {
		return db.VcsConnection{}, err
	}
	return h.Queries.GetVCSGitLabConnectionByIDAndWorkspace(ctx, db.GetVCSGitLabConnectionByIDAndWorkspaceParams{
		ID:          canonicalID,
		WorkspaceID: workspaceID,
	})
}

func gitLabVCSConnectionServerConfigured(connection db.VcsConnection) bool {
	return connection.AccessTokenEncrypted != "" && connection.WebhookSecretEncrypted != ""
}

func (h *Handler) gitLabConfigForVCSConnectionRow(connection db.VcsConnection) (gitlab.Config, error) {
	if connection.Provider != string(vcs.KindGitLab) {
		return gitlab.Config{}, errors.New("connection is not a GitLab connection")
	}
	baseURL, host, ok := normalizeGitLabConnectionBaseURL(connection.InstanceUrl)
	if !ok {
		return gitlab.Config{}, errors.New("GitLab connection URL is invalid")
	}
	if connection.AccessTokenEncrypted == "" || connection.WebhookSecretEncrypted == "" || h.VCSSecretBox == nil {
		return gitlab.Config{}, errGitLabConnectionUnavailable
	}

	config := gitlab.Config{BaseURL: baseURL, Host: host, ProxyURL: gitlab.LoadConfigFromEnv().ProxyURL}
	var err error
	config.Token, err = h.openVCSSecret(connection.AccessTokenEncrypted)
	if err != nil {
		return gitlab.Config{}, errors.New("cannot decrypt GitLab connection credentials")
	}
	config.WebhookSecret, err = h.openVCSSecret(connection.WebhookSecretEncrypted)
	if err != nil {
		return gitlab.Config{}, errors.New("cannot decrypt GitLab webhook credentials")
	}
	if config.Token == "" || config.WebhookSecret == "" {
		return gitlab.Config{}, errGitLabConnectionUnavailable
	}
	return config, nil
}

func (h *Handler) gitLabConfigForConnection(ctx context.Context, workspaceID, connectionID pgtype.UUID) (gitlab.Config, error) {
	connection, err := h.resolveGitLabVCSConnection(ctx, workspaceID, connectionID)
	if err != nil {
		return gitlab.Config{}, err
	}
	return h.gitLabConfigForVCSConnectionRow(connection)
}

func (h *Handler) createGitLabVCSConnection(
	ctx context.Context,
	workspaceID pgtype.UUID,
	baseURL, token string,
	connectedBy pgtype.UUID,
	preferredWebhookSecret string,
) (db.VcsConnection, string, error) {
	if h.VCSSecretBox == nil {
		return db.VcsConnection{}, "", errGitLabConnectionUnavailable
	}
	connections, err := h.Queries.ListVCSConnectionsByWorkspace(ctx, workspaceID)
	if err != nil {
		return db.VcsConnection{}, "", err
	}
	var existing db.VcsConnection
	for _, candidate := range connections {
		candidateURL, _, valid := normalizeGitLabConnectionBaseURL(candidate.InstanceUrl)
		if !valid || candidateURL != baseURL {
			continue
		}
		if candidate.Provider != string(vcs.KindGitLab) {
			return db.VcsConnection{}, "", errGitLabConnectionConflict
		}
		existing = candidate
		break
	}
	if existing.ID.Valid && !connectedBy.Valid {
		connectedBy = existing.ConnectedByID
	}

	account, err := validateGitLabVCSConnectionToken(ctx, baseURL, token)
	if err != nil {
		if errors.Is(err, vcs.ErrUnauthorized) {
			return db.VcsConnection{}, "", gitlab.ErrUnauthorized
		}
		return db.VcsConnection{}, "", fmt.Errorf("%w: %v", errGitLabTokenValidation, err)
	}
	tokenEncrypted, err := h.sealVCSSecret(token)
	if err != nil {
		return db.VcsConnection{}, "", err
	}
	secretEncrypted := ""
	webhookSecret := ""
	if existing.ID.Valid && existing.WebhookSecretEncrypted != "" {
		secretEncrypted = existing.WebhookSecretEncrypted
		if !connectedBy.Valid {
			connectedBy = existing.ConnectedByID
		}
	} else {
		webhookSecret = preferredWebhookSecret
		if webhookSecret == "" {
			webhookSecret, err = newVCSWebhookSecret()
			if err != nil {
				return db.VcsConnection{}, "", err
			}
		}
		secretEncrypted, err = h.sealVCSSecret(webhookSecret)
		if err != nil {
			return db.VcsConnection{}, "", err
		}
	}
	connection, err := h.Queries.UpsertVCSConnection(ctx, db.UpsertVCSConnectionParams{
		WorkspaceID:            workspaceID,
		Provider:               string(vcs.KindGitLab),
		InstanceUrl:            baseURL,
		AccountLogin:           account.Login,
		AccessTokenEncrypted:   tokenEncrypted,
		WebhookSecretEncrypted: secretEncrypted,
		ConnectedByID:          connectedBy,
	})
	if err != nil {
		return db.VcsConnection{}, "", err
	}
	return connection, webhookSecret, nil
}

func (h *Handler) writeGitLabConnectionError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, errGitLabConnectionConflict):
		writeError(w, http.StatusConflict, "a non-GitLab VCS connection already exists for this instance")
	case errors.Is(err, gitlab.ErrUnauthorized):
		writeError(w, http.StatusBadRequest, "GitLab rejected the access token")
	case errors.Is(err, errGitLabConnectionUnavailable):
		writeError(w, http.StatusServiceUnavailable, "GitLab connection credentials are unavailable")
	case errors.Is(err, errGitLabTokenValidation):
		writeError(w, http.StatusBadGateway, "failed to validate GitLab credentials")
	default:
		writeError(w, http.StatusInternalServerError, "failed to save GitLab connection")
	}
}
