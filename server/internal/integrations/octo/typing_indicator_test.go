package octo

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/multica-ai/multica/server/internal/events"
	"github.com/multica-ai/multica/server/internal/integrations/channel"
	"github.com/multica-ai/multica/server/internal/integrations/channel/engine"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/multica-ai/multica/server/pkg/protocol"
)

type fakeOctoInteractionSender struct {
	mu           sync.Mutex
	typing       []octoInteractionTarget
	readReceipts []octoInteractionTarget
}

func (f *fakeOctoInteractionSender) SendTyping(_ context.Context, target octoInteractionTarget) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.typing = append(f.typing, target)
	return nil
}

func (f *fakeOctoInteractionSender) SendReadReceipt(_ context.Context, target octoInteractionTarget) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.readReceipts = append(f.readReceipts, target)
	return nil
}

func (f *fakeOctoInteractionSender) counts() (typing int, receipts int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.typing), len(f.readReceipts)
}

func (f *fakeOctoInteractionSender) firstReadReceipt() octoInteractionTarget {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.readReceipts) == 0 {
		return octoInteractionTarget{}
	}
	return f.readReceipts[0]
}

func TestTypingIndicatorSendsReadReceiptAndRepeatsTypingUntilSettled(t *testing.T) {
	sessionID := testUUID(7)
	sender := &fakeOctoInteractionSender{}
	mgr := NewTypingIndicatorManager(&fakeOctoOutboundQueries{}, nil, nil)
	mgr.newSender = func(credentials) octoInteractionSender { return sender }
	mgr.typingInterval = 10 * time.Millisecond

	msg := channel.InboundMessage{
		MessageID: "9001",
		Source: channel.Source{
			ChannelType: TypeOcto,
			ChatID:      "group-1",
			ChatType:    channel.ChatTypeGroup,
		},
		Raw: mustRaw(t, octoRawMessage{
			RobotID:     "robot-1",
			ChannelType: octoChannelTypeGroup,
		}),
	}
	inst := engine.ResolvedInstallation{
		ID:       testUUID(1),
		Platform: db.ChannelInstallation{ID: testUUID(1), Config: octoInstallConfigJSON()},
	}

	mgr.OnIngested(context.Background(), inst, msg, sessionID)

	if !waitForCount(t, 200*time.Millisecond, func() bool {
		typing, receipts := sender.counts()
		return typing >= 2 && receipts == 1
	}) {
		typing, receipts := sender.counts()
		t.Fatalf("typing/readReceipt calls = %d/%d, want at least 2/1", typing, receipts)
	}
	receipt := sender.firstReadReceipt()
	if receipt.ChannelID != "group-1" || receipt.ChannelType != octoChannelTypeGroup {
		t.Fatalf("read receipt target = %+v", receipt)
	}
	if len(receipt.MessageIDs) != 1 || receipt.MessageIDs[0] != "9001" {
		t.Fatalf("read receipt message ids = %+v", receipt.MessageIDs)
	}

	mgr.OnSettled(context.Background(), sessionID)
	typingBefore, _ := sender.counts()
	time.Sleep(35 * time.Millisecond)
	typingAfter, _ := sender.counts()
	if typingAfter != typingBefore {
		t.Fatalf("typing ticker kept running after settle: before=%d after=%d", typingBefore, typingAfter)
	}
}

func TestTypingIndicatorClearsOnTaskFailedEvent(t *testing.T) {
	sessionID := testUUID(8)
	sender := &fakeOctoInteractionSender{}
	mgr := NewTypingIndicatorManager(&fakeOctoOutboundQueries{}, nil, nil)
	mgr.newSender = func(credentials) octoInteractionSender { return sender }
	mgr.typingInterval = 10 * time.Millisecond

	mgr.OnIngested(context.Background(), engine.ResolvedInstallation{
		ID:       testUUID(1),
		Platform: db.ChannelInstallation{ID: testUUID(1), Config: octoInstallConfigJSON()},
	}, channel.InboundMessage{
		MessageID: "9002",
		Source: channel.Source{
			ChannelType: TypeOcto,
			ChatID:      "group-1",
			ChatType:    channel.ChatTypeGroup,
		},
	}, sessionID)

	if !waitForCount(t, 200*time.Millisecond, func() bool {
		typing, _ := sender.counts()
		return typing >= 2
	}) {
		typing, _ := sender.counts()
		t.Fatalf("typing calls = %d, want at least 2", typing)
	}
	mgr.handleEvent(events.Event{
		Type:    protocol.EventTaskFailed,
		Payload: map[string]any{"chat_session_id": util.UUIDToString(sessionID)},
	})

	typingBefore, _ := sender.counts()
	time.Sleep(35 * time.Millisecond)
	typingAfter, _ := sender.counts()
	if typingAfter != typingBefore {
		t.Fatalf("typing ticker kept running after task failed: before=%d after=%d", typingBefore, typingAfter)
	}
}

func TestOctoResolverSetWiresTypingNotifier(t *testing.T) {
	set := NewOctoResolverSet(nil, nil, nil, NewTypingIndicatorManager(nil, nil, nil))
	if set.Typing == nil {
		t.Fatal("Octo resolver set must expose typing notifier")
	}
}

func waitForCount(t *testing.T, timeout time.Duration, ok func() bool) bool {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if ok() {
			return true
		}
		time.Sleep(5 * time.Millisecond)
	}
	return ok()
}
