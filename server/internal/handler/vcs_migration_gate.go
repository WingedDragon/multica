package handler

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"
)

var errVCSMigrationMaintenance = errors.New("GitLab consolidation is in maintenance mode")

func (h *Handler) gitLabMigrationReadyContext(ctx context.Context) (bool, error) {
	state, err := h.Queries.GetVCSGitLabMigrationState(ctx)
	if err != nil {
		return false, err
	}
	return state.Phase == "complete" && !state.MaintenanceMode, nil
}

func (h *Handler) gitLabMigrationReady(r *http.Request) (bool, error) {
	return h.gitLabMigrationReadyContext(r.Context())
}

func isVCSMigrationGatedRequest(r *http.Request) bool {
	switch r.Method {
	case http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete:
		return !strings.HasPrefix(r.URL.Path, "/api/webhooks/vcs/")
	case http.MethodGet:
		switch r.URL.Path {
		case "/api/github/setup",
			"/api/integrations/composio/callback",
			"/api/apps/oauth/callback",
			"/collab",
			"/api/daemon/ws":
			return true
		}
	}
	return false
}

// GitLabMigrationWriteGate closes HTTP mutations and stateful GET handshakes
// during the cutover. Unified VCS webhooks reach their receipt handler, which
// persists maintenance deliveries and then deliberately returns 503.
func (h *Handler) GitLabMigrationWriteGate(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !isVCSMigrationGatedRequest(r) {
			next.ServeHTTP(w, r)
			return
		}
		ready, err := h.gitLabMigrationReady(r)
		if err != nil || !ready {
			writeError(w, http.StatusServiceUnavailable, errVCSMigrationMaintenance.Error())
			return
		}
		next.ServeHTTP(w, r)
	})
}

// GitLabMigrationWebSocketGate rejects new collaboration sockets during
// maintenance and cancels the request context of an established proxy session
// as soon as the migration state becomes unready.
func (h *Handler) GitLabMigrationWebSocketGate(next http.Handler, pollInterval time.Duration) http.Handler {
	if pollInterval <= 0 {
		pollInterval = time.Second
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ready, err := h.gitLabMigrationReady(r)
		if err != nil || !ready {
			writeError(w, http.StatusServiceUnavailable, errVCSMigrationMaintenance.Error())
			return
		}

		ctx, cancel := context.WithCancel(r.Context())
		done := make(chan struct{})
		go func() {
			defer close(done)
			ticker := time.NewTicker(pollInterval)
			defer ticker.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case <-ticker.C:
					ready, err := h.gitLabMigrationReadyContext(ctx)
					if err != nil || !ready {
						cancel()
						return
					}
				}
			}
		}()
		next.ServeHTTP(w, r.WithContext(ctx))
		cancel()
		<-done
	})
}
