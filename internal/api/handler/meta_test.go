package handler

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"iot-server/internal/health"
)

func TestProbesAndVersion(t *testing.T) {
	state := &health.State{}
	state.SetReady(true)
	meta := Meta{InstanceID: "box-1", ConfigVersion: "cfg-1", BuildVersion: "b02", Revision: "local"}

	live := getJSON(t, http.HandlerFunc(Live), "/health/live")
	if live["status"] != "live" {
		t.Fatalf("live = %v", live)
	}
	ready := getJSON(t, Ready(state), "/health/ready")
	if ready["status"] != "ready" {
		t.Fatalf("ready = %v", ready)
	}
	state.SetReady(false)
	notReady, status := do(t, Ready(state), http.MethodGet, "/health/ready")
	if status != http.StatusServiceUnavailable || notReady["status"] != "not_ready" {
		t.Fatalf("status=%d body=%v", status, notReady)
	}
	version := getJSON(t, Version(meta), "/version")
	if version["instance_id"] != "box-1" || version["config_version"] != "cfg-1" || version["build_version"] != "b02" || version["revision"] != "local" || version["go_version"] == "" {
		t.Fatalf("version = %v", version)
	}
}

func TestHeadHasNoBody(t *testing.T) {
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodHead, "/health/live", nil)
	Live(rec, req)
	if rec.Body.Len() != 0 {
		t.Fatalf("HEAD body = %q", rec.Body.String())
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	if rec.Header().Get("Content-Length") == "" || rec.Header().Get("Content-Length") == "0" {
		t.Fatalf("HEAD content-length = %q", rec.Header().Get("Content-Length"))
	}
}

func getJSON(t *testing.T, h http.Handler, path string) map[string]string {
	t.Helper()
	body, status := do(t, h, http.MethodGet, path)
	if status != http.StatusOK {
		t.Fatalf("%s status = %d", path, status)
	}
	return body
}

func do(t *testing.T, h http.Handler, method, path string) (map[string]string, int) {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(method, path, nil))
	var body map[string]string
	if rec.Body.Len() > 0 {
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatal(err)
		}
	}
	return body, rec.Code
}
