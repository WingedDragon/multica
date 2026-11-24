package handler

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/multica-ai/multica/server/internal/daemonws"
	"github.com/multica-ai/multica/server/pkg/protocol"
)

func TestDaemonRPCOnEstablishedWebSocketTracksMigrationGate(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	setVCSMigrationStateForTest(t, "complete", false)

	hub := daemonws.NewHub()
	hub.SetRPCHandler(testHandler.DaemonRPCHandler)
	identity := daemonws.ClientIdentity{
		UserID:       testUserID,
		WorkspaceID:  testWorkspaceID,
		WorkspaceIDs: []string{testWorkspaceID},
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hub.HandleWebSocket(w, r, identity)
	}))
	defer server.Close()

	conn, response, err := websocket.DefaultDialer.Dial("ws"+server.URL[len("http"):], nil)
	if err != nil {
		t.Fatalf("daemon WebSocket dial failed (response %v): %v", response, err)
	}
	defer conn.Close()
	conn.SetReadDeadline(time.Now().Add(2 * time.Second))

	requestStatus := func(requestID string) int {
		t.Helper()
		payload, err := json.Marshal(protocol.RPCRequestPayload{
			RequestID: requestID,
			Method:    "tasks.claim",
			Body:      json.RawMessage(`{}`),
			TimeoutMs: 1000,
		})
		if err != nil {
			t.Fatalf("marshal RPC request: %v", err)
		}
		raw, err := json.Marshal(protocol.Message{
			Type:    protocol.EventDaemonRPCRequest,
			Payload: payload,
		})
		if err != nil {
			t.Fatalf("marshal daemon message: %v", err)
		}
		if err := conn.WriteMessage(websocket.TextMessage, raw); err != nil {
			t.Fatalf("write RPC request %q: %v", requestID, err)
		}
		_, raw, err = conn.ReadMessage()
		if err != nil {
			t.Fatalf("read RPC response %q: %v", requestID, err)
		}
		var msg protocol.Message
		if err := json.Unmarshal(raw, &msg); err != nil {
			t.Fatalf("decode RPC message %q: %v", requestID, err)
		}
		if msg.Type != protocol.EventDaemonRPCResponse {
			t.Fatalf("message type = %q, want %q", msg.Type, protocol.EventDaemonRPCResponse)
		}
		var rpcResponse protocol.RPCResponsePayload
		if err := json.Unmarshal(msg.Payload, &rpcResponse); err != nil {
			t.Fatalf("decode RPC response %q: %v", requestID, err)
		}
		if rpcResponse.RequestID != requestID {
			t.Fatalf("response request_id = %q, want %q", rpcResponse.RequestID, requestID)
		}
		return rpcResponse.Status
	}

	if status := requestStatus("before-maintenance"); status == http.StatusServiceUnavailable {
		t.Fatalf("RPC status before maintenance = %d, want normal handler response", status)
	}
	setVCSMigrationStateForTest(t, "verification_pending", true)
	if status := requestStatus("during-maintenance"); status != http.StatusServiceUnavailable {
		t.Fatalf("RPC status during maintenance = %d, want 503", status)
	}
	setVCSMigrationStateForTest(t, "complete", false)
	if status := requestStatus("after-maintenance"); status == http.StatusServiceUnavailable {
		t.Fatalf("RPC status after maintenance = %d, want normal handler response", status)
	}
}

func TestDaemonWSHeartbeatRejectsMaintenanceBeforeRuntimeWrites(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	setVCSMigrationStateForTest(t, "verification_pending", true)

	const runtimeID = "00000000-0000-4000-8000-000000000001"
	identity := daemonws.ClientIdentity{
		WorkspaceID:  testWorkspaceID,
		WorkspaceIDs: []string{testWorkspaceID},
		RuntimeIDs:   []string{runtimeID},
		RuntimeLeases: map[string]*daemonws.RuntimeLease{
			runtimeID: daemonws.NewRuntimeLease(testWorkspaceID, "online", time.Now(), true),
		},
	}
	ack, err := testHandler.HandleDaemonWSHeartbeat(context.Background(), identity, runtimeID, protocol.DaemonHeartbeatRequestPayload{RuntimeID: runtimeID})
	if !errors.Is(err, errVCSMigrationMaintenance) {
		t.Fatalf("heartbeat error = %v, want maintenance gate error", err)
	}
	if ack != nil {
		t.Fatalf("heartbeat ack = %+v, want nil during maintenance", ack)
	}
}
