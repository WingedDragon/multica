package octo

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	"github.com/multica-ai/multica/server/internal/integrations/channel"
)

func inboundFromBotMessage(m botMessage, robotID string) (channel.InboundMessage, bool) {
	if m.MessageID == "" || m.FromUID == "" {
		return channel.InboundMessage{}, false
	}
	if robotID != "" && m.FromUID == robotID {
		return channel.InboundMessage{}, false
	}
	msgType, ok := normalizeMessageType(m.Payload.Type)
	if !ok {
		return channel.InboundMessage{}, false
	}

	chatType := normalizeChatType(m.ChannelType)
	addressed := chatType == channel.ChatTypeP2P || mentionedOctoBot(m.Payload.Mention, robotID)
	text := stripMentionEntities(resolveMessageText(m.Payload), m.Payload.Mention, robotID)
	raw, _ := json.Marshal(octoRawMessage{
		RobotID:     robotID,
		ChannelType: m.ChannelType,
		MessageSeq:  m.MessageSeq,
		Timestamp:   m.Timestamp,
	})

	return channel.InboundMessage{
		EventID:        m.MessageID,
		MessageID:      m.MessageID,
		Type:           msgType,
		Text:           text,
		ReplyTo:        replyCtxFromPayload(m.Payload),
		AddressedToBot: addressed,
		Source: channel.Source{
			ChannelType: TypeOcto,
			ChatID:      m.ChannelID,
			ChatType:    chatType,
			SenderID:    m.FromUID,
		},
		Raw: raw,
	}, true
}

func replyCtxFromPayload(p messagePayload) *channel.ReplyCtx {
	if p.Reply == nil || p.Reply.MessageID == "" {
		return nil
	}
	return &channel.ReplyCtx{MessageID: p.Reply.MessageID}
}

func normalizeChatType(t octoChannelType) channel.ChatType {
	if t == octoChannelTypeDM {
		return channel.ChatTypeP2P
	}
	return channel.ChatTypeGroup
}

func normalizeMessageType(t int) (channel.MsgType, bool) {
	switch t {
	case octoMessageTypeText, octoMessageTypeMultipleForward, octoMessageTypeRichText:
		return channel.MsgTypeText, true
	case octoMessageTypeImage, octoMessageTypeGIF:
		return channel.MsgTypeImage, true
	case octoMessageTypeVoice:
		return channel.MsgTypeAudio, true
	case octoMessageTypeVideo:
		return channel.MsgTypeVideo, true
	case octoMessageTypeFile:
		return channel.MsgTypeFile, true
	default:
		return channel.MsgTypeUnknown, false
	}
}

func mentionedOctoBot(m *mentionPayload, robotID string) bool {
	if m == nil {
		return false
	}
	if robotID != "" {
		for _, uid := range m.UIDs {
			if uid == robotID {
				return true
			}
		}
	}
	isHumanBroadcast := truthyOctoFlag(m.All) || truthyOctoFlag(m.Humans)
	return !isHumanBroadcast && truthyOctoFlag(m.AIs)
}

func truthyOctoFlag(v any) bool {
	switch x := v.(type) {
	case bool:
		return x
	case int:
		return x == 1
	case int32:
		return x == 1
	case int64:
		return x == 1
	case float64:
		return x == 1
	case json.Number:
		i, err := x.Int64()
		return err == nil && i == 1
	case string:
		return x == "1" || strings.EqualFold(x, "true")
	default:
		return false
	}
}

func resolveMessageText(p messagePayload) string {
	switch p.Type {
	case octoMessageTypeText:
		return stringContent(p.Content)
	case octoMessageTypeImage:
		return mediaText("[图片]", p.URL)
	case octoMessageTypeGIF:
		return mediaText("[GIF]", p.URL)
	case octoMessageTypeVoice:
		return mediaText("[语音消息]", p.URL)
	case octoMessageTypeVideo:
		return mediaText("[视频]", p.URL)
	case octoMessageTypeFile:
		name := p.Name
		if name == "" {
			name = octoUnknownFileName
		}
		return mediaText(fmt.Sprintf("[文件: %s]", name), p.URL)
	case octoMessageTypeMultipleForward:
		return resolveMultipleForwardText(p)
	case octoMessageTypeRichText:
		return resolveRichTextPlain(p)
	default:
		return stringContent(p.Content)
	}
}

func stringContent(v any) string {
	s, _ := v.(string)
	return s
}

func mediaText(label, url string) string {
	if url == "" {
		return label
	}
	return label + "\n" + url
}

func resolveRichTextPlain(p messagePayload) string {
	if strings.TrimSpace(p.Plain) != "" {
		return p.Plain
	}
	var out strings.Builder
	for _, block := range normalizeRichTextBlocks(p.Content) {
		switch block.Type {
		case richTextBlockImage:
			out.WriteString(richTextImagePlaceholder)
		case richTextBlockText:
			out.WriteString(block.Text)
		default:
			out.WriteString(block.Text)
		}
	}
	return out.String()
}

func normalizeRichTextBlocks(v any) []richTextBlock {
	switch x := v.(type) {
	case []richTextBlock:
		return x
	case []any:
		blocks := make([]richTextBlock, 0, len(x))
		for _, item := range x {
			if m, ok := item.(map[string]any); ok {
				blocks = append(blocks, richTextBlock{
					Type: stringMapValue(m, "type"),
					Text: stringMapValue(m, "text"),
					URL:  stringMapValue(m, "url"),
					Name: stringMapValue(m, "name"),
				})
			}
		}
		return blocks
	case string:
		if x == "" {
			return nil
		}
		return []richTextBlock{{Type: richTextBlockText, Text: x}}
	default:
		return nil
	}
}

func stringMapValue(m map[string]any, key string) string {
	v, _ := m[key].(string)
	return v
}

func resolveMultipleForwardText(p messagePayload) string {
	if len(p.Msgs) == 0 {
		return octoMultipleForwardText
	}
	names := make(map[string]string, len(p.Users))
	for _, user := range p.Users {
		if user.UID != "" && user.Name != "" {
			names[user.UID] = user.Name
		}
	}
	lines := []string{octoMultipleForwardRecordText}
	for _, msg := range p.Msgs {
		name := msg.FromUID
		if names[msg.FromUID] != "" {
			name = names[msg.FromUID]
		}
		lines = append(lines, name+": "+resolveMessageText(msg.Payload))
	}
	return strings.Join(lines, "\n")
}

func stripMentionEntities(text string, mention *mentionPayload, robotID string) string {
	if mention == nil || robotID == "" || text == "" {
		return strings.TrimSpace(text)
	}
	var ranges []utf16Range
	for _, entity := range mention.Entities {
		if entity.UID == robotID && entity.Length > 0 {
			ranges = append(ranges, utf16Range{start: entity.Offset, end: entity.Offset + entity.Length})
		}
	}
	if len(ranges) == 0 {
		return strings.TrimSpace(text)
	}

	var out strings.Builder
	pos := 0
	for _, r := range text {
		width := utf16Width(r)
		next := pos + width
		if !overlapsAny(pos, next, ranges) {
			out.WriteRune(r)
		}
		pos = next
	}
	return strings.TrimSpace(out.String())
}

type utf16Range struct {
	start int
	end   int
}

func utf16Width(r rune) int {
	if r <= 0xffff {
		return 1
	}
	return 2
}

func overlapsAny(start, end int, ranges []utf16Range) bool {
	for _, r := range ranges {
		if start < r.end && end > r.start {
			return true
		}
	}
	return false
}

func channelTypeString(t octoChannelType) string {
	return strconv.Itoa(int(t))
}
