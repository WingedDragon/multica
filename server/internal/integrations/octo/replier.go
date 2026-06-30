package octo

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"strings"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/multica-ai/multica/server/internal/integrations/channel"
	"github.com/multica-ai/multica/server/internal/integrations/channel/engine"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

const (
	agentOfflineText  = "The agent is offline right now. Your message was received and will be handled once it is back online."
	agentArchivedText = "This agent has been archived and cannot respond. Please contact your workspace admin."
)

type bindingMinter interface {
	Mint(ctx context.Context, workspaceID, installationID pgtype.UUID, octoUserID string) (BindingToken, error)
}

type OutboundReplier struct {
	binding     bindingMinter
	decrypt     Decrypter
	newSender   func(creds credentials) octoReplySender
	appURL      string
	bindingPath string
	logger      *slog.Logger
}

type OutboundReplierConfig struct {
	Binding     bindingMinter
	Decrypt     Decrypter
	AppURL      string
	BindingPath string
	Logger      *slog.Logger
}

var _ engine.OutboundReplier = (*OutboundReplier)(nil)

func NewOutboundReplier(cfg OutboundReplierConfig) *OutboundReplier {
	logger := cfg.Logger
	if logger == nil {
		logger = slog.Default()
	}
	bindingPath := cfg.BindingPath
	if bindingPath == "" {
		bindingPath = "/octo/bind"
	}
	if !strings.HasPrefix(bindingPath, "/") {
		bindingPath = "/" + bindingPath
	}
	r := &OutboundReplier{
		binding:     cfg.Binding,
		decrypt:     cfg.Decrypt,
		appURL:      strings.TrimRight(cfg.AppURL, "/"),
		bindingPath: bindingPath,
		logger:      logger,
	}
	r.newSender = func(c credentials) octoReplySender {
		return newOctoSender(c, nil, logger)
	}
	return r
}

func (r *OutboundReplier) Reply(ctx context.Context, inst engine.ResolvedInstallation, msg channel.InboundMessage, res engine.Result) {
	switch res.Outcome {
	case engine.OutcomeNeedsBinding:
		if err := r.sendBindingPrompt(ctx, inst, msg, res); err != nil {
			r.logger.WarnContext(ctx, "octo replier: binding prompt failed",
				"installation_id", util.UUIDToString(inst.ID), "error", err)
		}
	case engine.OutcomeAgentOffline:
		if err := r.post(ctx, inst, msg, agentOfflineText); err != nil {
			r.logger.WarnContext(ctx, "octo replier: offline notice failed",
				"installation_id", util.UUIDToString(inst.ID), "error", err)
		}
	case engine.OutcomeAgentArchived:
		if err := r.post(ctx, inst, msg, agentArchivedText); err != nil {
			r.logger.WarnContext(ctx, "octo replier: archived notice failed",
				"installation_id", util.UUIDToString(inst.ID), "error", err)
		}
	case engine.OutcomeIngested:
		if res.IssueID.Valid {
			if err := r.post(ctx, inst, msg, issueCreatedText(res)); err != nil {
				r.logger.WarnContext(ctx, "octo replier: issue-created confirmation failed",
					"installation_id", util.UUIDToString(inst.ID), "error", err)
			}
		}
	}
}

func (r *OutboundReplier) sendBindingPrompt(ctx context.Context, inst engine.ResolvedInstallation, msg channel.InboundMessage, res engine.Result) error {
	sender := res.Sender
	if sender == "" {
		sender = msg.Source.SenderID
	}
	if sender == "" {
		return errors.New("missing sender id")
	}
	if r.binding == nil {
		return errors.New("binding service not configured")
	}
	if r.appURL == "" {
		return errors.New("app url not configured")
	}
	token, err := r.binding.Mint(ctx, inst.WorkspaceID, inst.ID, sender)
	if err != nil {
		return fmt.Errorf("mint binding token: %w", err)
	}
	bindURL := r.appURL + r.bindingPath + "?token=" + url.QueryEscape(token.Raw)
	text := "To start chatting with me, link your Octo account to Multica: " + bindURL + "\nThis link expires in 15 minutes."
	return r.post(ctx, inst, msg, text)
}

func (r *OutboundReplier) post(ctx context.Context, inst engine.ResolvedInstallation, msg channel.InboundMessage, text string) error {
	row, ok := inst.Platform.(db.ChannelInstallation)
	if !ok {
		return errors.New("installation platform row unavailable")
	}
	creds, err := decodeCredentials(row.Config, r.decrypt)
	if err != nil {
		return fmt.Errorf("decode credentials: %w", err)
	}
	if _, err := r.newSender(creds).Send(ctx, octoOutboundMessage{
		ChannelID:   msg.Source.ChatID,
		ChannelType: octoChannelTypeFromMessage(msg),
		Text:        text,
	}); err != nil {
		return fmt.Errorf("post octo reply: %w", err)
	}
	return nil
}

func issueCreatedText(res engine.Result) string {
	id := res.IssueIdentifier
	if id == "" {
		id = fmt.Sprintf("#%d", res.IssueNumber)
	}
	title := strings.TrimSpace(res.IssueTitle)
	if title == "" {
		return "Created " + id
	}
	return "Created " + id + " - " + title
}
