package octo

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/multica-ai/multica/server/internal/events"
	"github.com/multica-ai/multica/server/internal/integrations/channel"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/multica-ai/multica/server/pkg/protocol"
)

type outboundQueries interface {
	GetChannelChatSessionBindingBySession(ctx context.Context, arg db.GetChannelChatSessionBindingBySessionParams) (db.ChannelChatSessionBinding, error)
	GetChannelInstallation(ctx context.Context, arg db.GetChannelInstallationParams) (db.ChannelInstallation, error)
}

type octoReplySender interface {
	Send(ctx context.Context, out octoOutboundMessage) (channel.SendResult, error)
}

type Outbound struct {
	q         outboundQueries
	decrypt   Decrypter
	logger    *slog.Logger
	newSender func(creds credentials) octoReplySender
}

func NewOutbound(q outboundQueries, decrypt Decrypter, logger *slog.Logger) *Outbound {
	if logger == nil {
		logger = slog.Default()
	}
	o := &Outbound{q: q, decrypt: decrypt, logger: logger}
	o.newSender = func(c credentials) octoReplySender {
		return newOctoSender(c, nil, logger)
	}
	return o
}

func (o *Outbound) Register(bus *events.Bus) {
	bus.Subscribe(protocol.EventTaskFailed, o.handleEvent)
	bus.Subscribe(protocol.EventChatDone, o.handleEvent)
}

func (o *Outbound) handleEvent(e events.Event) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := o.processEvent(ctx, e); err != nil {
		o.logger.WarnContext(ctx, "octo outbound: reply delivery failed",
			"error", err, "chat_session_id", e.ChatSessionID)
	}
}

func (o *Outbound) processEvent(ctx context.Context, e events.Event) error {
	sessionID, ok := octoChatSessionIDFromEvent(e)
	if !ok {
		return nil
	}
	binding, err := o.q.GetChannelChatSessionBindingBySession(ctx, db.GetChannelChatSessionBindingBySessionParams{
		ChatSessionID: sessionID,
		ChannelType:   string(TypeOcto),
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		return fmt.Errorf("lookup octo chat binding: %w", err)
	}
	content := outboundEventContent(e)
	if content == "" {
		return nil
	}
	inst, err := o.q.GetChannelInstallation(ctx, db.GetChannelInstallationParams{
		ID:          binding.InstallationID,
		ChannelType: string(TypeOcto),
	})
	if err != nil {
		return fmt.Errorf("load octo installation: %w", err)
	}
	if inst.Status != "active" {
		return nil
	}
	creds, err := decodeCredentials(inst.Config, o.decrypt)
	if err != nil {
		return fmt.Errorf("decode octo credentials: %w", err)
	}
	target := outboundTarget(binding)
	if _, err := o.newSender(creds).Send(ctx, octoOutboundMessage{
		ChannelID:   target.ChannelID,
		ChannelType: target.ChannelType,
		Text:        content,
		ReplyTo:     target.ReplyTo,
	}); err != nil {
		return fmt.Errorf("post octo reply: %w", err)
	}
	return nil
}

const taskFailedText = "The agent run failed before it could reply. Please try again."

func outboundEventContent(e events.Event) string {
	switch e.Type {
	case protocol.EventChatDone:
		return chatDoneContent(e.Payload)
	case protocol.EventTaskFailed:
		return taskFailedText
	default:
		return ""
	}
}

type octoOutboundTarget struct {
	ChannelID   string
	ChannelType octoChannelType
	ReplyTo     string
}

func outboundTarget(b db.ChannelChatSessionBinding) octoOutboundTarget {
	target := octoOutboundTarget{
		ChannelID:   b.ChannelChatID,
		ChannelType: octoChannelTypeGroup,
	}
	if b.ChatType == string(channel.ChatTypeP2P) {
		target.ChannelType = octoChannelTypeDM
	}
	if len(b.Config) > 0 {
		var cfg octoBindingConfig
		if err := json.Unmarshal(b.Config, &cfg); err == nil {
			if cfg.ChannelID != "" {
				target.ChannelID = cfg.ChannelID
			}
			if cfg.ChannelType != 0 {
				target.ChannelType = cfg.ChannelType
			}
		}
	}
	if b.LastMessageID.Valid {
		target.ReplyTo = b.LastMessageID.String
	}
	return target
}

func chatDoneContent(payload any) string {
	switch p := payload.(type) {
	case protocol.ChatDonePayload:
		return p.Content
	case map[string]any:
		if s, ok := p["content"].(string); ok {
			return s
		}
	}
	return ""
}
