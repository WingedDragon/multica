package handler

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/multica-ai/multica/server/internal/collab"
)

func setVCSMigrationStateForTest(t *testing.T, phase string, maintenance bool) {
	t.Helper()
	if testPool == nil {
		t.Skip("database not available")
	}
	ctx := context.Background()
	var oldPhase string
	var oldMaintenance bool
	if err := testPool.QueryRow(ctx, `
		SELECT phase, maintenance_mode
		FROM vcs_gitlab_migration_state
		WHERE id = 1
	`).Scan(&oldPhase, &oldMaintenance); err != nil {
		t.Fatalf("read VCS migration state: %v", err)
	}
	t.Cleanup(func() {
		if _, err := testPool.Exec(context.Background(), `
			UPDATE vcs_gitlab_migration_state
			SET phase = $1, maintenance_mode = $2, updated_at = now()
			WHERE id = 1
		`, oldPhase, oldMaintenance); err != nil {
			t.Errorf("restore VCS migration state: %v", err)
		}
	})
	if _, err := testPool.Exec(ctx, `
		UPDATE vcs_gitlab_migration_state
		SET phase = $1, maintenance_mode = $2, updated_at = now()
		WHERE id = 1
	`, phase, maintenance); err != nil {
		t.Fatalf("set VCS migration state: %v", err)
	}
}

func TestGitLabMigrationWriteGateBlocksBusinessWritesAndOAuthCallbacks(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	setVCSMigrationStateForTest(t, "verification_pending", true)

	var handled int
	next := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		handled++
		w.WriteHeader(http.StatusNoContent)
	})
	gate := testHandler.GitLabMigrationWriteGate(next)
	tests := []struct {
		method string
		path   string
		status int
	}{
		{http.MethodPost, "/api/issues", http.StatusServiceUnavailable},
		{http.MethodPatch, "/api/issues/1", http.StatusServiceUnavailable},
		{http.MethodDelete, "/api/issues/1", http.StatusServiceUnavailable},
		{http.MethodGet, "/api/github/setup", http.StatusServiceUnavailable},
		{http.MethodGet, "/api/integrations/composio/callback", http.StatusServiceUnavailable},
		{http.MethodGet, "/api/apps/oauth/callback", http.StatusServiceUnavailable},
		{http.MethodGet, "/collab", http.StatusServiceUnavailable},
		{http.MethodGet, "/api/daemon/ws", http.StatusServiceUnavailable},
		{http.MethodPost, "/api/webhooks/vcs/connection-1", http.StatusNoContent},
		{http.MethodGet, "/api/issues", http.StatusNoContent},
		{http.MethodOptions, "/api/issues", http.StatusNoContent},
	}
	for _, tc := range tests {
		t.Run(tc.method+" "+tc.path, func(t *testing.T) {
			before := handled
			req := httptest.NewRequest(tc.method, tc.path, nil)
			w := httptest.NewRecorder()
			gate.ServeHTTP(w, req)
			if w.Code != tc.status {
				t.Fatalf("status = %d, want %d: %s", w.Code, tc.status, w.Body.String())
			}
			wantHandled := tc.status == http.StatusNoContent
			if (handled > before) != wantHandled {
				t.Fatalf("downstream called = %t, want %t", handled > before, wantHandled)
			}
		})
	}

	setVCSMigrationStateForTest(t, "complete", false)
	w := httptest.NewRecorder()
	gate.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/api/issues", nil))
	if w.Code != http.StatusNoContent {
		t.Fatalf("write after migration completion status = %d, want 204", w.Code)
	}
}

func TestGitLabMigrationWebSocketGateClosesEstablishedCollabAndAllowsReconnect(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	setVCSMigrationStateForTest(t, "complete", false)

	upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	accepted := make(chan struct{}, 2)
	backendClosed := make(chan struct{}, 2)
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		accepted <- struct{}{}
		defer conn.Close()
		_, _, err = conn.ReadMessage()
		if err != nil {
			backendClosed <- struct{}{}
		}
	}))
	defer backend.Close()

	client, err := collab.NewClient(backend.URL, "collab-secret")
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	gate := testHandler.GitLabMigrationWebSocketGate(client.Proxy(), 10*time.Millisecond)
	api := httptest.NewServer(gate)
	defer api.Close()

	dial := func() (*websocket.Conn, *http.Response, error) {
		return websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(api.URL, "http"), nil)
	}
	conn, resp, err := dial()
	if err != nil {
		t.Fatalf("initial collab websocket dial failed (response %v): %v", resp, err)
	}
	defer conn.Close()
	awaitSignal(t, accepted, "first collab websocket accept")

	setVCSMigrationStateForTest(t, "verification_pending", true)
	awaitSignal(t, backendClosed, "collab websocket close on maintenance")
	if _, _, err := conn.ReadMessage(); err == nil {
		t.Fatal("established collab websocket remained open after maintenance began")
	}

	blocked, blockedResponse, err := dial()
	if err == nil {
		blocked.Close()
		t.Fatal("new collab websocket handshake succeeded during maintenance")
	}
	if blockedResponse == nil || blockedResponse.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("maintenance handshake response = %v, want 503", blockedResponse)
	}

	setVCSMigrationStateForTest(t, "complete", false)
	reconnected, reconnectResponse, err := dial()
	if err != nil {
		t.Fatalf("collab websocket did not reconnect after completion (response %v): %v", reconnectResponse, err)
	}
	defer reconnected.Close()
	awaitSignal(t, accepted, "collab websocket accept after completion")
}

func awaitSignal(t *testing.T, ch <-chan struct{}, what string) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(2 * time.Second):
		t.Fatalf("timed out waiting for %s", what)
	}
}
