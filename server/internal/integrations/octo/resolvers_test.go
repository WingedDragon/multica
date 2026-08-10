package octo

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/multica-ai/multica/server/internal/integrations/channel"
	"github.com/multica-ai/multica/server/internal/integrations/channel/engine"
)

type fakeOctoChatSession struct {
	appendIn     engine.AppendInput
	mediaIn      engine.BindMediaInput
	pendingFresh pgtype.UUID
}

func (f *fakeOctoChatSession) EnsureSession(_ context.Context, _ engine.EnsureSessionInput) (pgtype.UUID, error) {
	return testUUID(42), nil
}

func (f *fakeOctoChatSession) MarkPendingFresh(_ context.Context, sessionID pgtype.UUID) error {
	f.pendingFresh = sessionID
	return nil
}

func (f *fakeOctoChatSession) AppendUserMessage(_ context.Context, in engine.AppendInput) (engine.AppendResult, error) {
	f.appendIn = in
	return engine.AppendResult{}, nil
}

func (f *fakeOctoChatSession) BindMediaRefs(_ context.Context, in engine.BindMediaInput) error {
	f.mediaIn = in
	return nil
}

func TestOctoSessionBinderMarksPendingFresh(t *testing.T) {
	session := &fakeOctoChatSession{}
	binder := sessionBinder{session: session}
	sessionID := testUUID(91)

	if err := binder.MarkPendingFresh(context.Background(), sessionID); err != nil {
		t.Fatalf("MarkPendingFresh: %v", err)
	}
	if session.pendingFresh != sessionID {
		t.Fatalf("pending fresh session = %v, want %v", session.pendingFresh, sessionID)
	}
}

func TestOctoSessionRoutingStoresChannelTypeForOutbound(t *testing.T) {
	msg := channel.InboundMessage{
		MessageID: "m-1",
		Source: channel.Source{
			ChannelType: TypeOcto,
			ChatID:      "group-1____topic-1",
			ChatType:    channel.ChatTypeGroup,
		},
		Raw: mustRaw(t, octoRawMessage{
			RobotID:     "robot-1",
			ChannelType: octoChannelTypeCommunityTopic,
		}),
	}

	key, cfg := octoSessionRouting(msg)
	if key != "group-1____topic-1" {
		t.Fatalf("binding key = %q", key)
	}
	var decoded octoBindingConfig
	if err := json.Unmarshal(cfg, &decoded); err != nil {
		t.Fatalf("decode config: %v", err)
	}
	if decoded.ChannelID != "group-1____topic-1" || decoded.ChannelType != octoChannelTypeCommunityTopic {
		t.Errorf("binding config = %+v", decoded)
	}
}

func TestOctoSessionRoutingFallsBackToDMChannelType(t *testing.T) {
	msg := channel.InboundMessage{
		Source: channel.Source{
			ChannelType: TypeOcto,
			ChatID:      "u-alice",
			ChatType:    channel.ChatTypeP2P,
		},
	}

	_, cfg := octoSessionRouting(msg)
	var decoded octoBindingConfig
	if err := json.Unmarshal(cfg, &decoded); err != nil {
		t.Fatalf("decode config: %v", err)
	}
	if decoded.ChannelType != octoChannelTypeDM {
		t.Errorf("fallback channel_type = %d, want DM", decoded.ChannelType)
	}
}

func TestOctoSessionBinderForwardsMediaPendingSeconds(t *testing.T) {
	session := &fakeOctoChatSession{}
	binder := &sessionBinder{session: session}

	if _, err := binder.AppendMessage(context.Background(), engine.AppendParams{
		SessionID:           testUUID(1),
		Sender:              testUUID(2),
		InstallationID:      testUUID(3),
		ClaimToken:          testUUID(4),
		MediaPendingSeconds: 12.5,
		Message: channel.InboundMessage{
			MessageID: "message-1",
			Text:      "hello",
		},
	}); err != nil {
		t.Fatalf("AppendMessage: %v", err)
	}

	if session.appendIn.MediaPendingSeconds != 12.5 {
		t.Fatalf("MediaPendingSeconds = %v, want 12.5", session.appendIn.MediaPendingSeconds)
	}
}

func TestOctoSessionBinderBindMediaMapping(t *testing.T) {
	session := &fakeOctoChatSession{}
	binder := &sessionBinder{session: session}
	ref := channel.MediaRef{Type: channel.MsgTypeImage, StorageURL: "https://cdn.example.test/image.png"}

	if err := binder.BindMedia(context.Background(), engine.BindMediaParams{
		MessageID:   testUUID(1),
		SessionID:   testUUID(2),
		WorkspaceID: testUUID(3),
		Sender:      testUUID(4),
		MediaRefs:   []channel.MediaRef{ref},
	}); err != nil {
		t.Fatalf("BindMedia: %v", err)
	}

	got := session.mediaIn
	if got.MessageID != testUUID(1) || got.SessionID != testUUID(2) || got.WorkspaceID != testUUID(3) ||
		got.Sender != testUUID(4) || len(got.MediaRefs) != 1 || got.MediaRefs[0] != ref {
		t.Fatalf("media mapping wrong: %+v", got)
	}
}

func mustRaw(t *testing.T, v any) json.RawMessage {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal raw: %v", err)
	}
	return raw
}
