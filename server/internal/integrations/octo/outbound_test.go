package octo

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/multica-ai/multica/server/internal/events"
	"github.com/multica-ai/multica/server/internal/integrations/channel"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/multica-ai/multica/server/pkg/protocol"
)

type fakeOctoOutboundQueries struct {
	binding    db.ChannelChatSessionBinding
	bindingErr error
	inst       db.ChannelInstallation
	instErr    error
}

func (f *fakeOctoOutboundQueries) GetChannelChatSessionBindingBySession(context.Context, db.GetChannelChatSessionBindingBySessionParams) (db.ChannelChatSessionBinding, error) {
	return f.binding, f.bindingErr
}

func (f *fakeOctoOutboundQueries) GetChannelInstallation(context.Context, db.GetChannelInstallationParams) (db.ChannelInstallation, error) {
	return f.inst, f.instErr
}

type fakeOctoSender struct {
	called int
	got    octoOutboundMessage
}

func (f *fakeOctoSender) Send(_ context.Context, out octoOutboundMessage) (channel.SendResult, error) {
	f.called++
	f.got = out
	return channel.SendResult{MessageID: "m-1"}, nil
}

func testUUID(b byte) pgtype.UUID {
	var u pgtype.UUID
	u.Bytes[0] = b
	u.Valid = true
	return u
}

func octoInstallConfigJSON() []byte {
	raw, _ := json.Marshal(installConfig{
		AppID:            "robot-1",
		RobotID:          "robot-1",
		APIURL:           "https://octo.example/api",
		WSURL:            "wss://octo.example/ws",
		IMTokenEncrypted: base64.StdEncoding.EncodeToString([]byte("im-token")),
	})
	return raw
}

func octoChatDoneEvent(sessionID string, content string) events.Event {
	return events.Event{
		Type:          protocol.EventChatDone,
		ChatSessionID: sessionID,
		Payload:       protocol.ChatDonePayload{Content: content},
	}
}

func octoTaskFailedEvent(sessionID string) events.Event {
	return events.Event{
		Type:    protocol.EventTaskFailed,
		Payload: map[string]any{"chat_session_id": sessionID},
	}
}

func TestOutboundPostsReplyWithBoundOctoChannelTypeAndQuoteTarget(t *testing.T) {
	cfg, _ := json.Marshal(octoBindingConfig{ChannelID: "group-1____topic-1", ChannelType: octoChannelTypeCommunityTopic})
	q := &fakeOctoOutboundQueries{
		binding: db.ChannelChatSessionBinding{
			InstallationID: testUUID(1),
			ChannelChatID:  "group-1____topic-1",
			Config:         cfg,
			LastMessageID:  pgtype.Text{String: "9001", Valid: true},
		},
		inst: db.ChannelInstallation{ID: testUUID(1), Status: "active", Config: octoInstallConfigJSON()},
	}
	fs := &fakeOctoSender{}
	o := NewOutbound(q, nil, nil)
	o.newSender = func(credentials) octoReplySender { return fs }

	o.handleEvent(octoChatDoneEvent("00000000-0000-0000-0000-000000000001", "done"))

	if fs.called != 1 {
		t.Fatalf("sender called %d times, want 1", fs.called)
	}
	if fs.got.ChannelID != "group-1____topic-1" || fs.got.ChannelType != octoChannelTypeCommunityTopic {
		t.Errorf("target = %+v", fs.got)
	}
	if fs.got.ReplyTo != "9001" {
		t.Errorf("reply target = %q, want last inbound message", fs.got.ReplyTo)
	}
	if fs.got.Text != "done" {
		t.Errorf("text = %q", fs.got.Text)
	}
}

func TestOutboundPostsTaskFailedNotice(t *testing.T) {
	const sid = "00000000-0000-0000-0000-000000000001"
	cfg, _ := json.Marshal(octoBindingConfig{ChannelID: "group-1", ChannelType: octoChannelTypeGroup})
	q := &fakeOctoOutboundQueries{
		binding: db.ChannelChatSessionBinding{
			InstallationID: testUUID(1),
			ChannelChatID:  "group-1",
			Config:         cfg,
			LastMessageID:  pgtype.Text{String: "9001", Valid: true},
		},
		inst: db.ChannelInstallation{ID: testUUID(1), Status: "active", Config: octoInstallConfigJSON()},
	}
	fs := &fakeOctoSender{}
	o := NewOutbound(q, nil, nil)
	o.newSender = func(credentials) octoReplySender { return fs }

	o.handleEvent(octoTaskFailedEvent(sid))

	if fs.called != 1 {
		t.Fatalf("sender called %d times, want 1", fs.called)
	}
	if fs.got.ChannelID != "group-1" || fs.got.ChannelType != octoChannelTypeGroup {
		t.Fatalf("target = %+v", fs.got)
	}
	if fs.got.ReplyTo != "9001" {
		t.Fatalf("failed notice should quote last inbound message, got %q", fs.got.ReplyTo)
	}
	if fs.got.Text == "" {
		t.Fatal("failed notice text must be non-empty")
	}
}

func TestOutboundIgnoresNonOctoEmptyAndRevoked(t *testing.T) {
	const sid = "00000000-0000-0000-0000-000000000001"
	cfg, _ := json.Marshal(octoBindingConfig{ChannelID: "group-1", ChannelType: octoChannelTypeGroup})
	active := db.ChannelInstallation{ID: testUUID(1), Status: "active", Config: octoInstallConfigJSON()}
	binding := db.ChannelChatSessionBinding{InstallationID: testUUID(1), ChannelChatID: "group-1", Config: cfg}

	cases := []struct {
		name string
		q    *fakeOctoOutboundQueries
		evt  events.Event
	}{
		{name: "no octo binding", q: &fakeOctoOutboundQueries{bindingErr: pgx.ErrNoRows}, evt: octoChatDoneEvent(sid, "hi")},
		{name: "empty content", q: &fakeOctoOutboundQueries{binding: binding, inst: active}, evt: octoChatDoneEvent(sid, "")},
		{name: "revoked", q: &fakeOctoOutboundQueries{binding: binding, inst: db.ChannelInstallation{ID: testUUID(1), Status: "revoked", Config: octoInstallConfigJSON()}}, evt: octoChatDoneEvent(sid, "hi")},
		{name: "no session", q: &fakeOctoOutboundQueries{}, evt: octoChatDoneEvent("", "hi")},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fs := &fakeOctoSender{}
			o := NewOutbound(tc.q, nil, nil)
			o.newSender = func(credentials) octoReplySender { return fs }
			o.handleEvent(tc.evt)
			if fs.called != 0 {
				t.Fatalf("sender called %d times, want 0", fs.called)
			}
		})
	}
}
