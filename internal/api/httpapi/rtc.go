package httpapi

import (
	"context"
	"errors"
	"net/http"
	"time"

	"iot-server/internal/mqttmsg"
	"iot-server/internal/store"
)

func (s *Server) registerRTC(mux *http.ServeMux) {
	mux.HandleFunc("POST /api/v1/devices/{id}/tickets", s.issueTicket)
	mux.HandleFunc("POST /api/v1/devices/{id}/tickets/rotate", s.rotateTicket)
	mux.HandleFunc("POST /api/v1/devices/{id}/watch", s.watch)
	mux.HandleFunc("POST /api/v1/viewers/{viewerID}/heartbeat", s.heartbeatViewer)
	mux.HandleFunc("POST /api/v1/viewers/{viewerID}/leave", s.leaveViewer)
	mux.HandleFunc("GET /api/v1/sessions/{sessionID}", s.getSession)
	mux.HandleFunc("POST /api/v1/devices/{id}/calls", s.startCall)
	mux.HandleFunc("POST /api/v1/calls/{callID}/accept", s.acceptCall)
	mux.HandleFunc("POST /api/v1/calls/{callID}/reject", s.rejectCall)
	mux.HandleFunc("POST /api/v1/calls/{callID}/end", s.endCall)
	mux.HandleFunc("GET /api/v1/calls/{callID}", s.getCall)
	mux.HandleFunc("PUT /internal/rtc/plan", s.putPlan)
	mux.HandleFunc("POST /internal/media/events", s.mediaEvent)
}

func (s *Server) issueTicket(w http.ResponseWriter, r *http.Request) {
	s.withTicketDevice(w, r, func(user store.User, device store.Device) {
		issued, err := s.store.IssueTicket(r.Context(), user, device.DeviceID)
		if !writeRTCError(w, err) {
			return
		}
		writeJSON(w, http.StatusCreated, ticketJSON(device.DeviceID, issued))
	})
}

func (s *Server) rotateTicket(w http.ResponseWriter, r *http.Request) {
	s.withTicketDevice(w, r, func(user store.User, device store.Device) {
		issued, err := s.store.RotateTicket(r.Context(), user, device.DeviceID)
		if !writeRTCError(w, err) {
			return
		}
		writeJSON(w, http.StatusOK, ticketJSON(device.DeviceID, issued))
	})
}

func (s *Server) watch(w http.ResponseWriter, r *http.Request) {
	s.withTicketDevice(w, r, func(user store.User, device store.Device) {
		var body struct {
			Ticket         string `json:"ticket"`
			ClientID       string `json:"client_id"`
			IdempotencyKey string `json:"idempotency_key"`
			ChannelID      string `json:"channel_id"`
			StreamType     string `json:"stream_type"`
			Resolution     string `json:"resolution"`
		}
		if !decode(w, r, &body) {
			writeError(w, http.StatusBadRequest, "invalid")
			return
		}
		result, err := s.store.Watch(r.Context(), user, device.DeviceID, body.Ticket, body.ClientID, body.IdempotencyKey, body.ChannelID, body.StreamType, body.Resolution)
		if !writeRTCError(w, err) {
			return
		}
		status := http.StatusCreated
		if result.Replayed {
			status = http.StatusOK
		}
		writeJSON(w, status, map[string]any{
			"session_id":     result.SessionID,
			"generation":     result.Generation,
			"session_status": result.SessionStatus,
			"viewer_id":      result.ViewerID,
			"credential": map[string]any{
				"provider":    "test",
				"connectable": false,
				"token":       result.Credential,
			},
		})
	})
}

func (s *Server) heartbeatViewer(w http.ResponseWriter, r *http.Request) {
	user, ok := s.user(w, r)
	if !ok {
		return
	}
	if err := s.store.HeartbeatViewer(r.Context(), user, r.PathValue("viewerID")); !writeRTCError(w, err) {
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"viewer_id": r.PathValue("viewerID")})
}

func (s *Server) leaveViewer(w http.ResponseWriter, r *http.Request) {
	user, ok := s.user(w, r)
	if !ok {
		return
	}
	if err := s.store.LeaveViewer(r.Context(), user, r.PathValue("viewerID")); !writeRTCError(w, err) {
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"viewer_id": r.PathValue("viewerID")})
}

func (s *Server) getSession(w http.ResponseWriter, r *http.Request) {
	user, ok := s.user(w, r)
	if !ok {
		return
	}
	view, err := s.store.GetSession(r.Context(), user, r.PathValue("sessionID"))
	if !writeRTCError(w, err) {
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"session_id":   view.SessionID,
		"device_id":    view.DeviceID,
		"status":       view.Status,
		"generation":   view.Generation,
		"device_ready": view.DeviceReady,
		"media_ready":  view.MediaReady,
		"occupants":    view.Occupants,
	})
}

func (s *Server) startCall(w http.ResponseWriter, r *http.Request) {
	s.withTicketDevice(w, r, func(user store.User, device store.Device) {
		var body struct {
			Ticket     string `json:"ticket"`
			ChannelID  string `json:"channel_id"`
			StreamType string `json:"stream_type"`
			Resolution string `json:"resolution"`
		}
		if !decode(w, r, &body) {
			writeError(w, http.StatusBadRequest, "invalid")
			return
		}
		result, err := s.store.StartCall(r.Context(), user, device.DeviceID, body.Ticket, body.ChannelID, body.StreamType, body.Resolution)
		if !writeRTCError(w, err) {
			return
		}
		writeJSON(w, http.StatusCreated, map[string]any{
			"call_id":     result.CallID,
			"session_id":  result.SessionID,
			"generation":  result.Generation,
			"status":      result.Status,
			"device_busy": false,
		})
	})
}

func (s *Server) acceptCall(w http.ResponseWriter, r *http.Request) {
	s.actCall(w, r, s.store.AcceptCall)
}

func (s *Server) rejectCall(w http.ResponseWriter, r *http.Request) {
	s.actCall(w, r, s.store.RejectCall)
}

func (s *Server) endCall(w http.ResponseWriter, r *http.Request) {
	s.actCall(w, r, s.store.EndCall)
}

func (s *Server) actCall(w http.ResponseWriter, r *http.Request, fn func(context.Context, store.User, string) error) {
	user, ok := s.user(w, r)
	if !ok {
		return
	}
	if err := fn(r.Context(), user, r.PathValue("callID")); !writeRTCError(w, err) {
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"call_id": r.PathValue("callID")})
}

func (s *Server) getCall(w http.ResponseWriter, r *http.Request) {
	user, ok := s.user(w, r)
	if !ok {
		return
	}
	view, err := s.store.GetCall(r.Context(), user, r.PathValue("callID"))
	if !writeRTCError(w, err) {
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"call_id":    view.CallID,
		"device_id":  view.DeviceID,
		"session_id": view.SessionID,
		"status":     view.Status,
		"generation": view.Generation,
	})
}

func (s *Server) putPlan(w http.ResponseWriter, r *http.Request) {
	if !s.hookOK(r) {
		writeError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	var body struct {
		DeviceID string `json:"device_id"`
		Op       string `json:"op"`
		Result   string `json:"result"`
	}
	if !decode(w, r, &body) || !mqttmsg.ValidDeviceID(body.DeviceID) {
		writeError(w, http.StatusBadRequest, "invalid")
		return
	}
	if err := s.store.PutControlPlan(r.Context(), s.tenant, body.DeviceID, body.Op, body.Result); !writeRTCError(w, err) {
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"device_id": body.DeviceID, "op": body.Op})
}

func (s *Server) mediaEvent(w http.ResponseWriter, r *http.Request) {
	if !s.hookOK(r) {
		writeError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	var body struct {
		EventID    string `json:"event_id"`
		EventType  string `json:"event_type"`
		SessionID  string `json:"session_id"`
		Identity   string `json:"identity"`
		Generation int64  `json:"generation"`
	}
	if !decode(w, r, &body) {
		writeError(w, http.StatusBadRequest, "invalid")
		return
	}
	if err := s.store.ApplyMediaEvent(r.Context(), s.tenant, body.EventID, body.EventType, body.SessionID, body.Identity, body.Generation); !writeRTCError(w, err) {
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"event_id": body.EventID})
}

func (s *Server) withTicketDevice(w http.ResponseWriter, r *http.Request, fn func(store.User, store.Device)) {
	user, ok := s.user(w, r)
	if !ok {
		return
	}
	deviceID := r.PathValue("id")
	if !mqttmsg.ValidDeviceID(deviceID) {
		writeError(w, http.StatusNotFound, "not_found")
		return
	}
	device, err := s.store.GetDevice(r.Context(), user.TenantID, deviceID)
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "not_found")
		return
	}
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, "unavailable")
		return
	}
	if !device.Enabled || device.Revoked {
		writeError(w, http.StatusConflict, "conflict")
		return
	}
	fn(user, device)
}

func writeRTCError(w http.ResponseWriter, err error) bool {
	if err == nil {
		return true
	}
	switch {
	case errors.Is(err, store.ErrNotFound):
		writeError(w, http.StatusNotFound, "not_found")
	case errors.Is(err, store.ErrForbidden):
		writeError(w, http.StatusForbidden, "forbidden")
	case errors.Is(err, store.ErrTicket):
		writeError(w, http.StatusForbidden, "ticket_invalid")
	case errors.Is(err, store.ErrBusy):
		writeError(w, http.StatusConflict, "busy")
	case errors.Is(err, store.ErrStopping):
		writeError(w, http.StatusConflict, "stopping")
	case errors.Is(err, store.ErrConflict):
		writeError(w, http.StatusConflict, "conflict")
	case errors.Is(err, store.ErrInvalid):
		writeError(w, http.StatusBadRequest, "invalid")
	default:
		writeError(w, http.StatusServiceUnavailable, "unavailable")
	}
	return false
}

func ticketJSON(deviceID string, issued store.TicketIssue) map[string]any {
	return map[string]any{
		"device_id":  deviceID,
		"version":    issued.Version,
		"expires_at": issued.Expires.UTC().Format(time.RFC3339),
		"ticket":     issued.Ticket,
	}
}
