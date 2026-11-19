package httpapi

import (
	"encoding/json"
	"net/http"
	"strconv"
	"time"

	"iot-server/internal/mqttmsg"
)

func (s *Server) mqttAuth(w http.ResponseWriter, r *http.Request) {
	if !s.hookOK(r) {
		s.noteDeny()
		writeDecision(w, "deny")
		return
	}
	var body struct {
		Username  string `json:"username"`
		Password  string `json:"password"`
		ClientID  string `json:"clientid"`
		ClientID2 string `json:"client_id"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&body); err != nil {
		s.noteDeny()
		writeDecision(w, "deny")
		return
	}
	if body.ClientID == "" {
		body.ClientID = body.ClientID2
	}
	kind, deviceID := mqttmsg.Classify(body.Username, body.ClientID)
	switch kind {
	case "service":
		if mqttmsg.SameSecret(body.Password, s.servicePassword) {
			writeDecision(w, "allow")
			return
		}
	case "device":
		if !mqttmsg.SameSecret(body.Password, s.devicePassword) {
			break
		}
		gate, err := s.store.DeviceGate(r.Context(), s.tenant, deviceID)
		if err != nil {
			s.log.Error("mqtt auth dependency failed", "device_id", deviceID)
			s.noteDeny()
			writeDecision(w, "deny")
			return
		}
		if gate.Found && gate.Enabled && !gate.Revoked {
			writeDecision(w, "allow")
			return
		}
	}
	s.noteDeny()
	if s.log != nil {
		s.log.Warn("mqtt auth denied", "username_kind", kind, "client_id", body.ClientID)
	}
	writeDecision(w, "deny")
}

func (s *Server) mqttACL(w http.ResponseWriter, r *http.Request) {
	if !s.hookOK(r) {
		writeDecision(w, "deny")
		return
	}
	var body struct {
		Username  string `json:"username"`
		ClientID  string `json:"clientid"`
		ClientID2 string `json:"client_id"`
		Topic     string `json:"topic"`
		Action    string `json:"action"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&body); err != nil {
		writeDecision(w, "deny")
		return
	}
	if body.ClientID == "" {
		body.ClientID = body.ClientID2
	}
	kind, deviceID := mqttmsg.Classify(body.Username, body.ClientID)
	model := ""
	if kind == "device" {
		gate, err := s.store.DeviceGate(r.Context(), s.tenant, deviceID)
		if err != nil || !gate.Found || !gate.Enabled || gate.Revoked {
			writeDecision(w, "deny")
			return
		}
		model = gate.TopicModel
	}
	writeDecision(w, mqttmsg.Decide(kind, deviceID, model, body.Action, body.Topic))
}

func (s *Server) hookOK(r *http.Request) bool {
	return mqttmsg.SameSecret(r.Header.Get("X-Iot-Hook-Token"), s.hookToken)
}

func (s *Server) noteDeny() {
	if s.metrics != nil && s.metrics.NoteAuthDeny(time.Now()) && s.log != nil {
		s.log.Warn("mqtt auth deny spike", "alert", "mqtt_auth_deny_spike")
	}
}

func writeDecision(w http.ResponseWriter, result string) {
	if result != "allow" {
		result = "deny"
	}
	payload := []byte(`{"result":"` + result + `","is_superuser":false}` + "\n")
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Length", strconv.Itoa(len(payload)))
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(payload)
}
