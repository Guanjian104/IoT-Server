package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestLoadDefaults(t *testing.T) {
	cfg, err := Load(nil)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.HTTPAddr != "127.0.0.1:8080" {
		t.Fatalf("addr = %s", cfg.HTTPAddr)
	}
	if cfg.ConfigVersion != "dev" || cfg.LogLevel != "info" || cfg.ShutdownTimeout != 10*time.Second {
		t.Fatalf("unexpected defaults: %+v", cfg)
	}
	if cfg.InstanceID == "" {
		t.Fatal("empty instance id")
	}
}

func TestLoadEnvOverridesFile(t *testing.T) {
	path := writeConfig(t, "IOT_HTTP_ADDR=127.0.0.1:19090\nIOT_CONFIG_VERSION=from-file\n")
	cfg, err := Load(func(key string) (string, bool) {
		switch key {
		case "IOT_CONFIG_FILE":
			return path, true
		case "IOT_CONFIG_VERSION":
			return "from-env", true
		default:
			return "", false
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.HTTPAddr != "127.0.0.1:19090" || cfg.ConfigVersion != "from-env" {
		t.Fatalf("cfg = %+v", cfg)
	}
}

func TestLoadHidesValues(t *testing.T) {
	const secret = "super-secret-value"
	path := writeConfig(t, "IOT_DB_PASSWORD="+secret+"\n")
	_, err := Load(func(key string) (string, bool) {
		if key == "IOT_CONFIG_FILE" {
			return path, true
		}
		return "", false
	})
	if err == nil || strings.Contains(err.Error(), secret) {
		t.Fatalf("error = %v", err)
	}

	_, err = Load(func(key string) (string, bool) {
		if key == "IOT_SHUTDOWN_TIMEOUT" {
			return "1h-" + secret, true
		}
		return "", false
	})
	if err == nil || err.Error() != "invalid IOT_SHUTDOWN_TIMEOUT" || strings.Contains(err.Error(), secret) {
		t.Fatalf("timeout error = %v", err)
	}

	_, err = Load(func(key string) (string, bool) {
		if key == "IOT_HTTP_ADDR" {
			return secret, true
		}
		return "", false
	})
	if err == nil || err.Error() != "invalid IOT_HTTP_ADDR" || strings.Contains(err.Error(), secret) {
		t.Fatalf("addr error = %v", err)
	}
}

func TestLoadRejectsEmptyHost(t *testing.T) {
	_, err := Load(func(key string) (string, bool) {
		if key == "IOT_HTTP_ADDR" {
			return ":8080", true
		}
		return "", false
	})
	if err == nil || err.Error() != "invalid IOT_HTTP_ADDR" {
		t.Fatalf("error = %v", err)
	}
}

func writeConfig(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.env")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}
