package handler

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/multica-ai/multica/server/internal/integrations/octo"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/multica-ai/multica/server/pkg/protocol"
)

type OctoInstallationResponse struct {
	ID              string `json:"id"`
	WorkspaceID     string `json:"workspace_id"`
	AgentID         string `json:"agent_id"`
	RobotID         string `json:"robot_id"`
	OwnerUID        string `json:"owner_uid"`
	OwnerChannelID  string `json:"owner_channel_id"`
	APIURL          string `json:"api_url"`
	WSURL           string `json:"ws_url"`
	InstallerUserID string `json:"installer_user_id"`
	Status          string `json:"status"`
	InstalledAt     string `json:"installed_at"`
	CreatedAt       string `json:"created_at"`
	UpdatedAt       string `json:"updated_at"`
}

func octoInstallationToResponse(row db.ChannelInstallation) OctoInstallationResponse {
	info := octo.DecodePublicConfig(row.Config)
	return OctoInstallationResponse{
		ID:              uuidToString(row.ID),
		WorkspaceID:     uuidToString(row.WorkspaceID),
		AgentID:         uuidToString(row.AgentID),
		RobotID:         info.RobotID,
		OwnerUID:        info.OwnerUID,
		OwnerChannelID:  info.OwnerChannelID,
		APIURL:          info.APIURL,
		WSURL:           info.WSURL,
		InstallerUserID: uuidToString(row.InstallerUserID),
		Status:          row.Status,
		InstalledAt:     row.InstalledAt.Time.UTC().Format(time.RFC3339),
		CreatedAt:       row.CreatedAt.Time.UTC().Format(time.RFC3339),
		UpdatedAt:       row.UpdatedAt.Time.UTC().Format(time.RFC3339),
	}
}

func (h *Handler) ListOctoInstallations(w http.ResponseWriter, r *http.Request) {
	if h.OctoInstall == nil {
		writeJSON(w, http.StatusOK, map[string]any{
			"installations":     []OctoInstallationResponse{},
			"configured":        false,
			"install_supported": false,
		})
		return
	}
	wsUUID, ok := parseUUIDOrBadRequest(w, chi.URLParam(r, "id"), "workspace id")
	if !ok {
		return
	}
	rows, err := h.OctoInstall.ListByWorkspace(r.Context(), wsUUID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to list octo installations")
		return
	}
	out := make([]OctoInstallationResponse, 0, len(rows))
	for _, row := range rows {
		out = append(out, octoInstallationToResponse(row))
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"installations":     out,
		"configured":        true,
		"install_supported": true,
	})
}

type RegisterOctoBYORequest struct {
	BotToken string `json:"bot_token"`
	APIURL   string `json:"api_url"`
}

func (h *Handler) RegisterOctoBYO(w http.ResponseWriter, r *http.Request) {
	if h.OctoInstall == nil {
		writeError(w, http.StatusServiceUnavailable, "octo integration not enabled")
		return
	}
	userID, ok := requireUserID(w, r)
	if !ok {
		return
	}
	wsUUID, ok := parseUUIDOrBadRequest(w, chi.URLParam(r, "id"), "workspace id")
	if !ok {
		return
	}
	agentIDStr := strings.TrimSpace(r.URL.Query().Get("agent_id"))
	if agentIDStr == "" {
		writeError(w, http.StatusBadRequest, "agent_id is required")
		return
	}
	agentUUID, ok := parseUUIDOrBadRequest(w, agentIDStr, "agent_id")
	if !ok {
		return
	}
	if _, err := h.Queries.GetAgentInWorkspace(r.Context(), db.GetAgentInWorkspaceParams{
		ID:          agentUUID,
		WorkspaceID: wsUUID,
	}); err != nil {
		writeError(w, http.StatusNotFound, "agent not found in this workspace")
		return
	}
	initiatorUUID, ok := parseUUIDOrBadRequest(w, userID, "user id")
	if !ok {
		return
	}
	var body RegisterOctoBYORequest
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	row, err := h.OctoInstall.RegisterBYO(r.Context(), octo.RegisterBYOParams{
		WorkspaceID: wsUUID,
		AgentID:     agentUUID,
		InitiatorID: initiatorUUID,
		BotToken:    body.BotToken,
		APIURL:      body.APIURL,
	})
	if err != nil {
		switch {
		case errors.Is(err, octo.ErrInvalidBotToken):
			writeError(w, http.StatusBadRequest, err.Error())
		case errors.Is(err, octo.ErrBotOwnedByAnotherWorkspace):
			writeError(w, http.StatusConflict, "this Octo bot is already connected to a different Multica workspace")
		default:
			writeError(w, http.StatusBadRequest, "could not register the Octo bot - check the bot token and Octo API URL")
		}
		return
	}
	h.publishOctoInstallationCreated(row, userID)
	writeJSON(w, http.StatusOK, octoInstallationToResponse(row))
}

func (h *Handler) publishOctoInstallationCreated(row db.ChannelInstallation, actorID string) {
	h.publish(protocol.EventOctoInstallationCreated, uuidToString(row.WorkspaceID), "user", actorID, map[string]any{
		"id": uuidToString(row.ID),
	})
}

func (h *Handler) RevokeOctoInstallation(w http.ResponseWriter, r *http.Request) {
	if h.OctoInstall == nil {
		writeError(w, http.StatusServiceUnavailable, "octo integration not configured")
		return
	}
	userID, ok := requireUserID(w, r)
	if !ok {
		return
	}
	wsUUID, ok := parseUUIDOrBadRequest(w, chi.URLParam(r, "id"), "workspace id")
	if !ok {
		return
	}
	instUUID, ok := parseUUIDOrBadRequest(w, chi.URLParam(r, "installationId"), "installation id")
	if !ok {
		return
	}
	if _, err := h.OctoInstall.GetInWorkspace(r.Context(), instUUID, wsUUID); err != nil {
		if errors.Is(err, octo.ErrInstallationNotFound) {
			writeError(w, http.StatusNotFound, "octo installation not found")
			return
		}
		writeError(w, http.StatusInternalServerError, "failed to load installation")
		return
	}
	if err := h.OctoInstall.Revoke(r.Context(), instUUID); err != nil {
		writeError(w, http.StatusInternalServerError, "failed to revoke installation")
		return
	}
	h.publish(protocol.EventOctoInstallationRevoked, uuidToString(wsUUID), "user", userID, map[string]any{
		"id": uuidToString(instUUID),
	})
	w.WriteHeader(http.StatusNoContent)
}

type RedeemOctoBindingTokenRequest struct {
	Token string `json:"token"`
}

type RedeemOctoBindingTokenResponse struct {
	WorkspaceID    string `json:"workspace_id"`
	InstallationID string `json:"installation_id"`
	OctoUserID     string `json:"octo_user_id"`
}

func (h *Handler) RedeemOctoBindingToken(w http.ResponseWriter, r *http.Request) {
	if h.OctoBindingTokens == nil {
		writeError(w, http.StatusServiceUnavailable, "octo integration not configured")
		return
	}
	userID, ok := requireUserID(w, r)
	if !ok {
		return
	}
	var req RedeemOctoBindingTokenRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.Token == "" {
		writeError(w, http.StatusBadRequest, "token is required")
		return
	}
	userUUID, ok := parseUUIDOrBadRequest(w, userID, "user id")
	if !ok {
		return
	}
	redeemed, err := h.OctoBindingTokens.RedeemAndBind(r.Context(), req.Token, userUUID)
	if err != nil {
		switch {
		case errors.Is(err, octo.ErrBindingTokenInvalid):
			writeError(w, http.StatusGone, "binding token invalid or expired")
		case errors.Is(err, octo.ErrBindingAlreadyAssigned):
			writeError(w, http.StatusConflict, "this Octo account is already bound to a different Multica user")
		case errors.Is(err, octo.ErrBindingNotWorkspaceMember):
			writeError(w, http.StatusForbidden, "binding refused (are you a workspace member?)")
		default:
			writeError(w, http.StatusInternalServerError, "failed to redeem token")
		}
		return
	}
	writeJSON(w, http.StatusOK, RedeemOctoBindingTokenResponse{
		WorkspaceID:    uuidToString(redeemed.WorkspaceID),
		InstallationID: uuidToString(redeemed.InstallationID),
		OctoUserID:     redeemed.OctoUserID,
	})
}
