package octo

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"

	"github.com/google/uuid"

	"github.com/multica-ai/multica/server/internal/integrations/channel"
)

const octoAPITimeoutBodyLimit = 2048

type octoSender struct {
	creds      credentials
	httpClient *http.Client
	logger     *slog.Logger
}

type octoOutboundMessage struct {
	ChannelID   string
	ChannelType octoChannelType
	Text        string
	ReplyTo     string
}

type octoInteractionTarget struct {
	ChannelID   string
	ChannelType octoChannelType
	MessageIDs  []string
}

type sendMessageRequest struct {
	ChannelID   string          `json:"channel_id"`
	ChannelType octoChannelType `json:"channel_type"`
	Payload     messagePayload  `json:"payload"`
	ClientMsgNo string          `json:"client_msg_no"`
}

func newOctoSender(creds credentials, httpClient *http.Client, logger *slog.Logger) *octoSender {
	if httpClient == nil {
		httpClient = http.DefaultClient
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &octoSender{creds: creds, httpClient: httpClient, logger: logger}
}

func (s *octoSender) Send(ctx context.Context, out octoOutboundMessage) (channel.SendResult, error) {
	if s.creds.APIURL == "" {
		return channel.SendResult{}, errors.New("octo: api url not configured")
	}
	if s.creds.IMToken == "" {
		return channel.SendResult{}, errors.New("octo: im token not configured")
	}
	if strings.TrimSpace(out.ChannelID) == "" {
		return channel.SendResult{}, errors.New("octo: channel id is required")
	}
	reqBody := sendMessageRequest{
		ChannelID:   out.ChannelID,
		ChannelType: out.ChannelType,
		Payload: messagePayload{
			Type:    octoMessageTypeText,
			Content: out.Text,
		},
		ClientMsgNo: uuid.NewString(),
	}
	if out.ReplyTo != "" {
		reqBody.Payload.Reply = &replyPayload{MessageID: out.ReplyTo}
	}
	respBody, err := s.postBotJSON(ctx, "/v1/bot/sendMessage", reqBody)
	if err != nil {
		return channel.SendResult{}, err
	}
	messageID, err := octoMessageIDFromResponse(respBody)
	if err != nil {
		return channel.SendResult{}, err
	}
	return channel.SendResult{MessageID: messageID}, nil
}

func (s *octoSender) SendTyping(ctx context.Context, target octoInteractionTarget) error {
	if err := s.validateInteractionTarget(target); err != nil {
		return err
	}
	_, err := s.postBotJSON(ctx, "/v1/bot/typing", map[string]any{
		"channel_id":   target.ChannelID,
		"channel_type": target.ChannelType,
	})
	return err
}

func (s *octoSender) SendReadReceipt(ctx context.Context, target octoInteractionTarget) error {
	if err := s.validateInteractionTarget(target); err != nil {
		return err
	}
	body := map[string]any{
		"channel_id":   target.ChannelID,
		"channel_type": target.ChannelType,
	}
	if len(target.MessageIDs) > 0 {
		body["message_ids"] = target.MessageIDs
	}
	_, err := s.postBotJSON(ctx, "/v1/bot/readReceipt", body)
	return err
}

func (s *octoSender) validateInteractionTarget(target octoInteractionTarget) error {
	if s.creds.APIURL == "" {
		return errors.New("octo: api url not configured")
	}
	if s.creds.IMToken == "" {
		return errors.New("octo: im token not configured")
	}
	if strings.TrimSpace(target.ChannelID) == "" {
		return errors.New("octo: channel id is required")
	}
	return nil
}

func (s *octoSender) postBotJSON(ctx context.Context, path string, payload any) ([]byte, error) {
	if s.creds.APIURL == "" {
		return nil, errors.New("octo: api url not configured")
	}
	if s.creds.IMToken == "" {
		return nil, errors.New("octo: im token not configured")
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("octo: encode %s request: %w", path, err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, octoURL(s.creds.APIURL, path), bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("octo: build %s request: %w", path, err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+s.creds.IMToken)

	resp, err := s.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("octo: post %s: %w", path, err)
	}
	defer func() { _ = resp.Body.Close() }()
	respBody, err := io.ReadAll(io.LimitReader(resp.Body, octoAPITimeoutBodyLimit))
	if err != nil {
		return nil, fmt.Errorf("octo: read %s response: %w", path, err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("octo: %s failed (%d): %s", path, resp.StatusCode, strings.TrimSpace(string(respBody)))
	}
	return respBody, nil
}

func octoURL(base, path string) string {
	return strings.TrimRight(base, "/") + path
}

func octoMessageIDFromResponse(body []byte) (string, error) {
	if len(bytes.TrimSpace(body)) == 0 {
		return "", nil
	}
	var raw map[string]any
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.UseNumber()
	if err := dec.Decode(&raw); err != nil {
		return "", fmt.Errorf("octo: decode send message response: %w", err)
	}
	switch v := raw["message_id"].(type) {
	case string:
		return v, nil
	case json.Number:
		return v.String(), nil
	case float64:
		return fmt.Sprintf("%.0f", v), nil
	default:
		return "", nil
	}
}
