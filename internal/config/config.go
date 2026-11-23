// Package config 从允许的键加载进程配置。
// 环境变量覆盖配置文件。错误信息不回显配置值。
package config

import (
	"errors"
	"fmt"
	"net"
	"os"
	"regexp"
	"strings"
	"time"
)

const maxConfigFileBytes = 64 * 1024

// Lookup 返回一个配置键是否被显式设置。
type Lookup func(key string) (string, bool)

// Config 是进程运行配置。密钥字段只存在于内存，日志经调用方脱敏。
type Config struct {
	HTTPAddr           string
	HookAddr           string
	SFUAddr            string
	SFUPublicIP        string
	SFUPublicURL       string
	SFUUDPMin          int
	SFUUDPMax          int
	InstanceID         string
	ConfigVersion      string
	LogLevel           string
	ShutdownTimeout    time.Duration
	TenantID           string
	CommandDeadline    time.Duration
	MySQLAddr          string
	MySQLUser          string
	MySQLPassword      string
	MySQLDatabase      string
	RedisAddr          string
	RedisPassword      string
	MQTTBrokerURL      string
	MQTTServiceUser    string
	MQTTServicePass    string
	MQTTDeviceUser     string
	MQTTDevicePass     string
	MQTTHookToken      string
	EcoIoTMQTTPassword string
	EMQXAPIURL         string
	EMQXAPIUser        string
	EMQXAPIPassword    string
	SeedAdminPass      string
	SeedOperatorPass   string
	StopDelay          time.Duration
	JoinGrace          time.Duration
	RingTimeout        time.Duration
	EnableTestControls bool
	mysqlEnabled       bool
	redisEnabled       bool
	mqttEnabled        bool
}

var knownKeys = []string{
	"IOT_HTTP_ADDR",
	"IOT_HOOK_ADDR",
	"IOT_SFU_ADDR",
	"IOT_SFU_PUBLIC_IP",
	"IOT_SFU_PUBLIC_URL",
	"IOT_SFU_UDP_MIN",
	"IOT_SFU_UDP_MAX",
	"IOT_INSTANCE_ID",
	"IOT_CONFIG_VERSION",
	"IOT_LOG_LEVEL",
	"IOT_SHUTDOWN_TIMEOUT",
	"IOT_CONFIG_FILE",
	"IOT_TENANT_ID",
	"IOT_COMMAND_DEADLINE",
	"IOT_MYSQL_ADDR",
	"IOT_MYSQL_USER",
	"IOT_MYSQL_PASSWORD",
	"IOT_MYSQL_DATABASE",
	"IOT_REDIS_ADDR",
	"IOT_REDIS_PASSWORD",
	"IOT_MQTT_CLIENT",
	"IOT_MQTT_BROKER_URL",
	"IOT_MQTT_SERVICE_USERNAME",
	"IOT_MQTT_SERVICE_PASSWORD",
	"IOT_MQTT_DEVICE_USERNAME",
	"IOT_MQTT_DEVICE_PASSWORD",
	"IOT_MQTT_HOOK_TOKEN",
	"IOT_ECO_IOT_MQTT_PASSWORD",
	"IOT_EMQX_API_URL",
	"IOT_EMQX_API_USER",
	"IOT_EMQX_API_PASSWORD",
	"IOT_SEED_ADMIN_PASSWORD",
	"IOT_SEED_OPERATOR_PASSWORD",
	"IOT_STOP_DELAY",
	"IOT_VIEWER_JOIN_GRACE",
	"IOT_CALL_RING_TIMEOUT",
	"IOT_ENABLE_TEST_CONTROLS",
}

var keyPattern = regexp.MustCompile(`^IOT_[A-Z0-9_]{1,64}$`)

// Load 读取配置。lookup 为 nil 时只使用默认值。
func Load(lookup Lookup) (Config, error) {
	if lookup == nil {
		lookup = func(string) (string, bool) { return "", false }
	}
	values := map[string]string{}
	if path, ok := lookup("IOT_CONFIG_FILE"); ok && strings.TrimSpace(path) != "" {
		fromFile, err := parseFile(strings.TrimSpace(path))
		if err != nil {
			return Config{}, err
		}
		values = fromFile
	}
	for _, key := range knownKeys {
		if key == "IOT_CONFIG_FILE" {
			continue
		}
		if value, ok := lookup(key); ok {
			values[key] = value
		}
	}
	return build(values)
}

func build(values map[string]string) (Config, error) {
	cfg := Config{
		HTTPAddr:        "127.0.0.1:8080",
		ConfigVersion:   "dev",
		LogLevel:        "info",
		ShutdownTimeout: 10 * time.Second,
	}
	if value, ok := values["IOT_HTTP_ADDR"]; ok && strings.TrimSpace(value) != "" {
		cfg.HTTPAddr = strings.TrimSpace(value)
	}
	if value, ok := values["IOT_CONFIG_VERSION"]; ok && strings.TrimSpace(value) != "" {
		cfg.ConfigVersion = strings.TrimSpace(value)
	}
	if value, ok := values["IOT_LOG_LEVEL"]; ok && strings.TrimSpace(value) != "" {
		cfg.LogLevel = strings.ToLower(strings.TrimSpace(value))
	}
	if value, ok := values["IOT_INSTANCE_ID"]; ok && strings.TrimSpace(value) != "" {
		cfg.InstanceID = strings.TrimSpace(value)
	}
	if value, ok := values["IOT_SHUTDOWN_TIMEOUT"]; ok && strings.TrimSpace(value) != "" {
		timeout, err := time.ParseDuration(strings.TrimSpace(value))
		if err != nil || timeout <= 0 {
			return Config{}, errors.New("invalid IOT_SHUTDOWN_TIMEOUT")
		}
		cfg.ShutdownTimeout = timeout
	}
	if err := validateAddr(cfg.HTTPAddr); err != nil {
		return Config{}, err
	}
	switch cfg.LogLevel {
	case "debug", "info", "warn", "error":
	default:
		return Config{}, errors.New("invalid IOT_LOG_LEVEL")
	}
	if cfg.InstanceID == "" {
		cfg.InstanceID = defaultInstanceID()
	}
	return finish(cfg, values)
}

func validateAddr(addr string) error {
	host, port, err := net.SplitHostPort(addr)
	if err != nil || port == "" || host == "" {
		return errors.New("invalid IOT_HTTP_ADDR")
	}
	return nil
}

func defaultInstanceID() string {
	host, err := os.Hostname()
	if err != nil || strings.TrimSpace(host) == "" {
		host = "local"
	}
	return fmt.Sprintf("%s-%d", strings.TrimSpace(host), os.Getpid())
}

func parseFile(path string) (map[string]string, error) {
	info, err := os.Stat(path)
	if err != nil || info.IsDir() {
		return nil, fmt.Errorf("cannot read config file %s", path)
	}
	if info.Size() > maxConfigFileBytes {
		return nil, errors.New("config file is too large")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("cannot read config file %s", path)
	}
	values := map[string]string{}
	for lineNumber, line := range strings.Split(string(raw), "\n") {
		line = strings.TrimSpace(strings.TrimRight(line, "\r"))
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, value, ok := strings.Cut(line, "=")
		key = strings.TrimSpace(key)
		if !ok || key == "" {
			return nil, fmt.Errorf("invalid config file line %d", lineNumber+1)
		}
		if !allowedKey(key) {
			return nil, unknownKeyError(key)
		}
		if _, exists := values[key]; exists {
			return nil, fmt.Errorf("duplicate config key %s", key)
		}
		values[key] = strings.TrimSpace(value)
	}
	return values, nil
}

func allowedKey(key string) bool {
	if key == "IOT_CONFIG_FILE" {
		return false
	}
	for _, known := range knownKeys {
		if key == known {
			return true
		}
	}
	return false
}

func unknownKeyError(key string) error {
	if keyPattern.MatchString(key) {
		return fmt.Errorf("unknown config key %s", key)
	}
	return errors.New("unknown config key")
}
