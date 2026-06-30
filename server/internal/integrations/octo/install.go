package octo

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/multica-ai/multica/server/internal/integrations/channel/engine"
	"github.com/multica-ai/multica/server/internal/util/secretbox"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

var (
	ErrInstallationNotFound       = errors.New("octo installation not found")
	ErrBotOwnedByAnotherWorkspace = errors.New("octo: this bot is already connected to another agent or Multica workspace")
	ErrInvalidBotToken            = errors.New("octo: bot token must start with bf_ or app_")
)

type installQueries interface {
	WithTx(tx pgx.Tx) installQueries
	UpsertChannelInstallation(ctx context.Context, arg db.UpsertChannelInstallationParams) (db.ChannelInstallation, error)
	ListChannelInstallationsByWorkspace(ctx context.Context, arg db.ListChannelInstallationsByWorkspaceParams) ([]db.ChannelInstallation, error)
	GetChannelInstallationInWorkspace(ctx context.Context, arg db.GetChannelInstallationInWorkspaceParams) (db.ChannelInstallation, error)
	SetChannelInstallationStatus(ctx context.Context, arg db.SetChannelInstallationStatusParams) error
}

type dbInstallQueries struct{ *db.Queries }

func (q dbInstallQueries) WithTx(tx pgx.Tx) installQueries {
	return dbInstallQueries{q.Queries.WithTx(tx)}
}

type InstallService struct {
	box        *secretbox.Box
	q          installQueries
	tx         engine.TxStarter
	httpClient *http.Client
	logger     *slog.Logger
}

func NewInstallService(q *db.Queries, tx engine.TxStarter, box *secretbox.Box, logger *slog.Logger) (*InstallService, error) {
	if q == nil {
		return nil, errors.New("octo: InstallService requires queries")
	}
	return newInstallService(dbInstallQueries{q}, tx, box, logger)
}

func newInstallService(q installQueries, tx engine.TxStarter, box *secretbox.Box, logger *slog.Logger) (*InstallService, error) {
	if box == nil {
		return nil, errors.New("octo: InstallService requires a non-nil secretbox.Box")
	}
	if q == nil {
		return nil, errors.New("octo: InstallService requires queries")
	}
	if tx == nil {
		return nil, errors.New("octo: InstallService requires a tx starter")
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &InstallService{
		box:        box,
		q:          q,
		tx:         tx,
		httpClient: http.DefaultClient,
		logger:     logger,
	}, nil
}

type RegisterBYOParams struct {
	WorkspaceID pgtype.UUID
	AgentID     pgtype.UUID
	InitiatorID pgtype.UUID
	BotToken    string
	APIURL      string
}

func (s *InstallService) RegisterBYO(ctx context.Context, p RegisterBYOParams) (db.ChannelInstallation, error) {
	botToken := strings.TrimSpace(p.BotToken)
	if !isValidOctoBotToken(botToken) {
		return db.ChannelInstallation{}, ErrInvalidBotToken
	}
	apiURL := strings.TrimRight(strings.TrimSpace(p.APIURL), "/")
	if apiURL == "" {
		return db.ChannelInstallation{}, errors.New("octo: api url is required")
	}
	reg, err := registerBot(ctx, s.httpClient, apiURL, botToken)
	if err != nil {
		return db.ChannelInstallation{}, err
	}
	sealed, err := s.box.Seal([]byte(reg.IMToken))
	if err != nil {
		return db.ChannelInstallation{}, fmt.Errorf("encrypt octo im token: %w", err)
	}
	cfgJSON, err := json.Marshal(installConfig{
		AppID:            reg.RobotID,
		RobotID:          reg.RobotID,
		OwnerUID:         reg.OwnerUID,
		OwnerChannelID:   reg.OwnerChannelID,
		APIURL:           reg.APIURL,
		WSURL:            reg.WSURL,
		IMTokenEncrypted: base64.StdEncoding.EncodeToString(sealed),
	})
	if err != nil {
		return db.ChannelInstallation{}, fmt.Errorf("encode octo installation config: %w", err)
	}
	return s.persistInstall(ctx, installPersist{
		wsID:        p.WorkspaceID,
		agentID:     p.AgentID,
		installerID: p.InitiatorID,
		configJSON:  cfgJSON,
	})
}

func isValidOctoBotToken(token string) bool {
	return strings.HasPrefix(token, "bf_") || strings.HasPrefix(token, "app_")
}

type installPersist struct {
	wsID        pgtype.UUID
	agentID     pgtype.UUID
	installerID pgtype.UUID
	configJSON  []byte
}

const pgUniqueViolation = "23505"

func (s *InstallService) persistInstall(ctx context.Context, p installPersist) (db.ChannelInstallation, error) {
	tx, err := s.tx.Begin(ctx)
	if err != nil {
		return db.ChannelInstallation{}, fmt.Errorf("begin octo install tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	qtx := s.q.WithTx(tx)
	inst, err := qtx.UpsertChannelInstallation(ctx, db.UpsertChannelInstallationParams{
		WorkspaceID:     p.wsID,
		AgentID:         p.agentID,
		ChannelType:     string(TypeOcto),
		Config:          p.configJSON,
		InstallerUserID: p.installerID,
	})
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == pgUniqueViolation {
			return db.ChannelInstallation{}, ErrBotOwnedByAnotherWorkspace
		}
		return db.ChannelInstallation{}, fmt.Errorf("upsert octo installation: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return db.ChannelInstallation{}, fmt.Errorf("commit octo install: %w", err)
	}
	return inst, nil
}

func (s *InstallService) ListByWorkspace(ctx context.Context, wsID pgtype.UUID) ([]db.ChannelInstallation, error) {
	return s.q.ListChannelInstallationsByWorkspace(ctx, db.ListChannelInstallationsByWorkspaceParams{
		WorkspaceID: wsID,
		ChannelType: string(TypeOcto),
	})
}

func (s *InstallService) GetInWorkspace(ctx context.Context, id, wsID pgtype.UUID) (db.ChannelInstallation, error) {
	inst, err := s.q.GetChannelInstallationInWorkspace(ctx, db.GetChannelInstallationInWorkspaceParams{
		ID:          id,
		WorkspaceID: wsID,
		ChannelType: string(TypeOcto),
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return db.ChannelInstallation{}, ErrInstallationNotFound
		}
		return db.ChannelInstallation{}, err
	}
	return inst, nil
}

func (s *InstallService) Revoke(ctx context.Context, id pgtype.UUID) error {
	return s.q.SetChannelInstallationStatus(ctx, db.SetChannelInstallationStatusParams{
		ID:     id,
		Status: "revoked",
	})
}

type registerBotResponse struct {
	RobotID        string `json:"robot_id"`
	Name           string `json:"name"`
	IMToken        string `json:"im_token"`
	WSURL          string `json:"ws_url"`
	APIURL         string `json:"api_url"`
	OwnerUID       string `json:"owner_uid"`
	OwnerChannelID string `json:"owner_channel_id"`
}

func registerBot(ctx context.Context, httpClient *http.Client, apiURL, botToken string) (registerBotResponse, error) {
	if httpClient == nil {
		httpClient = http.DefaultClient
	}
	body, err := json.Marshal(map[string]string{"agent_platform": "multica"})
	if err != nil {
		return registerBotResponse{}, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, octoURL(apiURL, "/v1/bot/register"), bytes.NewReader(body))
	if err != nil {
		return registerBotResponse{}, fmt.Errorf("octo: build register request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+botToken)
	resp, err := httpClient.Do(req)
	if err != nil {
		return registerBotResponse{}, fmt.Errorf("octo: register bot: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	respBody, err := io.ReadAll(io.LimitReader(resp.Body, octoAPITimeoutBodyLimit))
	if err != nil {
		return registerBotResponse{}, fmt.Errorf("octo: read register response: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return registerBotResponse{}, fmt.Errorf("octo: register bot failed (%d): %s", resp.StatusCode, strings.TrimSpace(string(respBody)))
	}
	var out registerBotResponse
	if err := json.Unmarshal(respBody, &out); err != nil {
		return registerBotResponse{}, fmt.Errorf("octo: decode register response: %w", err)
	}
	if out.RobotID == "" || out.IMToken == "" || out.WSURL == "" || out.APIURL == "" {
		return registerBotResponse{}, errors.New("octo: register response missing robot_id / im_token / ws_url / api_url")
	}
	return out, nil
}
