package octo

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

type installConfig struct {
	AppID            string `json:"app_id"`
	RobotID          string `json:"robot_id"`
	OwnerUID         string `json:"owner_uid,omitempty"`
	OwnerChannelID   string `json:"owner_channel_id,omitempty"`
	APIURL           string `json:"api_url"`
	WSURL            string `json:"ws_url"`
	IMTokenEncrypted string `json:"im_token_encrypted"`
}

// Decrypter turns stored ciphertext into plaintext. Tests pass nil to treat the
// decoded bytes as plaintext.
type Decrypter func(ciphertext []byte) (plaintext []byte, err error)

func decodeCredentials(raw json.RawMessage, decrypt Decrypter) (credentials, error) {
	if len(raw) == 0 {
		return credentials{}, errors.New("octo: empty installation config")
	}
	var cfg installConfig
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return credentials{}, fmt.Errorf("decode octo installation config: %w", err)
	}
	token, err := decryptToken(cfg.IMTokenEncrypted, decrypt)
	if err != nil {
		return credentials{}, fmt.Errorf("decrypt im token: %w", err)
	}
	robotID := cfg.RobotID
	if robotID == "" {
		robotID = cfg.AppID
	}
	return credentials{
		RobotID: robotID,
		IMToken: token,
		APIURL:  cfg.APIURL,
		WSURL:   cfg.WSURL,
	}, nil
}

type PublicConfig struct {
	AppID          string
	RobotID        string
	OwnerUID       string
	OwnerChannelID string
	APIURL         string
	WSURL          string
}

func DecodePublicConfig(raw json.RawMessage) PublicConfig {
	var cfg installConfig
	_ = json.Unmarshal(raw, &cfg)
	robotID := cfg.RobotID
	if robotID == "" {
		robotID = cfg.AppID
	}
	return PublicConfig{
		AppID:          cfg.AppID,
		RobotID:        robotID,
		OwnerUID:       cfg.OwnerUID,
		OwnerChannelID: cfg.OwnerChannelID,
		APIURL:         cfg.APIURL,
		WSURL:          cfg.WSURL,
	}
}

func decryptToken(enc string, decrypt Decrypter) (string, error) {
	if enc == "" {
		return "", nil
	}
	ciphertext, err := base64.StdEncoding.DecodeString(stripWhitespace(enc))
	if err != nil {
		return "", fmt.Errorf("base64 decode: %w", err)
	}
	if decrypt == nil {
		return string(ciphertext), nil
	}
	plaintext, err := decrypt(ciphertext)
	if err != nil {
		return "", err
	}
	return string(plaintext), nil
}

func stripWhitespace(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		switch r {
		case ' ', '\t', '\n', '\r':
			continue
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}
