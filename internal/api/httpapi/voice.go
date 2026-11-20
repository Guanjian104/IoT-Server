package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"time"

	"iot-server/internal/media"
	"iot-server/internal/mqttmsg"
)

var errVoiceUnavailable = errors.New("voice unavailable")

type voicePublisher interface {
	PublishURL(resourceID string) string
}

// commandPayload 生成下发 JSON。开启 voice_call 时由本机 SFU 签发设备凭证，并把对端凭证返回给调用方。
func (s *Server) commandPayload(ctx context.Context, deviceID string, userID int64, cmd string, on *bool, raw json.RawMessage) ([]byte, map[string]any, error) {
	if cmd != "voice_call" || on == nil || !*on {
		payload, err := mqttmsg.CanonicalCommand(cmd, on, raw)
		return payload, nil, err
	}
	duration, err := voiceDuration(raw)
	if err != nil {
		return nil, nil, err
	}
	publisher, ok := s.media.(voicePublisher)
	if !ok || s.media == nil {
		return nil, nil, errVoiceUnavailable
	}
	room, err := s.media.CreateRoom(ctx, media.RoomRequest{SessionID: deviceID})
	if err != nil {
		return nil, nil, errVoiceUnavailable
	}
	expires := time.Now().Add(10 * time.Minute).UTC().Truncate(time.Second)
	deviceCred, err := s.media.CreateJoinCredential(ctx, media.JoinRequest{
		ResourceID: room.ID,
		Identity:   "call:" + deviceID,
		ExpiresAt:  expires,
	})
	if err != nil || !deviceCred.Connectable {
		return nil, nil, errVoiceUnavailable
	}
	peerCred, err := s.media.CreateJoinCredential(ctx, media.JoinRequest{
		ResourceID: room.ID,
		Identity:   "call:u" + strconv.FormatInt(userID, 10),
		ExpiresAt:  expires,
	})
	if err != nil || !peerCred.Connectable {
		return nil, nil, errVoiceUnavailable
	}
	whip := publisher.PublishURL(room.ID)
	if whip == "" || strings.Contains(whip, "127.0.0.1") || strings.Contains(whip, "localhost") {
		return nil, nil, errVoiceUnavailable
	}
	params := map[string]any{"whip_url": whip, "token": deviceCred.Token}
	if duration > 0 {
		params["duration_min"] = duration
	}
	encoded, err := json.Marshal(params)
	if err != nil {
		return nil, nil, err
	}
	payload, err := mqttmsg.CanonicalCommand(cmd, on, encoded)
	if err != nil {
		return nil, nil, err
	}
	return payload, map[string]any{
		"whip_url":   whip,
		"token":      peerCred.Token,
		"expires_at": expires.Format(time.RFC3339),
	}, nil
}

func voiceDuration(raw json.RawMessage) (int, error) {
	if len(bytes.TrimSpace(raw)) == 0 || string(bytes.TrimSpace(raw)) == "null" {
		return 0, nil
	}
	var doc map[string]json.RawMessage
	dec := json.NewDecoder(bytes.NewReader(raw))
	if err := dec.Decode(&doc); err != nil || dec.More() || doc == nil {
		return 0, errors.New("invalid voice params")
	}
	if len(doc) == 0 {
		return 0, nil
	}
	value, ok := doc["duration_min"]
	if !ok || len(doc) != 1 {
		return 0, errors.New("invalid voice params")
	}
	text := strings.TrimSpace(string(value))
	parsed, err := strconv.Atoi(text)
	if err != nil || parsed < 1 || parsed > 60 {
		return 0, errors.New("invalid voice params")
	}
	return parsed, nil
}
