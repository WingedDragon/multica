package octo

import (
	"encoding/json"
	"testing"

	"github.com/multica-ai/multica/server/internal/integrations/channel"
)

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

func mustRaw(t *testing.T, v any) json.RawMessage {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal raw: %v", err)
	}
	return raw
}
