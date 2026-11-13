package config

import (
	"strings"
	"testing"
)

func TestPublicBrokerRejectedWithoutEcho(t *testing.T) {
	const secret = "device-secret-value-not-for-logs"
	_, err := Load(func(key string) (string, bool) {
		switch key {
		case "IOT_MQTT_BROKER_URL":
			return "tcp://116.169.215.214:18087", true
		case "IOT_MQTT_SERVICE_USERNAME":
			return "iot-dev-service", true
		case "IOT_MQTT_SERVICE_PASSWORD":
			return secret, true
		case "IOT_MQTT_DEVICE_USERNAME":
			return "root", true
		case "IOT_MQTT_DEVICE_PASSWORD":
			return secret, true
		case "IOT_MQTT_HOOK_TOKEN":
			return secret, true
		case "IOT_EMQX_API_URL":
			return "http://127.0.0.1:18083", true
		case "IOT_EMQX_API_USER":
			return "admin", true
		case "IOT_EMQX_API_PASSWORD":
			return secret, true
		default:
			return "", false
		}
	})
	if err == nil || err.Error() != "invalid IOT_MQTT_BROKER_URL" || strings.Contains(err.Error(), secret) || strings.Contains(err.Error(), "116.169.215.214") {
		t.Fatalf("error = %v", err)
	}
}

func TestIncompleteDependencyHidesSecret(t *testing.T) {
	const secret = "another-secret-value-not-for-logs"
	_, err := Load(func(key string) (string, bool) {
		if key == "IOT_MYSQL_PASSWORD" {
			return secret, true
		}
		return "", false
	})
	if err == nil || strings.Contains(err.Error(), secret) || !strings.Contains(err.Error(), "incomplete") {
		t.Fatalf("error = %v", err)
	}
}
