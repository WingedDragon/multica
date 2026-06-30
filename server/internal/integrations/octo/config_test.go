package octo

import (
	"encoding/base64"
	"encoding/json"
	"testing"
)

func TestDecodeCredentials(t *testing.T) {
	raw, _ := json.Marshal(installConfig{
		AppID:            "robot-1",
		RobotID:          "robot-1",
		APIURL:           "https://octo.example/api",
		WSURL:            "wss://octo.example/ws",
		IMTokenEncrypted: base64.StdEncoding.EncodeToString([]byte("im-token")),
	})

	creds, err := decodeCredentials(raw, nil)
	if err != nil {
		t.Fatalf("decodeCredentials: %v", err)
	}
	if creds.RobotID != "robot-1" || creds.APIURL != "https://octo.example/api" || creds.WSURL != "wss://octo.example/ws" {
		t.Errorf("creds = %+v", creds)
	}
	if creds.IMToken != "im-token" {
		t.Errorf("im token = %q", creds.IMToken)
	}
	if _, err := decodeCredentials(nil, nil); err == nil {
		t.Error("empty config should error")
	}
}

func TestDecodePublicConfig(t *testing.T) {
	raw, _ := json.Marshal(installConfig{
		AppID:            "robot-1",
		RobotID:          "robot-1",
		OwnerUID:         "owner-1",
		OwnerChannelID:   "owner-channel",
		APIURL:           "https://octo.example/api",
		WSURL:            "wss://octo.example/ws",
		IMTokenEncrypted: "secret",
	})

	got := DecodePublicConfig(raw)
	if got.AppID != "robot-1" || got.RobotID != "robot-1" {
		t.Errorf("public identity = %+v", got)
	}
	if got.OwnerUID != "owner-1" || got.OwnerChannelID != "owner-channel" {
		t.Errorf("owner fields = %+v", got)
	}
	if got.APIURL != "https://octo.example/api" || got.WSURL != "wss://octo.example/ws" {
		t.Errorf("urls = %+v", got)
	}
}
