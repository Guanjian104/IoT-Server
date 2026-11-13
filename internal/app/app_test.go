package app

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"iot-server/internal/logger"
)

func TestRunServesAndShutsDown(t *testing.T) {
	var base string
	started := make(chan struct{})
	var readyStatus int
	var once sync.Once
	logBuf := &lockedBuffer{}
	log, err := logger.New(logBuf, "info")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan error, 1)
	go func() {
		done <- Run(ctx, Options{
			Lookup: func(key string) (string, bool) {
				switch key {
				case "IOT_HTTP_ADDR":
					return "127.0.0.1:0", true
				case "IOT_INSTANCE_ID":
					return "test-instance", true
				case "IOT_CONFIG_VERSION":
					return "test-config", true
				default:
					return "", false
				}
			},
			Logger: log,
			OnListening: func(addr net.Addr) {
				base = "http://" + addr.String()
				once.Do(func() { close(started) })
			},
			OnBeforeShutdown: func() {
				resp, err := http.Get(base + "/health/ready")
				if err != nil {
					readyStatus = -1
					return
				}
				defer resp.Body.Close()
				_, _ = io.Copy(io.Discard, resp.Body)
				readyStatus = resp.StatusCode
			},
		})
	}()

	select {
	case <-started:
	case err := <-done:
		t.Fatalf("server exited early: %v", err)
	case <-time.After(3 * time.Second):
		t.Fatal("server did not listen")
	}

	client := &http.Client{Timeout: time.Second, Transport: &http.Transport{DisableKeepAlives: true}}
	waitStatus(t, client, base+"/health/live", http.StatusOK)
	waitStatus(t, client, base+"/health/ready", http.StatusOK)
	version := waitJSON(t, client, base+"/version")
	if version["instance_id"] != "test-instance" || version["config_version"] != "test-config" || version["build_version"] == "" {
		t.Fatalf("version = %v", version)
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("shutdown did not finish")
	}
	if readyStatus != http.StatusServiceUnavailable {
		t.Fatalf("ready during shutdown = %d", readyStatus)
	}
	if strings.Contains(logBuf.String(), "super-secret-value") {
		t.Fatal("log leaked a secret")
	}
}

func TestRunConfigErrorOmitsSecret(t *testing.T) {
	const secret = "super-secret-value"
	err := Run(context.Background(), Options{
		Lookup: func(key string) (string, bool) {
			if key == "IOT_SHUTDOWN_TIMEOUT" {
				return secret, true
			}
			return "", false
		},
	})
	if err == nil || strings.Contains(err.Error(), secret) {
		t.Fatalf("error = %v", err)
	}
}

func waitStatus(t *testing.T, client *http.Client, url string, want int) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	var last error
	for time.Now().Before(deadline) {
		resp, err := client.Get(url)
		if err == nil {
			_, _ = io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
			if resp.StatusCode == want {
				return
			}
		} else {
			last = err
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("GET %s: last error %v", url, last)
}

func waitJSON(t *testing.T, client *http.Client, url string) map[string]string {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		resp, err := client.Get(url)
		if err != nil {
			time.Sleep(10 * time.Millisecond)
			continue
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode == http.StatusOK {
			var decoded map[string]string
			if err := json.Unmarshal(body, &decoded); err != nil {
				t.Fatal(err)
			}
			return decoded
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("GET %s did not return JSON", url)
	return nil
}

type lockedBuffer struct {
	mu  sync.Mutex
	buf []byte
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.buf = append(b.buf, p...)
	return len(p), nil
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return string(b.buf)
}
