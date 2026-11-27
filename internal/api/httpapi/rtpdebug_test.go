package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"iot-server/internal/media"
	"iot-server/internal/sfu"
)

func TestRTPDebugHook(t *testing.T) {
	node, err := sfu.Listen("127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = node.Close() })
	dir := t.TempDir()
	node.UseRTPDebugDir(dir)
	if _, err := node.Media().CreateRoom(context.Background(), media.RoomRequest{SessionID: "9030d6cb2703"}); err != nil {
		t.Fatal(err)
	}
	api := &Server{media: node.Media(), hookToken: "hook-token"}

	denied := httptest.NewRequest(http.MethodPost, "/internal/sessions/9030d6cb2703/rtp-debug", strings.NewReader(`{"log":true}`))
	denied.SetPathValue("sessionID", "9030d6cb2703")
	rec := httptest.NewRecorder()
	api.startRTPDebug(rec, denied)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d", rec.Code)
	}

	missing := rtpDebugRequest(t, http.MethodPost, "nosuch", `{"log":true}`)
	rec = httptest.NewRecorder()
	api.startRTPDebug(rec, missing)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}

	off := rtpDebugRequest(t, http.MethodPost, "9030d6cb2703", `{}`)
	rec = httptest.NewRecorder()
	api.startRTPDebug(rec, off)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d", rec.Code)
	}

	start := rtpDebugRequest(t, http.MethodPost, "9030d6cb2703", `{"log":true,"seconds":8}`)
	rec = httptest.NewRecorder()
	api.startRTPDebug(rec, start)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"capture":false`) {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	if _, err := os.Stat(filepath.Join(dir, "9030d6cb2703", "inbound", "payload.bin")); !os.IsNotExist(err) {
		t.Fatal("payload capture was created without capture=true")
	}

	again := rtpDebugRequest(t, http.MethodPost, "9030d6cb2703", `{"log":true}`)
	rec = httptest.NewRecorder()
	api.startRTPDebug(rec, again)
	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d", rec.Code)
	}

	got := rtpDebugRequest(t, http.MethodGet, "9030d6cb2703", "")
	rec = httptest.NewRecorder()
	api.getRTPDebug(rec, got)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	var running sfu.RTPReport
	if err := json.Unmarshal(rec.Body.Bytes(), &running); err != nil {
		t.Fatal(err)
	}
	if !running.Running || running.G722Confirmed {
		t.Fatalf("report = %+v", running)
	}

	stop := rtpDebugRequest(t, http.MethodDelete, "9030d6cb2703", "")
	rec = httptest.NewRecorder()
	api.stopRTPDebug(rec, stop)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	var stopped sfu.RTPReport
	if err := json.Unmarshal(rec.Body.Bytes(), &stopped); err != nil {
		t.Fatal(err)
	}
	if stopped.Running || !strings.Contains(stopped.Text, "是不是G.722：不能") {
		t.Fatalf("report = %+v", stopped)
	}
}

func rtpDebugRequest(t *testing.T, method, sessionID, body string) *http.Request {
	t.Helper()
	req := httptest.NewRequest(method, "/internal/sessions/"+sessionID+"/rtp-debug", strings.NewReader(body))
	req.SetPathValue("sessionID", sessionID)
	req.Header.Set("X-Iot-Hook-Token", "hook-token")
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	return req
}
