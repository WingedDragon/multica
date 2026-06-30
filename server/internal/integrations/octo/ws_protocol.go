package octo

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/ecdh"
	"crypto/md5"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strconv"
)

type packetType byte

const (
	protoVersion = 4
)

const (
	packetTypeConnect packetType = 1
	packetTypeConnack packetType = 2
	packetTypeRecv    packetType = 5
	packetTypeRecvack packetType = 6
	packetTypePing    packetType = 7
	packetTypePong    packetType = 8
)

type connectPacket struct {
	Version         byte
	DeviceFlag      byte
	DeviceID        string
	UID             string
	Token           string
	ClientTimestamp int64
	ClientKey       string
}

func encodeConnectPacket(p connectPacket) []byte {
	var body []byte
	body = append(body, p.Version)
	body = append(body, p.DeviceFlag)
	body = appendString(body, p.DeviceID)
	body = appendString(body, p.UID)
	body = appendString(body, p.Token)
	body = appendInt64(body, uint64(p.ClientTimestamp))
	body = appendString(body, p.ClientKey)
	return encodePacket(packetTypeConnect, body)
}

func encodePingPacket() []byte {
	return []byte{byte(packetTypePing) << 4}
}

func encodePongPacket() []byte {
	return []byte{byte(packetTypePong) << 4}
}

func encodeRecvackPacket(messageID string, messageSeq uint32) ([]byte, error) {
	id, err := strconv.ParseUint(messageID, 10, 64)
	if err != nil {
		return nil, fmt.Errorf("octo: parse recvack message id: %w", err)
	}
	var body []byte
	body = appendInt64(body, id)
	body = appendInt32(body, messageSeq)
	return encodePacket(packetTypeRecvack, body), nil
}

type wkSession struct {
	ServerVersion int
	AESKey        string
	AESIV         string
}

type connackPacket struct {
	ServerVersion int
	ReasonCode    byte
	ServerKey     string
	Salt          string
	NodeID        uint64
}

type decodedWKFrame struct {
	Type    packetType
	Message botMessage
	Ack     []byte
	Connack connackPacket
}

func decodeWKFrame(frame []byte, session wkSession) (decodedWKFrame, error) {
	if len(frame) == 0 {
		return decodedWKFrame{}, fmt.Errorf("octo: empty wk frame")
	}
	first := frame[0]
	t := packetType(first >> 4)
	flags := first & 0x0f
	if t == packetTypePing || t == packetTypePong {
		return decodedWKFrame{Type: t}, nil
	}
	remaining, n, err := decodeRemainingLength(frame[1:])
	if err != nil {
		return decodedWKFrame{}, err
	}
	bodyStart := 1 + n
	bodyEnd := bodyStart + remaining
	if bodyEnd > len(frame) {
		return decodedWKFrame{}, fmt.Errorf("octo: incomplete wk frame body")
	}
	body := frame[bodyStart:bodyEnd]
	switch t {
	case packetTypeConnack:
		ack, err := decodeConnackPacket(body, flags&0x01 > 0)
		if err != nil {
			return decodedWKFrame{}, err
		}
		return decodedWKFrame{Type: t, Connack: ack}, nil
	case packetTypeRecv:
		msg, err := decodeRecvPacket(body, session)
		if err != nil {
			return decodedWKFrame{}, err
		}
		ack, err := encodeRecvackPacket(msg.MessageID, uint32(msg.MessageSeq))
		if err != nil {
			return decodedWKFrame{}, err
		}
		return decodedWKFrame{Type: t, Message: msg, Ack: ack}, nil
	default:
		return decodedWKFrame{Type: t}, nil
	}
}

func decodeConnackPacket(body []byte, hasServerVersion bool) (connackPacket, error) {
	dec := wkDecoder{data: body}
	version := protoVersion
	if hasServerVersion {
		b, err := dec.readByte()
		if err != nil {
			return connackPacket{}, err
		}
		version = int(b)
	}
	if _, err := dec.readInt64(); err != nil {
		return connackPacket{}, err
	}
	reason, err := dec.readByte()
	if err != nil {
		return connackPacket{}, err
	}
	serverKey, err := dec.readString()
	if err != nil {
		return connackPacket{}, err
	}
	salt, err := dec.readString()
	if err != nil {
		return connackPacket{}, err
	}
	var nodeID uint64
	if version >= 4 {
		nodeID, err = dec.readInt64()
		if err != nil {
			return connackPacket{}, err
		}
	}
	return connackPacket{
		ServerVersion: version,
		ReasonCode:    reason,
		ServerKey:     serverKey,
		Salt:          salt,
		NodeID:        nodeID,
	}, nil
}

func deriveWKSession(private *ecdh.PrivateKey, ack connackPacket) (wkSession, error) {
	if ack.ReasonCode != 1 {
		return wkSession{}, fmt.Errorf("octo: connack failed: reasonCode=%d", ack.ReasonCode)
	}
	serverKeyBytes, err := base64.StdEncoding.DecodeString(ack.ServerKey)
	if err != nil {
		return wkSession{}, fmt.Errorf("octo: decode server key: %w", err)
	}
	serverKey, err := ecdh.X25519().NewPublicKey(serverKeyBytes)
	if err != nil {
		return wkSession{}, fmt.Errorf("octo: parse server key: %w", err)
	}
	secret, err := private.ECDH(serverKey)
	if err != nil {
		return wkSession{}, fmt.Errorf("octo: derive shared secret: %w", err)
	}
	secretBase64 := base64.StdEncoding.EncodeToString(secret)
	sum := md5.Sum([]byte(secretBase64))
	aesKey := hex.EncodeToString(sum[:])[:16]
	aesIV := ack.Salt
	if len(aesIV) > 16 {
		aesIV = aesIV[:16]
	}
	return wkSession{
		ServerVersion: ack.ServerVersion,
		AESKey:        aesKey,
		AESIV:         aesIV,
	}, nil
}

func decodeRemainingLength(data []byte) (length int, bytesRead int, err error) {
	multiplier := 1
	for i, b := range data {
		length += int(b&0x7f) * multiplier
		bytesRead = i + 1
		if b&0x80 == 0 {
			return length, bytesRead, nil
		}
		multiplier *= 128
		if multiplier > 128*128*128 {
			return 0, 0, fmt.Errorf("octo: malformed remaining length")
		}
	}
	return 0, 0, fmt.Errorf("octo: incomplete remaining length")
}

func decodeRecvPacket(body []byte, session wkSession) (botMessage, error) {
	dec := wkDecoder{data: body}
	settingByte, err := dec.readByte()
	if err != nil {
		return botMessage{}, err
	}
	setting := parseSettingByte(settingByte)
	if _, err := dec.readString(); err != nil {
		return botMessage{}, err
	}
	fromUID, err := dec.readString()
	if err != nil {
		return botMessage{}, err
	}
	channelID, err := dec.readString()
	if err != nil {
		return botMessage{}, err
	}
	channelTypeByte, err := dec.readByte()
	if err != nil {
		return botMessage{}, err
	}
	if session.ServerVersion >= 3 {
		if _, err := dec.readInt32(); err != nil {
			return botMessage{}, err
		}
	}
	if _, err := dec.readString(); err != nil {
		return botMessage{}, err
	}
	messageID, err := dec.readInt64()
	if err != nil {
		return botMessage{}, err
	}
	messageSeq, err := dec.readInt32()
	if err != nil {
		return botMessage{}, err
	}
	timestamp, err := dec.readInt32()
	if err != nil {
		return botMessage{}, err
	}
	if setting.topic {
		if _, err := dec.readString(); err != nil {
			return botMessage{}, err
		}
	}
	payloadBytes, err := decryptWKPayload(dec.remaining(), session.AESKey, session.AESIV)
	if err != nil {
		return botMessage{}, err
	}
	var payload messagePayload
	payloadDec := json.NewDecoder(bytes.NewReader(payloadBytes))
	payloadDec.UseNumber()
	if err := payloadDec.Decode(&payload); err != nil {
		return botMessage{}, fmt.Errorf("octo: decode recv payload: %w", err)
	}
	return botMessage{
		MessageID:   strconv.FormatUint(messageID, 10),
		MessageSeq:  int32(messageSeq),
		FromUID:     fromUID,
		ChannelID:   channelID,
		ChannelType: octoChannelType(channelTypeByte),
		Timestamp:   int64(timestamp),
		Payload:     payload,
	}, nil
}

type settingFlags struct {
	topic bool
}

func parseSettingByte(v byte) settingFlags {
	return settingFlags{topic: ((v >> 3) & 0x01) > 0}
}

func decryptWKPayload(data []byte, aesKey, aesIV string) ([]byte, error) {
	if aesKey == "" || aesIV == "" {
		return nil, fmt.Errorf("octo: recv payload crypto not initialized")
	}
	ciphertext, err := base64.StdEncoding.DecodeString(string(data))
	if err != nil {
		return nil, fmt.Errorf("octo: base64 decode recv payload: %w", err)
	}
	block, err := aes.NewCipher([]byte(aesKey))
	if err != nil {
		return nil, fmt.Errorf("octo: create aes cipher: %w", err)
	}
	if len(aesIV) != block.BlockSize() {
		return nil, fmt.Errorf("octo: invalid aes iv length")
	}
	if len(ciphertext) == 0 || len(ciphertext)%block.BlockSize() != 0 {
		return nil, fmt.Errorf("octo: invalid encrypted payload length")
	}
	plaintext := make([]byte, len(ciphertext))
	cipher.NewCBCDecrypter(block, []byte(aesIV)).CryptBlocks(plaintext, ciphertext)
	return pkcs7Unpad(plaintext, block.BlockSize())
}

func pkcs7Unpad(data []byte, blockSize int) ([]byte, error) {
	if len(data) == 0 || len(data)%blockSize != 0 {
		return nil, fmt.Errorf("octo: invalid pkcs7 payload length")
	}
	pad := int(data[len(data)-1])
	if pad == 0 || pad > blockSize || pad > len(data) {
		return nil, fmt.Errorf("octo: invalid pkcs7 padding")
	}
	for _, b := range data[len(data)-pad:] {
		if int(b) != pad {
			return nil, fmt.Errorf("octo: invalid pkcs7 padding")
		}
	}
	return data[:len(data)-pad], nil
}

type wkDecoder struct {
	data []byte
	pos  int
}

func (d *wkDecoder) readByte() (byte, error) {
	if d.pos >= len(d.data) {
		return 0, fmt.Errorf("octo: wk frame underflow")
	}
	b := d.data[d.pos]
	d.pos++
	return b, nil
}

func (d *wkDecoder) readString() (string, error) {
	n, err := d.readInt16()
	if err != nil {
		return "", err
	}
	if d.pos+int(n) > len(d.data) {
		return "", fmt.Errorf("octo: wk string underflow")
	}
	s := string(d.data[d.pos : d.pos+int(n)])
	d.pos += int(n)
	return s, nil
}

func (d *wkDecoder) readInt16() (uint16, error) {
	if d.pos+2 > len(d.data) {
		return 0, fmt.Errorf("octo: wk int16 underflow")
	}
	v := uint16(d.data[d.pos])<<8 | uint16(d.data[d.pos+1])
	d.pos += 2
	return v, nil
}

func (d *wkDecoder) readInt32() (uint32, error) {
	if d.pos+4 > len(d.data) {
		return 0, fmt.Errorf("octo: wk int32 underflow")
	}
	v := uint32(d.data[d.pos])<<24 |
		uint32(d.data[d.pos+1])<<16 |
		uint32(d.data[d.pos+2])<<8 |
		uint32(d.data[d.pos+3])
	d.pos += 4
	return v, nil
}

func (d *wkDecoder) readInt64() (uint64, error) {
	if d.pos+8 > len(d.data) {
		return 0, fmt.Errorf("octo: wk int64 underflow")
	}
	v := uint64(d.data[d.pos])<<56 |
		uint64(d.data[d.pos+1])<<48 |
		uint64(d.data[d.pos+2])<<40 |
		uint64(d.data[d.pos+3])<<32 |
		uint64(d.data[d.pos+4])<<24 |
		uint64(d.data[d.pos+5])<<16 |
		uint64(d.data[d.pos+6])<<8 |
		uint64(d.data[d.pos+7])
	d.pos += 8
	return v, nil
}

func (d *wkDecoder) remaining() []byte {
	if d.pos >= len(d.data) {
		return nil
	}
	return d.data[d.pos:]
}

func encodePacket(t packetType, body []byte) []byte {
	out := []byte{byte(t) << 4}
	out = append(out, encodeRemainingLength(len(body))...)
	out = append(out, body...)
	return out
}

func encodeRemainingLength(length int) []byte {
	if length == 0 {
		return []byte{0}
	}
	var out []byte
	for length > 0 {
		digit := byte(length % 128)
		length /= 128
		if length > 0 {
			digit |= 0x80
		}
		out = append(out, digit)
	}
	return out
}

func appendString(dst []byte, s string) []byte {
	b := []byte(s)
	dst = appendInt16(dst, uint16(len(b)))
	return append(dst, b...)
}

func appendInt16(dst []byte, v uint16) []byte {
	return append(dst, byte(v>>8), byte(v))
}

func appendInt32(dst []byte, v uint32) []byte {
	return append(dst, byte(v>>24), byte(v>>16), byte(v>>8), byte(v))
}

func appendInt64(dst []byte, v uint64) []byte {
	return append(dst,
		byte(v>>56),
		byte(v>>48),
		byte(v>>40),
		byte(v>>32),
		byte(v>>24),
		byte(v>>16),
		byte(v>>8),
		byte(v),
	)
}
