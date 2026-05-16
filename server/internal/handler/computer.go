package handler

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"sort"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgtype"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/multica-ai/multica/server/pkg/protocol"
)

// ComputerResponse is the §6.2 aggregate view of a daemon. It's the
// (workspace_id, daemon_id) group rollup of agent_runtime rows; no new
// table backs it. computer_id == daemon_id (§6.1 / D1) — the URL
// /computers/<daemon_id> is the canonical identifier the UI uses.
type ComputerResponse struct {
	ID            string                 `json:"id"`
	WorkspaceID   string                 `json:"workspace_id"`
	Name          string                 `json:"name"`
	Kind          string                 `json:"kind"`
	DeviceInfo    string                 `json:"device_info"`
	InstallSource string                 `json:"install_source"`
	Metadata      map[string]any         `json:"metadata"`
	OwnerID       *string                `json:"owner_id"`
	Status        string                 `json:"status"`
	LastSeenAt    *string                `json:"last_seen_at"`
	CreatedAt     string                 `json:"created_at"`
	Runtimes      []AgentRuntimeResponse `json:"runtimes"`
	RuntimeCount  int                    `json:"runtime_count"`
}

// computerListItem trims the per-runtime detail off the list response so
// /api/computers stays cheap even on workspaces with many daemons. The UI
// fetches the full /api/computers/{id} detail when a row is selected.
type computerListItem struct {
	ID            string         `json:"id"`
	WorkspaceID   string         `json:"workspace_id"`
	Name          string         `json:"name"`
	Kind          string         `json:"kind"`
	DeviceInfo    string         `json:"device_info"`
	InstallSource string         `json:"install_source"`
	Metadata      map[string]any `json:"metadata"`
	OwnerID       *string        `json:"owner_id"`
	Status        string         `json:"status"`
	LastSeenAt    *string        `json:"last_seen_at"`
	CreatedAt     string         `json:"created_at"`
	RuntimeCount  int            `json:"runtime_count"`
}

// ListComputers groups every agent_runtime in the workspace by daemon_id
// and returns one aggregate row per Computer. agent_runtime rows without
// a daemon_id (legacy cloud / pre-pairing data) are skipped — there is
// no Computer to attach them to.
func (h *Handler) ListComputers(w http.ResponseWriter, r *http.Request) {
	workspaceID := h.resolveWorkspaceID(r)
	if _, ok := h.requireWorkspaceMember(w, r, workspaceID, "workspace not found"); !ok {
		return
	}

	runtimes, err := h.Queries.ListAgentRuntimes(r.Context(), parseUUID(workspaceID))
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to list computers")
		return
	}

	groups := groupRuntimesByDaemon(runtimes)
	resp := make([]computerListItem, 0, len(groups))
	for _, g := range groups {
		c := buildComputer(g)
		resp = append(resp, computerListItem{
			ID:            c.ID,
			WorkspaceID:   c.WorkspaceID,
			Name:          c.Name,
			Kind:          c.Kind,
			DeviceInfo:    c.DeviceInfo,
			InstallSource: c.InstallSource,
			Metadata:      c.Metadata,
			OwnerID:       c.OwnerID,
			Status:        c.Status,
			LastSeenAt:    c.LastSeenAt,
			CreatedAt:     c.CreatedAt,
			RuntimeCount:  c.RuntimeCount,
		})
	}

	// Sort by name for a stable rendering order. The UI re-sorts client-side
	// based on user preferences, but a deterministic server order keeps tests
	// and snapshot diffs sane.
	sort.Slice(resp, func(i, j int) bool { return resp[i].Name < resp[j].Name })

	writeJSON(w, http.StatusOK, resp)
}

// GetComputer returns the §6.2 detail view: the same aggregate fields as
// the list plus the full runtimes[] array. Lookup is by daemon_id within
// the caller's workspace — there is no global UUID for a Computer.
func (h *Handler) GetComputer(w http.ResponseWriter, r *http.Request) {
	workspaceID := h.resolveWorkspaceID(r)
	if _, ok := h.requireWorkspaceMember(w, r, workspaceID, "workspace not found"); !ok {
		return
	}
	daemonID := chi.URLParam(r, "daemonId")
	if daemonID == "" {
		writeError(w, http.StatusBadRequest, "daemon_id is required")
		return
	}

	runtimes, err := h.Queries.ListAgentRuntimes(r.Context(), parseUUID(workspaceID))
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to load computer")
		return
	}

	var group []db.AgentRuntime
	for _, rt := range runtimes {
		if rt.DaemonID.Valid && rt.DaemonID.String == daemonID {
			group = append(group, rt)
		}
	}
	if len(group) == 0 {
		writeError(w, http.StatusNotFound, "computer not found")
		return
	}

	writeJSON(w, http.StatusOK, buildComputer(group))
}

// DeleteComputer implements §6.3: daemon-scoped Remove. Removes every
// agent_runtime row for this (workspace, daemon) pair and revokes the
// daemon_token in this workspace only — the daemon's bindings to other
// workspaces stay intact (the daemon process itself is never uninstalled).
//
// D2 contract: if any agent_runtime under this daemon still has active
// agents or running tasks, return 409 with the occupants so the UI can
// guide the user to cancel / unbind first.
func (h *Handler) DeleteComputer(w http.ResponseWriter, r *http.Request) {
	workspaceID := h.resolveWorkspaceID(r)
	member, ok := h.requireWorkspaceMember(w, r, workspaceID, "workspace not found")
	if !ok {
		return
	}
	daemonID := chi.URLParam(r, "daemonId")
	if daemonID == "" {
		writeError(w, http.StatusBadRequest, "daemon_id is required")
		return
	}

	runtimes, err := h.Queries.ListAgentRuntimes(r.Context(), parseUUID(workspaceID))
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to load computer")
		return
	}

	var group []db.AgentRuntime
	for _, rt := range runtimes {
		if rt.DaemonID.Valid && rt.DaemonID.String == daemonID {
			group = append(group, rt)
		}
	}
	if len(group) == 0 {
		writeError(w, http.StatusNotFound, "computer not found")
		return
	}

	// Permission gate: at least one of the runtimes under this daemon must
	// be editable by the caller. canEditRuntime treats owner/admin as
	// blanket-allowed and members as owner-only — matching the single-row
	// DELETE /api/runtimes/<id> behavior.
	editable := false
	for _, rt := range group {
		if canEditRuntime(member, rt) {
			editable = true
			break
		}
	}
	if !editable {
		writeError(w, http.StatusForbidden, "you can only remove computers you own")
		return
	}

	// D2: aggregate active-agents check across every runtime under this daemon.
	var totalActive int64
	for _, rt := range group {
		count, err := h.Queries.CountActiveAgentsByRuntime(r.Context(), rt.ID)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "failed to check computer dependencies")
			return
		}
		totalActive += count
	}
	if totalActive > 0 {
		writeJSON(w, http.StatusConflict, map[string]any{
			"error":         "computer has active agents bound to it. Archive or reassign the agents first.",
			"active_agents": totalActive,
		})
		return
	}

	// Per-runtime delete: clean up archived agents, then DELETE the row.
	// Mirrors DeleteAgentRuntime so foreign-key behaviour is identical and
	// any future per-runtime side effects (e.g. usage rollup invalidation)
	// only need to be wired in one place.
	for _, rt := range group {
		if err := h.Queries.DeleteArchivedAgentsByRuntime(r.Context(), rt.ID); err != nil {
			writeError(w, http.StatusInternalServerError, "failed to clean up archived agents")
			return
		}
		if err := h.Queries.DeleteAgentRuntime(r.Context(), rt.ID); err != nil {
			writeError(w, http.StatusInternalServerError, "failed to delete computer")
			return
		}
	}

	// Revoke daemon_token in this workspace only. D4 revoke (revoked_at = now())
	// is a soft mark — the row stays until cleanup or natural expiry — so the
	// daemon's other workspace bindings are untouched.
	revoked, err := h.Queries.RevokeDaemonTokensByWorkspaceAndDaemon(r.Context(), db.RevokeDaemonTokensByWorkspaceAndDaemonParams{
		WorkspaceID: parseUUID(workspaceID),
		DaemonID:    daemonID,
	})
	if err != nil {
		// Non-fatal: the agent_runtime rows are already gone; auth path
		// will fail on the next lookup. Log and continue.
		slog.Warn("delete computer: revoke daemon tokens failed", "error", err, "daemon_id", daemonID)
	}
	for _, hash := range revoked {
		h.DaemonTokenCache.Invalidate(r.Context(), hash)
	}

	userID := uuidToString(member.UserID)
	slog.Info(
		"computer removed",
		"workspace_id", workspaceID,
		"daemon_id", daemonID,
		"runtimes_removed", len(group),
		"tokens_revoked", len(revoked),
		"removed_by", userID,
	)

	// Reuse the existing daemon-register event channel so the frontend's
	// runtime-list query (and the new computer-list query, once wired up)
	// both refresh without us introducing a new event type the desktop app
	// would need a build to learn about.
	h.publish(protocol.EventDaemonRegister, workspaceID, "member", userID, map[string]any{
		"action": "delete",
	})

	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// groupRuntimesByDaemon bins agent_runtime rows by daemon_id, preserving
// the source order within each bin (sqlc returns rows ordered by created_at
// asc, which gives the §6.2 list a stable shape). Runtimes without a
// daemon_id (legacy data) are dropped — they don't belong to any Computer.
func groupRuntimesByDaemon(runtimes []db.AgentRuntime) [][]db.AgentRuntime {
	index := map[string]int{}
	var groups [][]db.AgentRuntime
	for _, rt := range runtimes {
		if !rt.DaemonID.Valid || rt.DaemonID.String == "" {
			continue
		}
		if i, ok := index[rt.DaemonID.String]; ok {
			groups[i] = append(groups[i], rt)
			continue
		}
		index[rt.DaemonID.String] = len(groups)
		groups = append(groups, []db.AgentRuntime{rt})
	}
	return groups
}

// buildComputer collapses a group of agent_runtime rows into a single
// ComputerResponse per the §6.2 / D3 field table. D3 explicitly forbids
// new columns: every field below is either taken from an existing column,
// derived from one (kind ← runtime_mode), or pulled from metadata jsonb.
func buildComputer(group []db.AgentRuntime) ComputerResponse {
	first := group[0]

	// Status: §6.2 rule — any row whose status is "online" makes the
	// Computer online. The Redis-TTL-aware liveness check the RFC describes
	// runs in the daemon heartbeat path; agent_runtime.status is the
	// already-resolved view of that, so we trust it here.
	status := "offline"
	var lastSeen pgtype.Timestamptz
	for _, rt := range group {
		if rt.Status == "online" {
			status = "online"
		}
		if rt.LastSeenAt.Valid {
			if !lastSeen.Valid || rt.LastSeenAt.Time.After(lastSeen.Time) {
				lastSeen = rt.LastSeenAt
			}
		}
	}

	metadata := map[string]any{}
	if first.Metadata != nil {
		_ = json.Unmarshal(first.Metadata, &metadata)
	}
	installSource, _ := metadata["install_source"].(string)

	runtimes := make([]AgentRuntimeResponse, len(group))
	for i, rt := range group {
		runtimes[i] = runtimeToResponse(rt)
	}

	return ComputerResponse{
		ID:            first.DaemonID.String,
		WorkspaceID:   uuidToString(first.WorkspaceID),
		Name:          first.Name,
		Kind:          first.RuntimeMode, // D3: kind := runtime_mode
		DeviceInfo:    first.DeviceInfo,
		InstallSource: installSource,
		Metadata:      metadata,
		OwnerID:       uuidToPtr(first.OwnerID),
		Status:        status,
		LastSeenAt:    timestampToPtr(lastSeen),
		CreatedAt:     timestampToString(first.CreatedAt),
		Runtimes:      runtimes,
		RuntimeCount:  len(group),
	}
}
