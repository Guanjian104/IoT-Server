package httpapi

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"iot-server/internal/metrics"
)

type kickerFunc func(context.Context, string) error

func (f kickerFunc) Kick(ctx context.Context, clientID string) error {
	return f(ctx, clientID)
}

func TestKickResultIsVisible(t *testing.T) {
	collected := metrics.New("dev", "none", "instance")
	server := New(nil, nil, collected, "dev", "", "", "", "", time.Second, kickerFunc(func(context.Context, string) error {
		return errors.New("kick failed")
	}))
	if server.kick(context.Background(), "abc123abc123") != "failed" {
		t.Fatal("failed kick was reported as success")
	}
	server.kicker = kickerFunc(func(context.Context, string) error { return nil })
	if server.kick(context.Background(), "abc123abc123") != "ok" {
		t.Fatal("successful kick was not ok")
	}
	server.kicker = nil
	if server.kick(context.Background(), "abc123abc123") != "skipped" {
		t.Fatal("missing kicker was counted")
	}
	text := collected.Text()
	if !strings.Contains(text, `iot_device_kick_total{result="ok"} 1`) || !strings.Contains(text, `iot_device_kick_total{result="failed"} 1`) {
		t.Fatal(text)
	}
}
