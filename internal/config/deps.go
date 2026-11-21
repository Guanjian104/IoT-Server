package config

import (
	"errors"
	"fmt"
	"net"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// Dependencies 报告本进程是否配置了 MySQL、Redis 或 MQTT。
func (c Config) Dependencies() bool {
	return c.mysqlEnabled || c.redisEnabled || c.mqttEnabled
}

// MySQLOn 报告 MySQL 是否纳入就绪判断。
func (c Config) MySQLOn() bool { return c.mysqlEnabled }

// RedisOn 报告 Redis 是否纳入就绪判断。
func (c Config) RedisOn() bool { return c.redisEnabled }

// MQTTOn 报告 MQTT 是否纳入就绪判断。
func (c Config) MQTTOn() bool { return c.mqttEnabled }

// Secrets 返回需要从日志中去掉的长密钥。
func (c Config) Secrets() []string {
	return []string{
		c.MySQLPassword,
		c.RedisPassword,
		c.MQTTServicePass,
		c.MQTTDevicePass,
		c.MQTTHookToken,
		c.EcoIoTMQTTPassword,
		c.EMQXAPIPassword,
		c.SeedAdminPass,
		c.SeedOperatorPass,
	}
}

func finish(cfg Config, values map[string]string) (Config, error) {
	cfg.TenantID = "dev"
	if value := strings.TrimSpace(values["IOT_TENANT_ID"]); value != "" {
		if value != "dev" {
			return Config{}, errors.New("invalid IOT_TENANT_ID")
		}
		cfg.TenantID = value
	}
	cfg.CommandDeadline = 5 * time.Second
	if value := strings.TrimSpace(values["IOT_COMMAND_DEADLINE"]); value != "" {
		deadline, err := time.ParseDuration(value)
		if err != nil || deadline < time.Second || deadline > 30*time.Second {
			return Config{}, errors.New("invalid IOT_COMMAND_DEADLINE")
		}
		cfg.CommandDeadline = deadline
	}
	if value := strings.TrimSpace(values["IOT_HOOK_ADDR"]); value != "" {
		if err := validateLocalAddr(value, true); err != nil || value == cfg.HTTPAddr {
			return Config{}, errors.New("invalid IOT_HOOK_ADDR")
		}
		cfg.HookAddr = value
	}
	if value := strings.TrimSpace(values["IOT_SFU_ADDR"]); value != "" {
		if err := validateLocalAddr(value, false); err != nil || value == cfg.HTTPAddr || value == cfg.HookAddr {
			return Config{}, errors.New("invalid IOT_SFU_ADDR")
		}
		cfg.SFUAddr = value
	}
	switch strings.ToLower(strings.TrimSpace(values["IOT_ENABLE_TEST_CONTROLS"])) {
	case "", "0", "false":
		cfg.EnableTestControls = false
	case "1", "true":
		cfg.EnableTestControls = true
	default:
		return Config{}, errors.New("invalid IOT_ENABLE_TEST_CONTROLS")
	}

	mysql, mysqlOn, err := requireGroup(values, "IOT_MYSQL_ADDR", "IOT_MYSQL_USER", "IOT_MYSQL_PASSWORD", "IOT_MYSQL_DATABASE")
	if err != nil {
		return Config{}, err
	}
	redis, redisOn, err := requireGroup(values, "IOT_REDIS_ADDR", "IOT_REDIS_PASSWORD")
	if err != nil {
		return Config{}, err
	}
	mqtt, mqttOn, err := requireGroup(values,
		"IOT_MQTT_BROKER_URL",
		"IOT_MQTT_SERVICE_USERNAME",
		"IOT_MQTT_SERVICE_PASSWORD",
		"IOT_MQTT_DEVICE_USERNAME",
		"IOT_MQTT_DEVICE_PASSWORD",
		"IOT_MQTT_HOOK_TOKEN",
		"IOT_EMQX_API_URL",
		"IOT_EMQX_API_USER",
		"IOT_EMQX_API_PASSWORD",
	)
	if err != nil {
		return Config{}, err
	}
	if mysqlOn {
		if err := validateLocalAddr(mysql["IOT_MYSQL_ADDR"], false); err != nil || mysql["IOT_MYSQL_USER"] != "iot_app" || mysql["IOT_MYSQL_DATABASE"] != "iot_dev" {
			return Config{}, errors.New("invalid IOT_MYSQL_ADDR")
		}
		cfg.MySQLAddr = mysql["IOT_MYSQL_ADDR"]
		cfg.MySQLUser = mysql["IOT_MYSQL_USER"]
		cfg.MySQLPassword = mysql["IOT_MYSQL_PASSWORD"]
		cfg.MySQLDatabase = mysql["IOT_MYSQL_DATABASE"]
		cfg.mysqlEnabled = true
	}
	if redisOn {
		if err := validateLocalAddr(redis["IOT_REDIS_ADDR"], false); err != nil {
			return Config{}, errors.New("invalid IOT_REDIS_ADDR")
		}
		cfg.RedisAddr = redis["IOT_REDIS_ADDR"]
		cfg.RedisPassword = redis["IOT_REDIS_PASSWORD"]
		cfg.redisEnabled = true
	}
	if mqttOn {
		if err := validateBroker(mqtt["IOT_MQTT_BROKER_URL"]); err != nil {
			return Config{}, err
		}
		if mqtt["IOT_MQTT_SERVICE_USERNAME"] != "iot-dev-service" || mqtt["IOT_MQTT_DEVICE_USERNAME"] != "root" {
			return Config{}, errors.New("invalid IOT_MQTT_SERVICE_USERNAME")
		}
		if err := validateAPI(mqtt["IOT_EMQX_API_URL"]); err != nil {
			return Config{}, err
		}
		host, _, _ := net.SplitHostPort(cfg.HTTPAddr)
		if !loopbackHost(host) {
			return Config{}, errors.New("invalid IOT_HTTP_ADDR")
		}
		cfg.MQTTBrokerURL = mqtt["IOT_MQTT_BROKER_URL"]
		cfg.MQTTServiceUser = mqtt["IOT_MQTT_SERVICE_USERNAME"]
		cfg.MQTTServicePass = mqtt["IOT_MQTT_SERVICE_PASSWORD"]
		cfg.MQTTDeviceUser = mqtt["IOT_MQTT_DEVICE_USERNAME"]
		cfg.MQTTDevicePass = mqtt["IOT_MQTT_DEVICE_PASSWORD"]
		cfg.MQTTHookToken = mqtt["IOT_MQTT_HOOK_TOKEN"]
		cfg.EMQXAPIURL = mqtt["IOT_EMQX_API_URL"]
		cfg.EMQXAPIUser = mqtt["IOT_EMQX_API_USER"]
		cfg.EMQXAPIPassword = mqtt["IOT_EMQX_API_PASSWORD"]
		cfg.mqttEnabled = true
	}
	cfg.EcoIoTMQTTPassword = strings.TrimSpace(values["IOT_ECO_IOT_MQTT_PASSWORD"])
	cfg.SeedAdminPass = strings.TrimSpace(values["IOT_SEED_ADMIN_PASSWORD"])
	cfg.SeedOperatorPass = strings.TrimSpace(values["IOT_SEED_OPERATOR_PASSWORD"])
	if cfg.StopDelay, err = boundedDuration(values["IOT_STOP_DELAY"], 30*time.Second, time.Second, 2*time.Minute, "IOT_STOP_DELAY"); err != nil {
		return Config{}, err
	}
	if cfg.JoinGrace, err = boundedDuration(values["IOT_VIEWER_JOIN_GRACE"], 15*time.Second, time.Second, 2*time.Minute, "IOT_VIEWER_JOIN_GRACE"); err != nil {
		return Config{}, err
	}
	if cfg.RingTimeout, err = boundedDuration(values["IOT_CALL_RING_TIMEOUT"], 30*time.Second, time.Second, 2*time.Minute, "IOT_CALL_RING_TIMEOUT"); err != nil {
		return Config{}, err
	}
	public, publicOn, err := requireGroup(values, "IOT_SFU_PUBLIC_IP", "IOT_SFU_PUBLIC_URL", "IOT_SFU_UDP_MIN", "IOT_SFU_UDP_MAX")
	if err != nil {
		return Config{}, err
	}
	if publicOn {
		if cfg.SFUAddr == "" || !publicIPv4(public["IOT_SFU_PUBLIC_IP"]) || !publicSFUURL(public["IOT_SFU_PUBLIC_URL"], public["IOT_SFU_PUBLIC_IP"]) {
			return Config{}, errors.New("invalid IOT_SFU_PUBLIC_IP")
		}
		minPort, maxPort, err := udpRange(public["IOT_SFU_UDP_MIN"], public["IOT_SFU_UDP_MAX"])
		if err != nil {
			return Config{}, err
		}
		cfg.SFUPublicIP = public["IOT_SFU_PUBLIC_IP"]
		cfg.SFUPublicURL = strings.TrimRight(public["IOT_SFU_PUBLIC_URL"], "/")
		cfg.SFUUDPMin = minPort
		cfg.SFUUDPMax = maxPort
	}
	return cfg, nil
}

func publicIPv4(value string) bool {
	ip := net.ParseIP(value)
	return ip != nil && ip.To4() != nil && ip.IsGlobalUnicast() && !ip.IsPrivate()
}

func publicSFUURL(raw, ip string) bool {
	parsed, err := url.Parse(raw)
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return false
	}
	if parsed.Path != "" && parsed.Path != "/" {
		return false
	}
	host, port, err := net.SplitHostPort(parsed.Host)
	if err != nil || host == "" || port == "" || loopbackHost(host) {
		return false
	}
	if parsedIP := net.ParseIP(host); parsedIP != nil && parsedIP.String() != ip {
		return false
	}
	return true
}

func udpRange(minRaw, maxRaw string) (int, int, error) {
	minPort, errMin := strconv.Atoi(minRaw)
	maxPort, errMax := strconv.Atoi(maxRaw)
	if errMin != nil || errMax != nil || minPort < 1024 || maxPort > 65535 || minPort > maxPort || minPort <= 40063 && maxPort >= 40000 {
		return 0, 0, errors.New("invalid IOT_SFU_UDP_MIN")
	}
	return minPort, maxPort, nil
}

func boundedDuration(raw string, fallback, min, max time.Duration, key string) (time.Duration, error) {
	value := strings.TrimSpace(raw)
	if value == "" {
		return fallback, nil
	}
	parsed, err := time.ParseDuration(value)
	if err != nil || parsed < min || parsed > max {
		return 0, fmt.Errorf("invalid %s", key)
	}
	return parsed, nil
}

func requireGroup(values map[string]string, keys ...string) (map[string]string, bool, error) {
	out := make(map[string]string, len(keys))
	present := false
	for _, key := range keys {
		out[key] = strings.TrimSpace(values[key])
		if out[key] != "" {
			present = true
		}
	}
	if !present {
		return nil, false, nil
	}
	for _, key := range keys {
		if out[key] == "" {
			return nil, false, fmt.Errorf("incomplete %s", key)
		}
	}
	return out, true, nil
}

func validateBroker(raw string) error {
	parsed, err := url.Parse(raw)
	if err != nil || (parsed.Scheme != "tcp" && parsed.Scheme != "mqtt") || parsed.Port() == "" || !loopbackHost(parsed.Hostname()) {
		return errors.New("invalid IOT_MQTT_BROKER_URL")
	}
	return nil
}

func validateAPI(raw string) error {
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Scheme != "http" || !loopbackHost(parsed.Hostname()) || parsed.Port() != "18083" {
		return errors.New("invalid IOT_EMQX_API_URL")
	}
	return nil
}

func validateLocalAddr(addr string, allowPrivate bool) error {
	host, port, err := net.SplitHostPort(addr)
	if err != nil || port == "" || host == "" {
		return errors.New("invalid address")
	}
	if loopbackHost(host) {
		return nil
	}
	ip := net.ParseIP(host)
	if allowPrivate && ip != nil && ip.IsPrivate() {
		return nil
	}
	return errors.New("invalid address")
}

func loopbackHost(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}
