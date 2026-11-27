package httpapi

import (
	"errors"
	"net/http"

	"iot-server/internal/sfu"
)

type rtpDebugAPI interface {
	StartRTPDebug(sessionID string, opt sfu.RTPDebugOptions) error
	StopRTPDebug(sessionID string) (sfu.RTPReport, error)
	RTPDebugReport(sessionID string) (sfu.RTPReport, error)
}

func (s *Server) startRTPDebug(w http.ResponseWriter, r *http.Request) {
	if !s.hookOK(r) {
		writeError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	sessionID := r.PathValue("sessionID")
	var body struct {
		Log        bool `json:"log"`
		Capture    bool `json:"capture"`
		Seconds    int  `json:"seconds"`
		MaxPackets int  `json:"max_packets"`
		MaxBytes   int  `json:"max_bytes"`
	}
	if !decode(w, r, &body) {
		writeError(w, http.StatusBadRequest, "invalid")
		return
	}
	dbg, ok := s.rtpDebug()
	if !ok {
		writeError(w, http.StatusServiceUnavailable, "unavailable")
		return
	}
	err := dbg.StartRTPDebug(sessionID, sfu.RTPDebugOptions{
		Log:        body.Log,
		Capture:    body.Capture,
		Seconds:    body.Seconds,
		MaxPackets: body.MaxPackets,
		MaxBytes:   body.MaxBytes,
	})
	if err != nil {
		writeRTPDebugError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"session_id": sessionID,
		"running":    true,
		"log":        body.Log,
		"capture":    body.Capture,
	})
}

func (s *Server) getRTPDebug(w http.ResponseWriter, r *http.Request) {
	if !s.hookOK(r) {
		writeError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	dbg, ok := s.rtpDebug()
	if !ok {
		writeError(w, http.StatusServiceUnavailable, "unavailable")
		return
	}
	report, err := dbg.RTPDebugReport(r.PathValue("sessionID"))
	if err != nil {
		writeRTPDebugError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, report)
}

func (s *Server) stopRTPDebug(w http.ResponseWriter, r *http.Request) {
	if !s.hookOK(r) {
		writeError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	dbg, ok := s.rtpDebug()
	if !ok {
		writeError(w, http.StatusServiceUnavailable, "unavailable")
		return
	}
	report, err := dbg.StopRTPDebug(r.PathValue("sessionID"))
	if err != nil {
		writeRTPDebugError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, report)
}

func (s *Server) rtpDebug() (rtpDebugAPI, bool) {
	if s == nil || s.media == nil {
		return nil, false
	}
	dbg, ok := s.media.(rtpDebugAPI)
	return dbg, ok && dbg != nil
}

func writeRTPDebugError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, sfu.ErrRTPDebugOption):
		writeError(w, http.StatusBadRequest, "invalid")
	case errors.Is(err, sfu.ErrRTPDebugNotFound), errors.Is(err, sfu.ErrRTPDebugIdle):
		writeError(w, http.StatusNotFound, "not_found")
	case errors.Is(err, sfu.ErrRTPDebugBusy):
		writeError(w, http.StatusConflict, "conflict")
	default:
		writeError(w, http.StatusServiceUnavailable, "unavailable")
	}
}
