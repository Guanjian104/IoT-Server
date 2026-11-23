package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
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
	opened, err := s.issueCallSession(ctx, deviceID, "call:u"+strconv.FormatInt(userID, 10), duration)
	if err != nil {
		return nil, nil, err
	}
	params := map[string]any{"whip_url": opened.whip, "token": opened.deviceToken}
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
		"whip_url":   opened.whip,
		"token":      opened.appToken,
		"expires_at": opened.expires.Format(time.RFC3339),
	}, nil
}

type callSession struct {
	whip        string
	deviceToken string
	appToken    string
	expires     time.Time
}

// issueCallSession 只建房间并签发双方发布凭证，不发布 MQTT。
func (s *Server) issueCallSession(ctx context.Context, mac, peerIdentity string, duration int) (callSession, error) {
	if !mqttmsg.ValidDeviceID(mac) || !callPeerIdentity(peerIdentity) {
		return callSession{}, errors.New("invalid voice params")
	}
	if duration < 0 || duration > 60 {
		return callSession{}, errors.New("invalid voice params")
	}
	publisher, ok := s.media.(voicePublisher)
	if !ok || s.media == nil {
		return callSession{}, errVoiceUnavailable
	}
	room, err := s.media.CreateRoom(ctx, media.RoomRequest{SessionID: mac})
	if err != nil {
		return callSession{}, errVoiceUnavailable
	}
	expires := time.Now().Add(10 * time.Minute).UTC().Truncate(time.Second)
	deviceCred, err := s.media.CreateJoinCredential(ctx, media.JoinRequest{
		ResourceID: room.ID,
		Identity:   "call:" + mac,
		ExpiresAt:  expires,
	})
	if err != nil || !deviceCred.Connectable {
		return callSession{}, errVoiceUnavailable
	}
	peerCred, err := s.media.CreateJoinCredential(ctx, media.JoinRequest{
		ResourceID: room.ID,
		Identity:   peerIdentity,
		ExpiresAt:  expires,
	})
	if err != nil || !peerCred.Connectable || peerCred.Token == deviceCred.Token {
		return callSession{}, errVoiceUnavailable
	}
	whip := publisher.PublishURL(room.ID)
	if whip == "" || strings.Contains(whip, "127.0.0.1") || strings.Contains(whip, "localhost") || strings.Contains(whip, "/whep/") {
		return callSession{}, errVoiceUnavailable
	}
	return callSession{whip: whip, deviceToken: deviceCred.Token, appToken: peerCred.Token, expires: expires}, nil
}

func (s *Server) issueCall(w http.ResponseWriter, r *http.Request) {
	if !s.hookOK(r) {
		writeError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	var body struct {
		MAC         string `json:"mac"`
		UserRef     string `json:"user_ref"`
		DurationMin int    `json:"duration_min"`
	}
	if !decode(w, r, &body) {
		return
	}
	opened, err := s.issueCallSession(r.Context(), body.MAC, "call:u"+body.UserRef, body.DurationMin)
	if errors.Is(err, errVoiceUnavailable) {
		writeError(w, http.StatusServiceUnavailable, "unavailable")
		return
	}
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"whip_url":     opened.whip,
		"device_token": opened.deviceToken,
		"app_token":    opened.appToken,
		"expires_at":   opened.expires.Format(time.RFC3339),
	})
}

func callPeerIdentity(identity string) bool {
	const prefix = "call:u"
	if !strings.HasPrefix(identity, prefix) {
		return false
	}
	rest := identity[len(prefix):]
	if rest == "" || len(rest) > 63 {
		return false
	}
	for _, c := range rest {
		if c < '0' || (c > '9' && c < 'A') || (c > 'Z' && c < 'a') || c > 'z' {
			if c != '_' && c != '-' {
				return false
			}
		}
	}
	return true
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
