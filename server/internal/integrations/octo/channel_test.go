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
		Payload     struct {
			Type    int    `json:"type"`
			Content string `json:"content"`
			Reply   struct {
				MessageID string `json:"message_id"`
			} `json:"reply"`
		} `json:"payload"`
		ClientMsgNo string `json:"client_msg_no"`
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
		ReplyTo:     "9001",
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
	if gotBody.Payload.Reply.MessageID != "9001" {
		t.Errorf("payload.reply.message_id = %q", gotBody.Payload.Reply.MessageID)
	}
	if gotBody.ClientMsgNo == "" {
		t.Error("client_msg_no should be generated for idempotency")
	}
}

func TestOctoSenderTypingAndReadReceipt(t *testing.T) {
	var calls []struct {
		Path string
		Body map[string]any
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer im-token" {
			t.Fatalf("Authorization = %q", r.Header.Get("Authorization"))
		}
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		calls = append(calls, struct {
			Path string
			Body map[string]any
		}{Path: r.URL.Path, Body: body})
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{}`))
	}))
	defer srv.Close()

	s := newOctoSender(credentials{APIURL: srv.URL, IMToken: "im-token"}, srv.Client(), nil)
	target := octoInteractionTarget{
		ChannelID:   "group-1",
		ChannelType: octoChannelTypeGroup,
		MessageIDs:  []string{"9001"},
	}
	if err := s.SendReadReceipt(ctxWithTestTimeout(t), target); err != nil {
		t.Fatalf("SendReadReceipt: %v", err)
	}
	if err := s.SendTyping(ctxWithTestTimeout(t), target); err != nil {
		t.Fatalf("SendTyping: %v", err)
	}

	if len(calls) != 2 {
		t.Fatalf("calls = %d, want 2", len(calls))
	}
	if calls[0].Path != "/v1/bot/readReceipt" {
		t.Fatalf("first path = %q, want readReceipt", calls[0].Path)
	}
	if calls[1].Path != "/v1/bot/typing" {
		t.Fatalf("second path = %q, want typing", calls[1].Path)
	}
	if calls[0].Body["channel_id"] != "group-1" || int(calls[0].Body["channel_type"].(float64)) != int(octoChannelTypeGroup) {
		t.Fatalf("readReceipt target = %#v", calls[0].Body)
	}
	ids, ok := calls[0].Body["message_ids"].([]any)
	if !ok || len(ids) != 1 || ids[0] != "9001" {
		t.Fatalf("readReceipt message_ids = %#v", calls[0].Body["message_ids"])
	}
	if calls[1].Body["channel_id"] != "group-1" || int(calls[1].Body["channel_type"].(float64)) != int(octoChannelTypeGroup) {
		t.Fatalf("typing target = %#v", calls[1].Body)
	}
	if _, ok := calls[1].Body["message_ids"]; ok {
		t.Fatalf("typing body must not carry message_ids: %#v", calls[1].Body)
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

func TestOctoChannelSendUsesThreadIDAsCommunityTopic(t *testing.T) {
	var got struct {
		ChannelID   string          `json:"channel_id"`
		ChannelType octoChannelType `json:"channel_type"`
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"message_id":"m-1","client_msg_no":"c-1","message_seq":1}`))
	}))
	defer srv.Close()

	ch := &octoChannel{
		sender: newOctoSender(credentials{APIURL: srv.URL, IMToken: "im-token"}, srv.Client(), nil),
	}
	_, err := ch.Send(ctxWithTestTimeout(t), channel.OutboundMessage{
		ChatID:   "group-1",
		ThreadID: "group-1____topic-1",
		Text:     "hello thread",
	})
	if err != nil {
		t.Fatalf("channel Send: %v", err)
	}
	if got.ChannelID != "group-1____topic-1" || got.ChannelType != octoChannelTypeCommunityTopic {
		t.Fatalf("thread target = %+v", got)
	}
}

func TestOctoChannelCapabilitiesDeclareTypingAndReplyTargets(t *testing.T) {
	ch := &octoChannel{}
	caps := ch.Capabilities()
	want := channel.CapText | channel.CapThreadReply | channel.CapQuoteReply | channel.CapTypingIndicator
	if !caps.Has(want) {
		t.Fatalf("capabilities = %s, want at least %s", caps, want)
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
