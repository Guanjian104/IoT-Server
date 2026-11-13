package logger

import (
	"bytes"
	"strings"
	"testing"
)

func TestRedactsSensitiveKeys(t *testing.T) {
	var buf bytes.Buffer
	log, err := New(&buf, "info")
	if err != nil {
		t.Fatal(err)
	}
	log.With("api_token", "super-secret-value").Info("started", "password", "super-secret-value", "addr", "127.0.0.1:8080")
	text := buf.String()
	if strings.Contains(text, "super-secret-value") {
		t.Fatalf("secret leaked: %s", text)
	}
	if !strings.Contains(text, "[redacted]") || !strings.Contains(text, "127.0.0.1:8080") {
		t.Fatalf("log = %s", text)
	}
}

func TestRejectsUnknownLevel(t *testing.T) {
	if _, err := New(&bytes.Buffer{}, "verbose"); err == nil {
		t.Fatal("expected error")
	}
}
