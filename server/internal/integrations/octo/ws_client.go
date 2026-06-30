package octo

import (
	"context"
	"crypto/ecdh"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/gorilla/websocket"
)

const wkHeartbeatInterval = 60 * time.Second

type wkSocketConfig struct {
	WSURL   string
	UID     string
	Token   string
	Handler func(context.Context, botMessage) error
	Logger  *slog.Logger
	Dialer  *websocket.Dialer
}

type wkSocketClient struct {
	cfg    wkSocketConfig
	logger *slog.Logger
}

func newWKSocketClient(cfg wkSocketConfig) *wkSocketClient {
	logger := cfg.Logger
	if logger == nil {
		logger = slog.Default()
	}
	return &wkSocketClient{cfg: cfg, logger: logger}
}

func (c *wkSocketClient) Run(ctx context.Context) error {
	if c.cfg.WSURL == "" {
		return errors.New("octo: ws url not configured")
	}
	if c.cfg.UID == "" || c.cfg.Token == "" {
		return errors.New("octo: ws uid/token not configured")
	}
	if c.cfg.Handler == nil {
		return errors.New("octo: ws handler not configured")
	}
	private, err := ecdh.X25519().GenerateKey(rand.Reader)
	if err != nil {
		return fmt.Errorf("octo: generate x25519 key: %w", err)
	}
	dialer := c.cfg.Dialer
	if dialer == nil {
		dialer = websocket.DefaultDialer
	}
	conn, _, err := dialer.DialContext(ctx, c.cfg.WSURL, nil)
	if err != nil {
		return fmt.Errorf("octo: dial ws: %w", err)
	}
	defer func() { _ = conn.Close() }()

	var writeMu sync.Mutex
	writeRaw := func(data []byte) error {
		writeMu.Lock()
		defer writeMu.Unlock()
		return conn.WriteMessage(websocket.BinaryMessage, data)
	}
	done := make(chan struct{})
	defer close(done)
	go func() {
		select {
		case <-ctx.Done():
			_ = conn.Close()
		case <-done:
		}
	}()
	go c.heartbeat(ctx, done, writeRaw)

	if err := writeRaw(encodeConnectPacket(connectPacket{
		Version:         protoVersion,
		DeviceFlag:      0,
		DeviceID:        uuid.NewString() + "W",
		UID:             c.cfg.UID,
		Token:           c.cfg.Token,
		ClientTimestamp: time.Now().UnixMilli(),
		ClientKey:       base64.StdEncoding.EncodeToString(private.PublicKey().Bytes()),
	})); err != nil {
		return fmt.Errorf("octo: send connect: %w", err)
	}

	session := wkSession{ServerVersion: protoVersion}
	for {
		_, data, err := conn.ReadMessage()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return fmt.Errorf("octo: read ws frame: %w", err)
		}
		decoded, err := decodeWKFrame(data, session)
		if err != nil {
			return err
		}
		switch decoded.Type {
		case packetTypeConnack:
			session, err = deriveWKSession(private, decoded.Connack)
			if err != nil {
				return err
			}
		case packetTypeRecv:
			if len(decoded.Ack) > 0 {
				if err := writeRaw(decoded.Ack); err != nil {
					return fmt.Errorf("octo: send recvack: %w", err)
				}
			}
			if err := c.cfg.Handler(ctx, decoded.Message); err != nil {
				return err
			}
		case packetTypePing:
			if err := writeRaw(encodePongPacket()); err != nil {
				return fmt.Errorf("octo: send pong: %w", err)
			}
		}
	}
}

func (c *wkSocketClient) heartbeat(ctx context.Context, done <-chan struct{}, writeRaw func([]byte) error) {
	ticker := time.NewTicker(wkHeartbeatInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-done:
			return
		case <-ticker.C:
			if err := writeRaw(encodePingPacket()); err != nil {
				c.logger.DebugContext(ctx, "octo: ws ping failed", "error", err)
				return
			}
		}
	}
}
