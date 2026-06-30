package octo

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/ecdh"
	"crypto/md5"
	"encoding/base64"
	"encoding/hex"
	"testing"
)

func TestEncodeRemainingLengthMatchesWuKongIMVariableLength(t *testing.T) {
	tests := []struct {
		name string
		n    int
		want []byte
	}{
		{name: "single byte max", n: 127, want: []byte{0x7f}},
		{name: "two bytes min", n: 128, want: []byte{0x80, 0x01}},
		{name: "two bytes arbitrary", n: 321, want: []byte{0xc1, 0x02}},
		{name: "three bytes min", n: 16384, want: []byte{0x80, 0x80, 0x01}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := encodeRemainingLength(tt.n)
			if !bytes.Equal(got, tt.want) {
				t.Fatalf("encodeRemainingLength(%d) = %x, want %x", tt.n, got, tt.want)
			}
		})
	}
}

func TestEncodeConnectPacketMatchesSocketTSLayout(t *testing.T) {
	got := encodeConnectPacket(connectPacket{
		Version:         4,
		DeviceFlag:      0,
		DeviceID:        "devW",
		UID:             "bot",
		Token:           "tok",
		ClientTimestamp: 1,
		ClientKey:       "key",
	})

	want := []byte{
		0x10, 0x1f,
		0x04,
		0x00,
		0x00, 0x04, 'd', 'e', 'v', 'W',
		0x00, 0x03, 'b', 'o', 't',
		0x00, 0x03, 't', 'o', 'k',
		0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x01,
		0x00, 0x03, 'k', 'e', 'y',
	}

	if !bytes.Equal(got, want) {
		t.Fatalf("encodeConnectPacket() = %x, want %x", got, want)
	}
}

func TestEncodeRecvackPacketMatchesSocketTSLayout(t *testing.T) {
	got, err := encodeRecvackPacket("258", 9)
	if err != nil {
		t.Fatalf("encodeRecvackPacket returned error: %v", err)
	}

	want := []byte{
		0x60, 0x0c,
		0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x01, 0x02,
		0x00, 0x00, 0x00, 0x09,
	}

	if !bytes.Equal(got, want) {
		t.Fatalf("encodeRecvackPacket() = %x, want %x", got, want)
	}
}

func TestDecodeWKFrameRecvDecryptsPayloadAndBuildsAck(t *testing.T) {
	const (
		aesKey = "1234567890abcdef"
		aesIV  = "abcdef1234567890"
	)
	payload := `{"type":1,"content":"hello","mention":{"uids":["r-bot"]}}`
	encryptedPayload := encryptPayloadForTest(t, aesKey, aesIV, payload)

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
	frame := encodePacket(packetTypeRecv, body)

	decoded, err := decodeWKFrame(frame, wkSession{
		ServerVersion: 4,
		AESKey:        aesKey,
		AESIV:         aesIV,
	})
	if err != nil {
		t.Fatalf("decodeWKFrame: %v", err)
	}
	if decoded.Type != packetTypeRecv {
		t.Fatalf("decoded type = %d, want RECV", decoded.Type)
	}
	if decoded.Message.MessageID != "258" || decoded.Message.MessageSeq != 9 {
		t.Errorf("message id/seq = %q/%d", decoded.Message.MessageID, decoded.Message.MessageSeq)
	}
	if decoded.Message.FromUID != "u-alice" || decoded.Message.ChannelID != "group-1" || decoded.Message.ChannelType != octoChannelTypeGroup {
		t.Errorf("message source = %+v", decoded.Message)
	}
	if decoded.Message.Payload.Type != octoMessageTypeText || decoded.Message.Payload.Content != "hello" {
		t.Errorf("payload = %+v", decoded.Message.Payload)
	}
	wantAck, err := encodeRecvackPacket("258", 9)
	if err != nil {
		t.Fatalf("encode ack: %v", err)
	}
	if !bytes.Equal(decoded.Ack, wantAck) {
		t.Errorf("ack = %x, want %x", decoded.Ack, wantAck)
	}
}

func TestDecodeWKFrameConnackReadsServerVersionAndNode(t *testing.T) {
	var body []byte
	body = append(body, 0x04)
	body = appendInt64(body, 0)
	body = append(body, 0x01)
	body = appendString(body, "server-key")
	body = appendString(body, "0123456789abcdef-salt")
	body = appendInt64(body, 101)
	frame := append([]byte{byte(packetTypeConnack)<<4 | 0x01}, encodeRemainingLength(len(body))...)
	frame = append(frame, body...)

	decoded, err := decodeWKFrame(frame, wkSession{})
	if err != nil {
		t.Fatalf("decodeWKFrame: %v", err)
	}
	if decoded.Type != packetTypeConnack {
		t.Fatalf("decoded type = %d, want CONNACK", decoded.Type)
	}
	if decoded.Connack.ServerVersion != 4 || decoded.Connack.ReasonCode != 1 || decoded.Connack.NodeID != 101 {
		t.Errorf("connack = %+v", decoded.Connack)
	}
	if decoded.Connack.ServerKey != "server-key" || decoded.Connack.Salt != "0123456789abcdef-salt" {
		t.Errorf("connack key/salt = %+v", decoded.Connack)
	}
}

func TestDeriveWKSessionMatchesSocketTSKeyDerivation(t *testing.T) {
	curve := ecdh.X25519()
	clientPrivate, err := curve.NewPrivateKey(bytes.Repeat([]byte{1}, 32))
	if err != nil {
		t.Fatalf("client private key: %v", err)
	}
	serverPrivate, err := curve.NewPrivateKey(bytes.Repeat([]byte{2}, 32))
	if err != nil {
		t.Fatalf("server private key: %v", err)
	}
	connack := connackPacket{
		ServerVersion: 4,
		ReasonCode:    1,
		ServerKey:     base64.StdEncoding.EncodeToString(serverPrivate.PublicKey().Bytes()),
		Salt:          "abcdefghijklmnop-extra",
	}

	got, err := deriveWKSession(clientPrivate, connack)
	if err != nil {
		t.Fatalf("deriveWKSession: %v", err)
	}
	secret, err := clientPrivate.ECDH(serverPrivate.PublicKey())
	if err != nil {
		t.Fatalf("test ECDH: %v", err)
	}
	sum := md5.Sum([]byte(base64.StdEncoding.EncodeToString(secret)))
	wantKey := hex.EncodeToString(sum[:])[:16]
	if got.AESKey != wantKey {
		t.Errorf("AESKey = %q, want %q", got.AESKey, wantKey)
	}
	if got.AESIV != "abcdefghijklmnop" {
		t.Errorf("AESIV = %q", got.AESIV)
	}
	if got.ServerVersion != 4 {
		t.Errorf("ServerVersion = %d", got.ServerVersion)
	}
}

func encryptPayloadForTest(t *testing.T, key, iv, plaintext string) []byte {
	t.Helper()
	block, err := aes.NewCipher([]byte(key))
	if err != nil {
		t.Fatalf("aes.NewCipher: %v", err)
	}
	padded := pkcs7PadForTest([]byte(plaintext), block.BlockSize())
	ciphertext := make([]byte, len(padded))
	cipher.NewCBCEncrypter(block, []byte(iv)).CryptBlocks(ciphertext, padded)
	return []byte(base64.StdEncoding.EncodeToString(ciphertext))
}

func pkcs7PadForTest(data []byte, blockSize int) []byte {
	pad := blockSize - len(data)%blockSize
	out := append([]byte(nil), data...)
	for i := 0; i < pad; i++ {
		out = append(out, byte(pad))
	}
	return out
}
