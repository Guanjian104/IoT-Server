package emqx

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestConnectedDevicesPaginatesAndParsesTime(t *testing.T) {
	var logins int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v5/login":
			logins++
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"token":"t-1"}`))
		case "/api/v5/clients":
			if r.Header.Get("Authorization") != "Bearer t-1" {
				http.Error(w, "unauthorized", http.StatusUnauthorized)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			if r.URL.Query().Get("page") == "1" {
				_, _ = w.Write([]byte(`{
					"data":[
						{"clientid":"abc123abc123","connected":true,"connected_at":"2026-11-18T12:00:00.871Z"},
						{"clientid":"gone","connected":false,"connected_at":"2026-11-18T11:00:00Z"}
					],
					"meta":{"page":1,"limit":100,"count":2,"hasnext":true}
				}`))
				return
			}
			_, _ = w.Write([]byte(`{
				"data":[
					{"clientid":"def456def456","conn_state":"connected","connected_at":1763460000}
				],
				"meta":{"page":2,"limit":100,"count":1,"hasnext":false}
			}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	online, complete, err := New(server.URL, "user", "pass").ConnectedDevices(context.Background())
	if err != nil || !complete || logins != 1 {
		t.Fatalf("list = %v %v %v logins=%d", online, complete, err, logins)
	}
	want := time.Date(2026, 11, 18, 12, 0, 0, 871000000, time.UTC).UnixMilli()
	if online["abc123abc123"] != want || online["def456def456"] != 1763460000000 {
		t.Fatalf("times = %#v", online)
	}
	if _, ok := online["gone"]; ok || len(online) != 2 {
		t.Fatalf("online = %#v", online)
	}
}

func TestConnectedDevicesRetriesUnauthorized(t *testing.T) {
	var clients int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v5/login":
			_, _ = w.Write([]byte(`{"token":"t-2"}`))
		case "/api/v5/clients":
			clients++
			if clients == 1 {
				http.Error(w, "unauthorized", http.StatusUnauthorized)
				return
			}
			_, _ = w.Write([]byte(`{"data":[],"meta":{"hasnext":false}}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	online, complete, err := New(server.URL, "user", "pass").ConnectedDevices(context.Background())
	if err != nil || !complete || len(online) != 0 || clients != 2 {
		t.Fatalf("retry = %v %v %v calls=%d", online, complete, err, clients)
	}
}
