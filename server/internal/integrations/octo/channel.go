package octo

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"

	"github.com/gorilla/websocket"

	"github.com/multica-ai/multica/server/internal/integrations/channel"
)

type credentials struct {
	RobotID string
	IMToken string
	APIURL  string
	WSURL   string
}

type octoChannel struct {
	sender  *octoSender
	handler channel.InboundHandler
	run     func(context.Context) error
	logger  *slog.Logger
}

func (c *octoChannel) Type() channel.Type { return TypeOcto }

func (c *octoChannel) Capabilities() channel.Capability {
	return channel.CapText | channel.CapThreadReply | channel.CapQuoteReply | channel.CapTypingIndicator
}

func (c *octoChannel) Connect(ctx context.Context) error {
	if c.run == nil {
		<-ctx.Done()
		return nil
	}
	return c.run(ctx)
}

func (c *octoChannel) Disconnect(ctx context.Context) error { return nil }

func (c *octoChannel) Send(ctx context.Context, out channel.OutboundMessage) (channel.SendResult, error) {
	if c.sender == nil {
		return channel.SendResult{}, errors.New("octo: sender not configured")
	}
	channelID := out.ChatID
	channelType := octoChannelTypeDM
	if out.ThreadID != "" {
		channelID = out.ThreadID
		channelType = octoChannelTypeCommunityTopic
	}
	return c.sender.Send(ctx, octoOutboundMessage{
		ChannelID:   channelID,
		ChannelType: channelType,
		Text:        out.Text,
		ReplyTo:     out.ReplyTo,
	})
}

type ChannelDeps struct {
	Decrypt    Decrypter
	HTTPClient *http.Client
	Dialer     *websocket.Dialer
	Logger     *slog.Logger
}

func RegisterOcto(reg *channel.Registry, deps ChannelDeps) {
	reg.Register(TypeOcto, newOctoFactory(deps))
}

func newOctoFactory(deps ChannelDeps) channel.Factory {
	logger := deps.Logger
	if logger == nil {
		logger = slog.Default()
	}
	return func(cfg channel.Config) (channel.Channel, error) {
		var ic installConfig
		if err := json.Unmarshal(cfg.Raw, &ic); err != nil {
			return nil, fmt.Errorf("octo: decode installation config: %w", err)
		}
		creds, err := decodeCredentials(cfg.Raw, deps.Decrypt)
		if err != nil {
			return nil, err
		}
		if creds.RobotID == "" {
			return nil, errors.New("octo: installation has no robot id")
		}
		if creds.IMToken == "" {
			return nil, errors.New("octo: installation has no im token")
		}
		if creds.WSURL == "" {
			return nil, errors.New("octo: installation has no ws url")
		}
		sender := newOctoSender(creds, deps.HTTPClient, logger)
		oc := &octoChannel{
			sender:  sender,
			handler: cfg.Handler,
			logger:  logger,
		}
		ws := newWKSocketClient(wkSocketConfig{
			WSURL: creds.WSURL,
			UID:   creds.RobotID,
			Token: creds.IMToken,
			Handler: func(ctx context.Context, msg botMessage) error {
				inbound, ok := inboundFromBotMessage(msg, creds.RobotID)
				if !ok {
					return nil
				}
				if cfg.Handler == nil {
					return errors.New("octo: inbound handler not configured")
				}
				return cfg.Handler(ctx, inbound)
			},
			Logger: logger,
			Dialer: deps.Dialer,
		})
		oc.run = ws.Run
		_ = ic
		return oc, nil
	}
}
