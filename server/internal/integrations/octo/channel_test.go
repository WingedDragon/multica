package octo

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/multica-ai/multica/server/internal/integrations/channel"
)

const testTimeout = 5 * time.Second

func TestOctoSenderSendPostsBotMessage(t *testing.T) {
	var gotAuth string
	var gotBody struct {
		ChannelID   string          `json:"channel_id"`
		ChannelType octoChannelType `json:"channel_type"`
		Payload     messagePayload  `json:"payload"`
		ClientMsgNo string          `json:"client_msg_no"`
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/bot/sendMessage" {
			t.Fatalf("path = %q, want /v1/bot/sendMessage", r.URL.Path)
		}
		gotAuth = r.Header.Get("Authorization")
		if err := json.NewDecoder(r.Body).Decode(&gotBody); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"message_id":"1234567890123456789","client_msg_no":"client-1","message_seq":42}`))
	}))
	defer srv.Close()

	s := newOctoSender(credentials{APIURL: srv.URL, IMToken: "im-token"}, srv.Client(), nil)
	res, err := s.Send(ctxWithTestTimeout(t), octoOutboundMessage{
		ChannelID:   "group-1",
		ChannelType: octoChannelTypeGroup,
		Text:        "reply body",
	})
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	if res.MessageID != "1234567890123456789" {
		t.Errorf("MessageID = %q", res.MessageID)
	}
	if gotAuth != "Bearer im-token" {
		t.Errorf("Authorization = %q", gotAuth)
	}
	if gotBody.ChannelID != "group-1" || gotBody.ChannelType != octoChannelTypeGroup {
		t.Errorf("channel target = %q/%d", gotBody.ChannelID, gotBody.ChannelType)
	}
	if gotBody.Payload.Type != octoMessageTypeText || gotBody.Payload.Content != "reply body" {
		t.Errorf("payload = %+v", gotBody.Payload)
	}
	if gotBody.ClientMsgNo == "" {
		t.Error("client_msg_no should be generated for idempotency")
	}
}

func TestOctoChannelSendDefaultsToDMWhenNoChannelTypeIsCarried(t *testing.T) {
	var gotChannelType octoChannelType
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			ChannelType octoChannelType `json:"channel_type"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		gotChannelType = req.ChannelType
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"message_id":"m-1","client_msg_no":"c-1","message_seq":1}`))
	}))
	defer srv.Close()

	ch := &octoChannel{
		sender: newOctoSender(credentials{APIURL: srv.URL, IMToken: "im-token"}, srv.Client(), nil),
	}
	_, err := ch.Send(ctxWithTestTimeout(t), channel.OutboundMessage{ChatID: "u-alice", Text: "hello"})
	if err != nil {
		t.Fatalf("channel Send: %v", err)
	}
	if gotChannelType != octoChannelTypeDM {
		t.Errorf("Channel.Send default channel_type = %d, want DM", gotChannelType)
	}
}

func TestOctoFactoryBuildsChannelFromInstallationConfig(t *testing.T) {
	raw, _ := json.Marshal(installConfig{
		AppID:            "robot-1",
		RobotID:          "robot-1",
		APIURL:           "https://octo.example/api",
		WSURL:            "wss://octo.example/ws",
		IMTokenEncrypted: base64.StdEncoding.EncodeToString([]byte("im-token")),
	})

	ch, err := newOctoFactory(ChannelDeps{})(channel.Config{
		Type: TypeOcto,
		Raw:  raw,
		Handler: func(context.Context, channel.InboundMessage) error {
			return nil
		},
	})
	if err != nil {
		t.Fatalf("factory: %v", err)
	}
	oc, ok := ch.(*octoChannel)
	if !ok {
		t.Fatalf("channel type = %T, want *octoChannel", ch)
	}
	if oc.sender == nil || oc.sender.creds.RobotID != "robot-1" || oc.sender.creds.IMToken != "im-token" {
		t.Fatalf("sender credentials = %+v", oc.sender)
	}
	if oc.run == nil {
		t.Fatal("factory should wire the WS run loop")
	}
	if oc.handler == nil {
		t.Fatal("factory should carry the inbound handler")
	}
}

func ctxWithTestTimeout(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
	t.Cleanup(cancel)
	return ctx
}
