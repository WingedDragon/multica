package octo

import (
	"context"
	"crypto/ecdh"
	"crypto/md5"
	"encoding/base64"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gorilla/websocket"
)

func TestWKSocketClientConnectsReceivesAndAcks(t *testing.T) {
	var upgrader websocket.Upgrader
	gotAck := make(chan []byte, 1)
	serverErr := make(chan error, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			serverErr <- err
			return
		}
		defer func() { _ = conn.Close() }()

		_, connectFrame, err := conn.ReadMessage()
		if err != nil {
			serverErr <- err
			return
		}
		connect, err := readConnectPacketForTest(connectFrame)
		if err != nil {
			serverErr <- err
			return
		}
		if connect.UID != "r-bot" || connect.Token != "im-token" {
			t.Errorf("connect uid/token = %q/%q", connect.UID, connect.Token)
		}

		session, serverKey, err := serverSessionForTest(connect.ClientKey)
		if err != nil {
			serverErr <- err
			return
		}
		if err := conn.WriteMessage(websocket.BinaryMessage, connackFrameForTest(serverKey, "abcdefghijklmnop-extra")); err != nil {
			serverErr <- err
			return
		}
		if err := conn.WriteMessage(websocket.BinaryMessage, recvFrameForTest(t, session)); err != nil {
			serverErr <- err
			return
		}
		_, ack, err := conn.ReadMessage()
		if err != nil {
			serverErr <- err
			return
		}
		gotAck <- ack
		serverErr <- nil
	}))
	defer srv.Close()

	received := make(chan botMessage, 1)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	client := newWKSocketClient(wkSocketConfig{
		WSURL: strings.Replace(srv.URL, "http://", "ws://", 1),
		UID:   "r-bot",
		Token: "im-token",
		Handler: func(_ context.Context, msg botMessage) error {
			received <- msg
			return nil
		},
	})
	runErr := make(chan error, 1)
	go func() { runErr <- client.Run(ctx) }()

	select {
	case msg := <-received:
		if msg.MessageID != "258" || msg.FromUID != "u-alice" || msg.Payload.Content != "hello from ws" {
			t.Fatalf("received msg = %+v", msg)
		}
	case <-ctxWithTestTimeout(t).Done():
		t.Fatal("timed out waiting for inbound message")
	}

	wantAck, err := encodeRecvackPacket("258", 9)
	if err != nil {
		t.Fatalf("encode ack: %v", err)
	}
	select {
	case ack := <-gotAck:
		if string(ack) != string(wantAck) {
			t.Fatalf("ack = %x, want %x", ack, wantAck)
		}
	case <-ctxWithTestTimeout(t).Done():
		t.Fatal("timed out waiting for recvack")
	}
	if err := <-serverErr; err != nil {
		t.Fatalf("server: %v", err)
	}
	cancel()
	if err := <-runErr; err != nil {
		t.Fatalf("client Run returned error after cancel: %v", err)
	}
}

type connectPacketForTest struct {
	UID       string
	Token     string
	ClientKey string
}

func readConnectPacketForTest(frame []byte) (connectPacketForTest, error) {
	remaining, n, err := decodeRemainingLength(frame[1:])
	if err != nil {
		return connectPacketForTest{}, err
	}
	dec := wkDecoder{data: frame[1+n : 1+n+remaining]}
	if _, err := dec.readByte(); err != nil {
		return connectPacketForTest{}, err
	}
	if _, err := dec.readByte(); err != nil {
		return connectPacketForTest{}, err
	}
	if _, err := dec.readString(); err != nil {
		return connectPacketForTest{}, err
	}
	uid, err := dec.readString()
	if err != nil {
		return connectPacketForTest{}, err
	}
	token, err := dec.readString()
	if err != nil {
		return connectPacketForTest{}, err
	}
	if _, err := dec.readInt64(); err != nil {
		return connectPacketForTest{}, err
	}
	clientKey, err := dec.readString()
	if err != nil {
		return connectPacketForTest{}, err
	}
	return connectPacketForTest{UID: uid, Token: token, ClientKey: clientKey}, nil
}

func serverSessionForTest(clientKey string) (wkSession, string, error) {
	curve := ecdh.X25519()
	serverPrivate, err := curve.NewPrivateKey([]byte("12345678901234567890123456789012"))
	if err != nil {
		return wkSession{}, "", err
	}
	clientKeyBytes, err := base64.StdEncoding.DecodeString(clientKey)
	if err != nil {
		return wkSession{}, "", err
	}
	clientPublic, err := curve.NewPublicKey(clientKeyBytes)
	if err != nil {
		return wkSession{}, "", err
	}
	secret, err := serverPrivate.ECDH(clientPublic)
	if err != nil {
		return wkSession{}, "", err
	}
	sum := md5.Sum([]byte(base64.StdEncoding.EncodeToString(secret)))
	return wkSession{
		ServerVersion: 4,
		AESKey:        hex.EncodeToString(sum[:])[:16],
		AESIV:         "abcdefghijklmnop",
	}, base64.StdEncoding.EncodeToString(serverPrivate.PublicKey().Bytes()), nil
}

func connackFrameForTest(serverKey string, salt string) []byte {
	var body []byte
	body = append(body, 0x04)
	body = appendInt64(body, 0)
	body = append(body, 0x01)
	body = appendString(body, serverKey)
	body = appendString(body, salt)
	body = appendInt64(body, 101)
	return append(append([]byte{byte(packetTypeConnack)<<4 | 0x01}, encodeRemainingLength(len(body))...), body...)
}

func recvFrameForTest(t *testing.T, session wkSession) []byte {
	payload := `{"type":1,"content":"hello from ws"}`
	encryptedPayload := encryptPayloadForTest(t, session.AESKey, session.AESIV, payload)
	var body []byte
	body = append(body, 0x00)
	body = appendString(body, "msg-key")
	body = appendString(body, "u-alice")
	body = appendString(body, "group-1")
	body = append(body, byte(octoChannelTypeGroup))
	body = appendInt32(body, 0)
	body = appendString(body, "client-1")
	body = appendInt64(body, 258)
	body = appendInt32(body, 9)
	body = appendInt32(body, 1700000000)
	body = append(body, encryptedPayload...)
	return encodePacket(packetTypeRecv, body)
}
