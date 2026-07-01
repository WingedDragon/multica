package octo

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/multica-ai/multica/server/internal/events"
	"github.com/multica-ai/multica/server/internal/integrations/channel"
	"github.com/multica-ai/multica/server/internal/integrations/channel/engine"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/multica-ai/multica/server/pkg/protocol"
)

const (
	defaultTypingInterval = 5 * time.Second
	typingCallTimeout     = 2500 * time.Millisecond
)

type octoInteractionSender interface {
	SendTyping(ctx context.Context, target octoInteractionTarget) error
	SendReadReceipt(ctx context.Context, target octoInteractionTarget) error
}

// TypingIndicatorQueries is reserved to keep the constructor wiring aligned
// with other IM typing managers; Octo currently needs only installation config.
type TypingIndicatorQueries interface{}

type typingState struct {
	cancel context.CancelFunc
}

type TypingIndicatorManager struct {
	decrypt Decrypter
	log     *slog.Logger

	newSender      func(credentials) octoInteractionSender
	typingInterval time.Duration

	mu     sync.Mutex
	states map[string][]typingState
}

func NewTypingIndicatorManager(_ TypingIndicatorQueries, decrypt Decrypter, logger *slog.Logger) *TypingIndicatorManager {
	if logger == nil {
		logger = slog.Default()
	}
	m := &TypingIndicatorManager{
		decrypt:        decrypt,
		log:            logger,
		typingInterval: defaultTypingInterval,
		states:         make(map[string][]typingState),
	}
	m.newSender = func(c credentials) octoInteractionSender {
		return newOctoSender(c, nil, logger)
	}
	return m
}

func (m *TypingIndicatorManager) Register(bus *events.Bus) {
	bus.Subscribe(protocol.EventChatDone, m.handleEvent)
	bus.Subscribe(protocol.EventTaskFailed, m.handleEvent)
}

func (m *TypingIndicatorManager) OnIngested(ctx context.Context, inst engine.ResolvedInstallation, msg channel.InboundMessage, sessionID pgtype.UUID) {
	row, ok := inst.Platform.(db.ChannelInstallation)
	if !ok {
		return
	}
	creds, err := decodeCredentials(row.Config, m.decrypt)
	if err != nil {
		m.log.WarnContext(ctx, "octo typing indicator: decode credentials failed",
			"chat_session_id", util.UUIDToString(sessionID), "error", err)
		return
	}
	target := octoInteractionTarget{
		ChannelID:   msg.Source.ChatID,
		ChannelType: octoChannelTypeFromMessage(msg),
	}
	if msg.MessageID != "" {
		target.MessageIDs = []string{msg.MessageID}
	}
	if target.ChannelID == "" {
		return
	}
	sender := m.newSender(creds)

	loopCtx, cancel := context.WithCancel(context.Background())
	key := util.UUIDToString(sessionID)
	m.mu.Lock()
	m.states[key] = append(m.states[key], typingState{cancel: cancel})
	m.mu.Unlock()

	m.sendReadReceipt(ctx, sender, target, sessionID)
	m.sendTyping(ctx, sender, target, sessionID)
	go m.repeatTyping(loopCtx, sender, target, sessionID)
}

func (m *TypingIndicatorManager) OnSettled(ctx context.Context, sessionID pgtype.UUID) {
	m.Clear(ctx, sessionID)
}

func (m *TypingIndicatorManager) Clear(_ context.Context, sessionID pgtype.UUID) {
	key := util.UUIDToString(sessionID)
	m.mu.Lock()
	states := m.states[key]
	delete(m.states, key)
	m.mu.Unlock()
	for _, state := range states {
		if state.cancel != nil {
			state.cancel()
		}
	}
}

func (m *TypingIndicatorManager) handleEvent(e events.Event) {
	sessionID, ok := octoChatSessionIDFromEvent(e)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), typingCallTimeout)
	defer cancel()
	m.Clear(ctx, sessionID)
}

func (m *TypingIndicatorManager) repeatTyping(ctx context.Context, sender octoInteractionSender, target octoInteractionTarget, sessionID pgtype.UUID) {
	ticker := time.NewTicker(m.typingInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			callCtx, cancel := context.WithTimeout(context.Background(), typingCallTimeout)
			m.sendTyping(callCtx, sender, target, sessionID)
			cancel()
		}
	}
}

func (m *TypingIndicatorManager) sendTyping(ctx context.Context, sender octoInteractionSender, target octoInteractionTarget, sessionID pgtype.UUID) {
	if err := sender.SendTyping(ctx, target); err != nil {
		m.log.WarnContext(ctx, "octo typing indicator: send typing failed",
			"chat_session_id", util.UUIDToString(sessionID), "error", err)
	}
}

func (m *TypingIndicatorManager) sendReadReceipt(ctx context.Context, sender octoInteractionSender, target octoInteractionTarget, sessionID pgtype.UUID) {
	if err := sender.SendReadReceipt(ctx, target); err != nil {
		m.log.WarnContext(ctx, "octo typing indicator: send read receipt failed",
			"chat_session_id", util.UUIDToString(sessionID), "error", err)
	}
}

func octoChatSessionIDFromEvent(e events.Event) (pgtype.UUID, bool) {
	if e.ChatSessionID != "" {
		if id, err := util.ParseUUID(e.ChatSessionID); err == nil && id.Valid {
			return id, true
		}
	}
	if m, ok := e.Payload.(map[string]any); ok {
		if s, _ := m["chat_session_id"].(string); s != "" {
			if id, err := util.ParseUUID(s); err == nil && id.Valid {
				return id, true
			}
		}
	}
	if p, ok := e.Payload.(protocol.ChatDonePayload); ok && p.ChatSessionID != "" {
		if id, err := util.ParseUUID(p.ChatSessionID); err == nil && id.Valid {
			return id, true
		}
	}
	return pgtype.UUID{}, false
}

var _ engine.TypingNotifier = (*TypingIndicatorManager)(nil)
