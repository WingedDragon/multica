package octo

import (
	"context"
	"testing"

	"github.com/multica-ai/multica/server/internal/integrations/channel"
	"github.com/multica-ai/multica/server/internal/integrations/channel/engine"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

func TestOutboundReplierQuotesInboundMessage(t *testing.T) {
	fs := &fakeOctoSender{}
	r := NewOutboundReplier(OutboundReplierConfig{})
	r.newSender = func(credentials) octoReplySender { return fs }

	r.Reply(context.Background(), engine.ResolvedInstallation{
		ID:       testUUID(1),
		Platform: db.ChannelInstallation{ID: testUUID(1), Status: "active", Config: octoInstallConfigJSON()},
	}, channel.InboundMessage{
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
	}, engine.Result{Outcome: engine.OutcomeAgentOffline})

	if fs.called != 1 {
		t.Fatalf("sender called %d times, want 1", fs.called)
	}
	if fs.got.ReplyTo != "9001" {
		t.Fatalf("replier ReplyTo = %q, want inbound message id", fs.got.ReplyTo)
	}
}
