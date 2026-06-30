package octo

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/multica-ai/multica/server/internal/util"
	"github.com/multica-ai/multica/server/internal/util/secretbox"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

func TestRegisterBotCallsOctoRegister(t *testing.T) {
	var gotAuth string
	var gotReq map[string]string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/bot/register" {
			t.Fatalf("path = %q", r.URL.Path)
		}
		gotAuth = r.Header.Get("Authorization")
		if err := json.NewDecoder(r.Body).Decode(&gotReq); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"robot_id":"robot-1",
			"name":"Octo Bot",
			"im_token":"im-token",
			"ws_url":"wss://octo.example/ws",
			"api_url":"https://octo.example/api",
			"owner_uid":"owner-1",
			"owner_channel_id":"owner-channel"
		}`))
	}))
	defer srv.Close()

	resp, err := registerBot(ctxWithTestTimeout(t), srv.Client(), srv.URL, "bf-token")
	if err != nil {
		t.Fatalf("registerBot: %v", err)
	}
	if gotAuth != "Bearer bf-token" {
		t.Errorf("Authorization = %q", gotAuth)
	}
	if gotReq["agent_platform"] != "multica" {
		t.Errorf("agent_platform = %q", gotReq["agent_platform"])
	}
	if resp.RobotID != "robot-1" || resp.IMToken != "im-token" || resp.OwnerUID != "owner-1" {
		t.Errorf("resp = %+v", resp)
	}
}

func TestRegisterBYORejectsInvalidTokenPrefix(t *testing.T) {
	s := &InstallService{}
	if _, err := s.RegisterBYO(context.Background(), RegisterBYOParams{BotToken: "bad"}); err != ErrInvalidBotToken {
		t.Fatalf("RegisterBYO error = %v, want ErrInvalidBotToken", err)
	}
}

func TestRegisterBYOPersistsRegisteredOctoBot(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"robot_id":"robot-1",
			"name":"Octo Bot",
			"im_token":"im-token",
			"ws_url":"wss://octo.example/ws",
			"api_url":"https://octo.example/api",
			"owner_uid":"owner-1",
			"owner_channel_id":"owner-channel"
		}`))
	}))
	defer srv.Close()

	q := &fakeInstallQueries{rowID: mustUUID(t, "00000000-0000-0000-0000-000000000010")}
	svc := newTestInstallService(t, q)
	row, err := svc.RegisterBYO(context.Background(), RegisterBYOParams{
		WorkspaceID: mustUUID(t, "00000000-0000-0000-0000-000000000001"),
		AgentID:     mustUUID(t, "00000000-0000-0000-0000-000000000002"),
		InitiatorID: mustUUID(t, "00000000-0000-0000-0000-000000000003"),
		BotToken:    "bf_token",
		APIURL:      srv.URL,
	})
	if err != nil {
		t.Fatalf("RegisterBYO: %v", err)
	}
	if !q.upsertCalled {
		t.Fatal("expected UpsertChannelInstallation to be called")
	}
	if row.ChannelType != string(TypeOcto) {
		t.Errorf("ChannelType = %q", row.ChannelType)
	}
	var cfg installConfig
	if err := json.Unmarshal(q.upsertParams.Config, &cfg); err != nil {
		t.Fatalf("decode persisted config: %v", err)
	}
	if cfg.AppID != "robot-1" || cfg.RobotID != "robot-1" || cfg.OwnerUID != "owner-1" {
		t.Errorf("config = %+v", cfg)
	}
	if cfg.APIURL != "https://octo.example/api" || cfg.WSURL != "wss://octo.example/ws" {
		t.Errorf("urls = %+v", cfg)
	}
	if cfg.IMTokenEncrypted == "" || cfg.IMTokenEncrypted == "im-token" || cfg.IMTokenEncrypted == "bf-token" {
		t.Fatalf("im token should be encrypted, got %q", cfg.IMTokenEncrypted)
	}
	creds, err := decodeCredentials(q.upsertParams.Config, svc.box.Open)
	if err != nil {
		t.Fatalf("decode stored credentials: %v", err)
	}
	if creds.IMToken != "im-token" {
		t.Errorf("stored im token = %q", creds.IMToken)
	}
}

func testBox(t *testing.T) *secretbox.Box {
	t.Helper()
	key := make([]byte, secretbox.KeySize)
	for i := range key {
		key[i] = byte(i + 1)
	}
	box, err := secretbox.New(key)
	if err != nil {
		t.Fatalf("secretbox.New: %v", err)
	}
	return box
}

func mustUUID(t *testing.T, s string) pgtype.UUID {
	t.Helper()
	u, err := util.ParseUUID(s)
	if err != nil {
		t.Fatalf("parse uuid %q: %v", s, err)
	}
	return u
}

type fakeInstallQueries struct {
	rowID        pgtype.UUID
	appIDTaken   bool
	upsertParams db.UpsertChannelInstallationParams
	upsertCalled bool
}

func (f *fakeInstallQueries) WithTx(_ pgx.Tx) installQueries { return f }

func (f *fakeInstallQueries) UpsertChannelInstallation(_ context.Context, arg db.UpsertChannelInstallationParams) (db.ChannelInstallation, error) {
	f.upsertCalled = true
	f.upsertParams = arg
	if f.appIDTaken {
		return db.ChannelInstallation{}, &pgconn.PgError{Code: "23505"}
	}
	return db.ChannelInstallation{
		ID:              f.rowID,
		WorkspaceID:     arg.WorkspaceID,
		AgentID:         arg.AgentID,
		ChannelType:     arg.ChannelType,
		Config:          arg.Config,
		InstallerUserID: arg.InstallerUserID,
		Status:          "active",
	}, nil
}

func (f *fakeInstallQueries) ListChannelInstallationsByWorkspace(_ context.Context, _ db.ListChannelInstallationsByWorkspaceParams) ([]db.ChannelInstallation, error) {
	return nil, nil
}

func (f *fakeInstallQueries) GetChannelInstallationInWorkspace(_ context.Context, _ db.GetChannelInstallationInWorkspaceParams) (db.ChannelInstallation, error) {
	return db.ChannelInstallation{}, nil
}

func (f *fakeInstallQueries) SetChannelInstallationStatus(_ context.Context, _ db.SetChannelInstallationStatusParams) error {
	return nil
}

type fakeTx struct {
	pgx.Tx
	committed bool
}

func (t *fakeTx) Commit(context.Context) error   { t.committed = true; return nil }
func (t *fakeTx) Rollback(context.Context) error { return nil }

type fakeTxStarter struct{ tx *fakeTx }

func (f *fakeTxStarter) Begin(context.Context) (pgx.Tx, error) { return f.tx, nil }

func newTestInstallService(t *testing.T, q installQueries) *InstallService {
	t.Helper()
	svc, err := newInstallService(q, &fakeTxStarter{tx: &fakeTx{}}, testBox(t), nil)
	if err != nil {
		t.Fatalf("newInstallService: %v", err)
	}
	return svc
}
