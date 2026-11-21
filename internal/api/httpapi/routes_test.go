package httpapi

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestInternalRoutesStayOffTheBusinessListener(t *testing.T) {
	api := New(nil, nil, nil, "dev", "hook-token", "", "", "", time.Second, nil)
	public := http.NewServeMux()
	api.Register(public)
	hook := http.NewServeMux()
	api.RegisterHook(hook, false)
	opened := http.NewServeMux()
	api.RegisterHook(opened, true)

	if status(public, http.MethodPost, "/internal/mqtt/auth") != http.StatusNotFound {
		t.Fatal("business listener exposed mqtt auth")
	}
	if status(public, http.MethodPost, "/internal/media/events") != http.StatusNotFound {
		t.Fatal("business listener exposed media events")
	}
	if status(public, http.MethodPut, "/internal/rtc/plan") != http.StatusNotFound {
		t.Fatal("business listener exposed the test plan")
	}
	if status(hook, http.MethodPost, "/api/v1/auth/login") != http.StatusNotFound {
		t.Fatal("hook listener exposed login")
	}
	if status(hook, http.MethodPut, "/internal/rtc/plan") != http.StatusNotFound {
		t.Fatal("test plan was mounted without the switch")
	}
	if status(hook, http.MethodPost, "/internal/media/events") != http.StatusUnauthorized {
		t.Fatal("media callback skipped the hook token")
	}
	if status(opened, http.MethodPut, "/internal/rtc/plan") != http.StatusUnauthorized {
		t.Fatal("opened test plan skipped the hook token")
	}
}

func status(mux *http.ServeMux, method, path string) int {
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(method, path, nil)
	mux.ServeHTTP(rec, req)
	return rec.Code
}
