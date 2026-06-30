package octo

import (
	"encoding/json"
	"testing"

	"github.com/multica-ai/multica/server/internal/integrations/channel"
)

func TestInboundFromBotMessageDM(t *testing.T) {
	msg, ok := inboundFromBotMessage(botMessage{
		MessageID:   "9001",
		MessageSeq:  7,
		FromUID:     "u-alice",
		ChannelID:   "u-alice@r-bot",
		ChannelType: octoChannelTypeDM,
		Timestamp:   1700000000,
		Payload: messagePayload{
			Type:    octoMessageTypeText,
			Content: "hello octo",
		},
	}, "r-bot")
	if !ok {
		t.Fatal("expected DM text to be ingestable")
	}
	if msg.Source.ChannelType != TypeOcto {
		t.Errorf("ChannelType = %q, want octo", msg.Source.ChannelType)
	}
	if msg.Source.ChatType != channel.ChatTypeP2P {
		t.Errorf("ChatType = %q, want p2p", msg.Source.ChatType)
	}
	if !msg.AddressedToBot {
		t.Error("DM should always be addressed to bot")
	}
	if msg.MessageID != "9001" || msg.EventID != "9001" {
		t.Errorf("MessageID/EventID = %q/%q, want message id", msg.MessageID, msg.EventID)
	}
	if msg.Source.SenderID != "u-alice" || msg.Source.ChatID != "u-alice@r-bot" {
		t.Errorf("sender/chat = %q/%q", msg.Source.SenderID, msg.Source.ChatID)
	}
	if msg.Text != "hello octo" {
		t.Errorf("Text = %q", msg.Text)
	}

	var raw octoRawMessage
	if err := json.Unmarshal(msg.Raw, &raw); err != nil {
		t.Fatalf("decode raw: %v", err)
	}
	if raw.RobotID != "r-bot" || raw.ChannelType != octoChannelTypeDM || raw.MessageSeq != 7 {
		t.Errorf("raw = %+v", raw)
	}
}

func TestInboundFromBotMessageGroupMentionSemantics(t *testing.T) {
	tests := []struct {
		name    string
		mention *mentionPayload
		want    bool
	}{
		{name: "no mention", mention: nil, want: false},
		{name: "explicit bot uid", mention: &mentionPayload{UIDs: []string{"r-bot"}}, want: true},
		{name: "ai only", mention: &mentionPayload{AIs: 1}, want: true},
		{name: "all humans only", mention: &mentionPayload{All: 1}, want: false},
		{name: "humans suppress ai broadcast", mention: &mentionPayload{All: 1, AIs: 1}, want: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			msg, ok := inboundFromBotMessage(botMessage{
				MessageID:   "9002",
				MessageSeq:  8,
				FromUID:     "u-alice",
				ChannelID:   "group-1",
				ChannelType: octoChannelTypeGroup,
				Timestamp:   1700000001,
				Payload: messagePayload{
					Type:    octoMessageTypeText,
					Content: "create an issue",
					Mention: tt.mention,
				},
			}, "r-bot")
			if !ok {
				t.Fatal("group text should be ingestable; engine decides whether to drop")
			}
			if msg.Source.ChatType != channel.ChatTypeGroup {
				t.Errorf("ChatType = %q, want group", msg.Source.ChatType)
			}
			if msg.AddressedToBot != tt.want {
				t.Errorf("AddressedToBot = %v, want %v", msg.AddressedToBot, tt.want)
			}
		})
	}
}

func TestInboundFromBotMessageStripsMentionEntities(t *testing.T) {
	msg, ok := inboundFromBotMessage(botMessage{
		MessageID:   "9003",
		MessageSeq:  9,
		FromUID:     "u-alice",
		ChannelID:   "group-1",
		ChannelType: octoChannelTypeGroup,
		Timestamp:   1700000002,
		Payload: messagePayload{
			Type:    octoMessageTypeText,
			Content: "@Multica 处理这个问题",
			Mention: &mentionPayload{
				UIDs:     []string{"r-bot"},
				Entities: []mentionEntity{{UID: "r-bot", Offset: 0, Length: 8}},
			},
		},
	}, "r-bot")
	if !ok {
		t.Fatal("expected group mention to be ingestable")
	}
	if msg.Text != "处理这个问题" {
		t.Errorf("Text = %q, want mention-stripped content", msg.Text)
	}
}

func TestInboundFromBotMessageRichTextUsesPlain(t *testing.T) {
	msg, ok := inboundFromBotMessage(botMessage{
		MessageID:   "9004",
		MessageSeq:  10,
		FromUID:     "u-alice",
		ChannelID:   "u-alice@r-bot",
		ChannelType: octoChannelTypeDM,
		Timestamp:   1700000003,
		Payload: messagePayload{
			Type: octoMessageTypeRichText,
			Content: []richTextBlock{
				{Type: richTextBlockText, Text: "ignored when plain exists"},
			},
			Plain: "hello [图片]",
		},
	}, "r-bot")
	if !ok {
		t.Fatal("expected rich text to be ingestable")
	}
	if msg.Text != "hello [图片]" {
		t.Errorf("Text = %q, want plain rich text", msg.Text)
	}
}

func TestInboundFromBotMessageSkipsSelfAndUnsupportedTypes(t *testing.T) {
	if _, ok := inboundFromBotMessage(botMessage{
		MessageID:   "9005",
		FromUID:     "r-bot",
		ChannelID:   "u-alice@r-bot",
		ChannelType: octoChannelTypeDM,
		Payload:     messagePayload{Type: octoMessageTypeText, Content: "echo"},
	}, "r-bot"); ok {
		t.Error("self messages should be skipped")
	}
	if _, ok := inboundFromBotMessage(botMessage{
		MessageID:   "9006",
		FromUID:     "u-alice",
		ChannelID:   "u-alice@r-bot",
		ChannelType: octoChannelTypeDM,
		Payload:     messagePayload{Type: 99, Content: "unknown"},
	}, "r-bot"); ok {
		t.Error("unsupported message types should be skipped")
	}
}
