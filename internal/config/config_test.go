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

func TestHookListenerIsSeparate(t *testing.T) {
	cfg, err := Load(func(key string) (string, bool) {
		switch key {
		case "IOT_HTTP_ADDR":
			return "127.0.0.1:18080", true
		case "IOT_HOOK_ADDR":
			return "127.0.0.1:18082", true
		case "IOT_ENABLE_TEST_CONTROLS":
			return "1", true
		default:
			return "", false
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.HookAddr != "127.0.0.1:18082" || !cfg.EnableTestControls {
		t.Fatalf("hook = %s test = %v", cfg.HookAddr, cfg.EnableTestControls)
	}
	_, err = Load(func(key string) (string, bool) {
		if key == "IOT_HOOK_ADDR" {
			return "127.0.0.1:8080", true
		}
		return "", false
	})
	if err == nil || err.Error() != "invalid IOT_HOOK_ADDR" {
		t.Fatalf("shared addr error = %v", err)
	}
	_, err = Load(func(key string) (string, bool) {
		if key == "IOT_ENABLE_TEST_CONTROLS" {
			return "yes", true
		}
		return "", false
	})
	if err == nil || err.Error() != "invalid IOT_ENABLE_TEST_CONTROLS" {
		t.Fatalf("flag error = %v", err)
	}
}

func TestSFUAddrStaysLocal(t *testing.T) {
	cfg, err := Load(func(key string) (string, bool) {
		if key == "IOT_SFU_ADDR" {
			return "127.0.0.1:18090", true
		}
		return "", false
	})
	if err != nil || cfg.SFUAddr != "127.0.0.1:18090" {
		t.Fatalf("sfu = %s err = %v", cfg.SFUAddr, err)
	}
	for _, addr := range []string{"127.0.0.1:8080", "10.0.0.8:18090", "bad"} {
		_, err = Load(func(key string) (string, bool) {
			if key == "IOT_SFU_ADDR" {
				return addr, true
			}
			return "", false
		})
		if err == nil || err.Error() != "invalid IOT_SFU_ADDR" {
			t.Fatalf("addr %s error = %v", addr, err)
		}
	}
}

func TestSFUPublicEndpoint(t *testing.T) {
	cfg, err := Load(func(key string) (string, bool) {
		switch key {
		case "IOT_SFU_ADDR":
			return "127.0.0.1:18096", true
		case "IOT_SFU_PUBLIC_IP":
			return "203.0.113.5", true
		case "IOT_SFU_PUBLIC_URL":
			return "http://203.0.113.5:18196", true
		case "IOT_SFU_UDP_MIN":
			return "40100", true
		case "IOT_SFU_UDP_MAX":
			return "40131", true
		default:
			return "", false
		}
	})
	if err != nil || cfg.SFUPublicIP != "203.0.113.5" || cfg.SFUPublicURL != "http://203.0.113.5:18196" || cfg.SFUUDPMin != 40100 || cfg.SFUUDPMax != 40131 {
		t.Fatalf("sfu = %+v %v", cfg, err)
	}
	_, err = Load(func(key string) (string, bool) {
		switch key {
		case "IOT_SFU_ADDR":
			return "127.0.0.1:18096", true
		case "IOT_SFU_UDP_MIN":
			return "40000", true
		case "IOT_SFU_UDP_MAX":
			return "40063", true
		case "IOT_SFU_PUBLIC_IP":
			return "203.0.113.5", true
		case "IOT_SFU_PUBLIC_URL":
			return "http://203.0.113.5:18196", true
		default:
			return "", false
		}
	})
	if err == nil || err.Error() != "invalid IOT_SFU_UDP_MIN" {
		t.Fatalf("occupied range error = %v", err)
	}
}

func TestRTPDebugDir(t *testing.T) {
	cfg, err := Load(func(key string) (string, bool) {
		if key == "IOT_RTP_DEBUG_DIR" {
			return "/tmp/rtp-debug", true
		}
		return "", false
	})
	if err != nil || cfg.RTPDebugDir != "/tmp/rtp-debug" {
		t.Fatalf("dir = %s err = %v", cfg.RTPDebugDir, err)
	}
	cfg, err = Load(func(string) (string, bool) { return "", false })
	if err != nil || cfg.RTPDebugDir != "/tmp/rtp-debug" {
		t.Fatalf("default = %s err = %v", cfg.RTPDebugDir, err)
	}
	_, err = Load(func(key string) (string, bool) {
		if key == "IOT_RTP_DEBUG_DIR" {
			return "data/rtp-debug", true
		}
		return "", false
	})
	if err == nil || err.Error() != "invalid IOT_RTP_DEBUG_DIR" {
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
