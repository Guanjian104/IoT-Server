//go:build integration

package integration

import (
	"bytes"
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	mqtt "github.com/eclipse/paho.mqtt.golang"
	"iot-server/internal/mqttmsg"

	_ "github.com/go-sql-driver/mysql"
)

func hookListenAddr() string {
	if addr := os.Getenv("IOT_INTEGRATION_HOOK_ADDR"); addr != "" {
		return addr
	}
	return "127.0.0.1:18082"
}

func TestP1IsolatedLoop(t *testing.T) {
	secrets := loadSecrets(t)
	root := moduleRoot(t)
	bin := filepath.Join(t.TempDir(), "server")
	build := exec.Command("go", "build", "-o", bin, "./cmd/server")
	build.Dir = root
	build.Env = append(os.Environ(), "GOTOOLCHAIN=local")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build failed: %s", scrub(string(out), secrets))
	}

	logPath := filepath.Join(t.TempDir(), "server.log")
	stop := startServer(t, bin, logPath, map[string]string{
		"IOT_HTTP_ADDR":   "127.0.0.1:18080",
		"IOT_HOOK_ADDR":   hookListenAddr(),
		"IOT_INSTANCE_ID": "iot-dev-1",
	}, secrets)
	t.Cleanup(stop)
	base := "http://127.0.0.1:18080"
	waitReady(t, base)
	waitLog(t, logPath, "mqtt subscribed", secrets)
	readyBody := map[string]string{}
	getJSON(t, base+"/health/ready", "", &readyBody)
	if readyBody["status"] != "ready" || readyBody["mqtt_subscribed"] != "true" {
		t.Fatalf("ready = %#v", readyBody)
	}

	admin := login(t, base, "admin", os.Getenv("IOT_SEED_ADMIN_PASSWORD"), secrets)
	operator := login(t, base, "operator", os.Getenv("IOT_SEED_OPERATOR_PASSWORD"), secrets)
	if loginStatus(t, base, "admin", "wrong-password") != http.StatusUnauthorized {
		t.Fatal("bad password was accepted")
	}
	if status := postJSON(t, base+"/api/v1/devices", operator, map[string]string{"device_id": "001122334455", "product_id": "controller"}, nil); status != http.StatusForbidden {
		t.Fatalf("operator create = %d", status)
	}

	deviceID := newDevice(t, base, admin)
	if status := getStatus(t, base+"/api/v1/devices/"+deviceID, operator); status != http.StatusForbidden {
		t.Fatalf("operator read before grant = %d", status)
	}
	grant(t, base, admin, deviceID, "operator", "view")
	if status := getStatus(t, base+"/api/v1/devices/"+deviceID, operator); status != http.StatusOK {
		t.Fatalf("operator read = %d", status)
	}
	if status := postJSON(t, base+"/api/v1/devices/"+deviceID+"/commands", operator, map[string]any{"identifier": "switch_light", "value": true}, nil); status != http.StatusForbidden {
		t.Fatalf("operator command = %d", status)
	}
	grant(t, base, admin, deviceID, "operator", "manage")
	grant(t, base, admin, deviceID, "operator", "call")
	grant(t, base, admin, deviceID, "operator", "ticket")

	db := openDB(t)
	defer db.Close()
	var perms int
	if err := db.QueryRow(`SELECT COUNT(*) FROM user_device_grant g JOIN app_user u ON u.id = g.user_id WHERE u.username = 'operator' AND g.device_id = ? AND g.permission IN ('call','ticket')`, deviceID).Scan(&perms); err != nil || perms != 2 {
		t.Fatalf("stored call/ticket grants = %d %v", perms, err)
	}
	if _, err := db.Exec(`INSERT IGNORE INTO product (tenant_id, product_id, name) VALUES ('other', 'controller', 'controller')`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT IGNORE INTO device (tenant_id, device_id, product_id, enabled, credential_revoked) VALUES ('other', 'abc123abc123', 'controller', 1, 0)`); err != nil {
		t.Fatal(err)
	}
	if status := getStatus(t, base+"/api/v1/devices/abc123abc123", admin); status != http.StatusNotFound {
		t.Fatalf("cross tenant = %d", status)
	}

	assertConnectDenied(t, "ffffffffffff", os.Getenv("IOT_MQTT_DEVICE_PASSWORD"), secrets)
	assertConnectDenied(t, deviceID, "wrong-password", secrets)
	assertConnectDenied(t, "app-user-1", os.Getenv("IOT_MQTT_DEVICE_PASSWORD"), secrets)
	appOpts := clientOptions("app-user-1", "app:someone", os.Getenv("IOT_MQTT_DEVICE_PASSWORD"))
	assertTokenDenied(t, appOpts, secrets)

	sim := startDevice(t, deviceID, true, 0)
	waitStatusField(t, base, admin, deviceID, "mqtt_online", true)
	statusBody := map[string]any{}
	getJSON(t, base+"/api/v1/devices/"+deviceID+"/status", admin, &statusBody)
	generation, ok := statusBody["generation_ms"].(float64)
	if !ok || generation <= 0 {
		t.Fatalf("generation = %#v", statusBody["generation_ms"])
	}
	if _, err := db.Exec(`UPDATE device_presence SET mqtt_online = 0 WHERE tenant_id = 'dev' AND device_id = ?`, deviceID); err != nil {
		t.Fatal(err)
	}
	waitStatusField(t, base, admin, deviceID, "mqtt_online", true)
	getJSON(t, base+"/api/v1/devices/"+deviceID+"/status", admin, &statusBody)
	got, ok := statusBody["generation_ms"].(float64)
	if !ok || got < generation || got-generation > 2000 {
		t.Fatalf("calibration generation = %#v, before %v", statusBody["generation_ms"], generation)
	}
	waitStatusField(t, base, admin, deviceID, "reported_status", "online")
	body := map[string]any{}
	getJSON(t, base+"/api/v1/devices/"+deviceID+"/status", admin, &body)
	if _, ok := body["online"]; ok || body["rtc_connected"] != false || body["track_ready"] != false {
		t.Fatalf("status collapsed: %#v", body)
	}

	report := sampleReport(deviceID, "s3-1", true)
	publish(t, sim.client, mqttmsg.DeviceTopic(deviceID, "report"), report, false)
	waitProperty(t, base, admin, deviceID, "switch_light", true)
	var count int
	if err := db.QueryRow(`SELECT COUNT(*) FROM device_property_history WHERE tenant_id = 'dev' AND device_id = ? AND msg_id = 's3-1'`, deviceID).Scan(&count); err != nil || count != 2 {
		t.Fatalf("history = %d %v", count, err)
	}
	publish(t, sim.client, mqttmsg.DeviceTopic(deviceID, "report"), report, false)
	time.Sleep(400 * time.Millisecond)
	if err := db.QueryRow(`SELECT COUNT(*) FROM device_property_history WHERE tenant_id = 'dev' AND device_id = ? AND msg_id = 's3-1'`, deviceID).Scan(&count); err != nil || count != 2 {
		t.Fatalf("duplicate history = %d %v", count, err)
	}
	changed := sampleReport(deviceID, "s3-1", false)
	publish(t, sim.client, mqttmsg.DeviceTopic(deviceID, "report"), changed, false)
	waitProperty(t, base, admin, deviceID, "switch_light", false)
	if err := db.QueryRow(`SELECT COUNT(*) FROM device_property_history WHERE tenant_id = 'dev' AND device_id = ? AND msg_id = 's3-1'`, deviceID).Scan(&count); err != nil || count != 4 {
		t.Fatalf("reused msgId history = %d %v", count, err)
	}
	publish(t, sim.client, mqttmsg.DeviceTopic(deviceID, "report"), []byte(`{"deviceId":"`+deviceID+`","msgId":"s3-new","time":1762070402000,"data":{"switch_light":{"value":true,"time":1762070402000}}}`), false)
	waitProperty(t, base, admin, deviceID, "switch_light", true)
	publish(t, sim.client, mqttmsg.DeviceTopic(deviceID, "report"), []byte(`{"deviceId":"`+deviceID+`","msgId":"s3-old","time":1762070401000,"data":{"switch_light":{"value":false,"time":1762070401000}}}`), false)
	time.Sleep(400 * time.Millisecond)
	var light []byte
	var reported int64
	if err := db.QueryRow(`SELECT value_json, reported_at_ms FROM device_property_current WHERE tenant_id = 'dev' AND device_id = ? AND identifier = 'switch_light'`, deviceID).Scan(&light, &reported); err != nil {
		t.Fatal(err)
	}
	if string(light) != "true" || reported != 1762070402000 {
		t.Fatalf("older report overwrote current light=%s at=%d", light, reported)
	}
	if err := db.QueryRow(`SELECT COUNT(*) FROM device_property_history WHERE tenant_id = 'dev' AND device_id = ? AND msg_id = 's3-old'`, deviceID).Scan(&count); err != nil || count != 1 {
		t.Fatalf("older report history = %d %v", count, err)
	}
	props := map[string]any{}
	getJSON(t, base+"/api/v1/devices/"+deviceID+"/properties", admin, &props)
	encoded, _ := json.Marshal(props)
	if strings.Contains(string(encoded), "switch_mist") {
		t.Fatal("missing switch was stored")
	}
	publish(t, sim.client, mqttmsg.DeviceTopic(deviceID, "report"), []byte(`{"deviceId":"`+deviceID+`","msgId":"s3-x","time":1,"data":{"nope":{"value":true,"time":1}}}`), false)
	time.Sleep(400 * time.Millisecond)
	var rejects int
	if err := db.QueryRow(`SELECT COUNT(*) FROM inbound_reject WHERE device_id = ?`, deviceID).Scan(&rejects); err != nil || rejects == 0 {
		t.Fatalf("rejects = %d %v", rejects, err)
	}

	if status := postJSON(t, base+"/api/v1/devices/"+deviceID+"/commands", operator, map[string]any{"identifier": "temp_humidity_sens_2", "value": false}, nil); status != http.StatusBadRequest {
		t.Fatalf("sensor command = %d", status)
	}
	command := map[string]any{}
	if status := postJSON(t, base+"/api/v1/devices/"+deviceID+"/commands", operator, map[string]any{"identifier": "switch_light", "value": true}, &command); status != http.StatusAccepted {
		t.Fatalf("command = %d", status)
	}
	waitCommand(t, base, admin, command["command_id"].(string), "accepted")
	waitProperty(t, base, admin, deviceID, "switch_light", true)
	if sim.count() != 1 || sim.executions() != 1 {
		t.Fatalf("publishes = %d executions = %d", sim.count(), sim.executions())
	}
	var setMsgID string
	var setPayload []byte
	if err := db.QueryRow(`SELECT msg_id, payload FROM device_command WHERE command_id = ?`, command["command_id"]).Scan(&setMsgID, &setPayload); err != nil {
		t.Fatal(err)
	}
	var lightHistory int
	if err := db.QueryRow(`SELECT COUNT(*) FROM device_property_history WHERE tenant_id = 'dev' AND device_id = ? AND msg_id = ?`, deviceID, "r-"+setMsgID).Scan(&lightHistory); err != nil || lightHistory != 1 {
		t.Fatalf("command history = %d %v", lightHistory, err)
	}
	publishAsService(t, mqttmsg.DeviceTopic(deviceID, "set"), setPayload)
	waitSimCount(t, sim, 2)
	time.Sleep(400 * time.Millisecond)
	if sim.executions() != 1 || sim.replyCount() != 2 {
		t.Fatalf("duplicate msgId executions = %d replies = %d", sim.executions(), sim.replyCount())
	}
	if err := db.QueryRow(`SELECT COUNT(*) FROM device_property_history WHERE tenant_id = 'dev' AND device_id = ? AND msg_id = ?`, deviceID, "r-"+setMsgID).Scan(&lightHistory); err != nil || lightHistory != 1 {
		t.Fatalf("duplicate command history = %d %v", lightHistory, err)
	}
	waitCommand(t, base, admin, command["command_id"].(string), "accepted")

	silentID := newDevice(t, base, admin)
	grant(t, base, admin, silentID, "operator", "manage")
	silent := startDevice(t, silentID, false, 0)
	silentCommand := map[string]any{}
	if status := postJSON(t, base+"/api/v1/devices/"+silentID+"/commands", operator, map[string]any{"identifier": "switch_fan", "value": false}, &silentCommand); status != http.StatusAccepted {
		t.Fatalf("silent command = %d", status)
	}
	waitCommand(t, base, admin, silentCommand["command_id"].(string), "timeout")
	var msgID string
	if err := db.QueryRow(`SELECT msg_id FROM device_command WHERE command_id = ?`, silentCommand["command_id"]).Scan(&msgID); err != nil {
		t.Fatal(err)
	}
	late := []byte(fmt.Sprintf(`{"deviceId":"%s","msgId":"%s","time":2,"code":0}`, silentID, msgID))
	publish(t, silent.client, mqttmsg.DeviceTopic(silentID, "reply"), late, false)
	time.Sleep(500 * time.Millisecond)
	waitCommand(t, base, admin, silentCommand["command_id"].(string), "timeout")

	failID := newDevice(t, base, admin)
	grant(t, base, admin, failID, "operator", "manage")
	_ = startDevice(t, failID, true, 1002)
	failCommand := map[string]any{}
	postJSON(t, base+"/api/v1/devices/"+failID+"/commands", operator, map[string]any{"identifier": "switch_heat", "value": true}, &failCommand)
	waitCommand(t, base, admin, failCommand["command_id"].(string), "failed")

	otherID := newDevice(t, base, admin)
	otherSim := startDevice(t, otherID, false, 0)
	otherSim.client.Subscribe("#", 1, nil).WaitTimeout(3 * time.Second)
	waitDisconnect(t, otherSim.client)

	disabled := map[string]any{}
	if status := postJSON(t, base+"/api/v1/devices/"+deviceID+"/disable", admin, map[string]any{}, &disabled); status != http.StatusOK || disabled["kick"] != "ok" {
		t.Fatalf("disable = %d %#v", status, disabled)
	}
	waitDisconnect(t, sim.client)
	assertConnectDenied(t, deviceID, os.Getenv("IOT_MQTT_DEVICE_PASSWORD"), secrets)
	postJSON(t, base+"/api/v1/devices/"+deviceID+"/enable", admin, map[string]any{}, nil)

	log2 := filepath.Join(t.TempDir(), "server-2.log")
	stop2 := startServer(t, bin, log2, map[string]string{
		"IOT_HTTP_ADDR":   "127.0.0.1:18081",
		"IOT_HOOK_ADDR":   "",
		"IOT_SFU_ADDR":    "",
		"IOT_INSTANCE_ID": "iot-dev-2",
	}, secrets)
	t.Cleanup(stop2)
	waitReady(t, "http://127.0.0.1:18081")
	waitLog(t, log2, "mqtt subscribed", secrets)
	ready2 := map[string]string{}
	getJSON(t, "http://127.0.0.1:18081/health/ready", "", &ready2)
	if ready2["status"] != "ready" || ready2["mqtt_subscribed"] != "true" {
		t.Fatalf("server 2 ready = %#v", ready2)
	}
	dualID := newDevice(t, base, admin)
	grant(t, base, admin, dualID, "operator", "manage")
	dual := startDevice(t, dualID, true, 0)
	dualCommand := map[string]any{}
	postJSON(t, base+"/api/v1/devices/"+dualID+"/commands", operator, map[string]any{"identifier": "switch_bask", "value": true}, &dualCommand)
	waitCommand(t, base, admin, dualCommand["command_id"].(string), "accepted")
	if dual.count() != 1 || dual.executions() != 1 {
		t.Fatalf("dual publishes = %d executions = %d", dual.count(), dual.executions())
	}
	dual.pauseReplies()
	first := map[string]any{}
	second := map[string]any{}
	if status := postJSON(t, base+"/api/v1/devices/"+dualID+"/commands", operator, map[string]any{"identifier": "switch_fan", "value": true}, &first); status != http.StatusAccepted {
		t.Fatalf("first serial command = %d", status)
	}
	if status := postJSON(t, base+"/api/v1/devices/"+dualID+"/commands", operator, map[string]any{"identifier": "switch_feed", "value": true}, &second); status != http.StatusAccepted {
		t.Fatalf("second serial command = %d", status)
	}
	waitSimCount(t, dual, 2)
	time.Sleep(800 * time.Millisecond)
	if dual.count() != 2 {
		t.Fatalf("second command left the queue early: %d", dual.count())
	}
	dual.resumeReplies()
	waitCommand(t, base, admin, first["command_id"].(string), "accepted")
	waitCommand(t, base, admin, second["command_id"].(string), "accepted")
	if dual.count() != 3 || dual.executions() != 3 {
		t.Fatalf("serial publishes = %d executions = %d", dual.count(), dual.executions())
	}

	for i := 0; i < 5; i++ {
		postAuth(t, "http://"+hookListenAddr(), os.Getenv("IOT_MQTT_HOOK_TOKEN"))
	}
	waitLog(t, logPath, "mqtt_auth_deny_spike", secrets)
	metricsBody := getRaw(t, base+"/metrics")
	if strings.Contains(metricsBody, deviceID) || !strings.Contains(metricsBody, "iot_mqtt_auth_deny_total") {
		t.Fatal("metrics label check failed")
	}
	if !strings.Contains(metricsBody, `iot_device_kick_total{result="ok"}`) || !strings.Contains(metricsBody, `iot_device_kick_total{result="failed"}`) || !strings.Contains(metricsBody, "iot_command_attention ") {
		t.Fatal("ops metrics missing")
	}
	beforeAttention := map[string]string{}
	getJSON(t, base+"/health/ready", "", &beforeAttention)
	attentionID := "m10att00-0000-4000-8000-000000000001"
	if _, err := db.Exec(`DELETE FROM device_command WHERE command_id = ?`, attentionID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO device_command (command_id, tenant_id, device_id, msg_id, state, payload, deadline_at) VALUES (?, 'dev', ?, 'srv-m10-attention', 'needs_attention', ?, DATE_ADD(UTC_TIMESTAMP(3), INTERVAL 1 HOUR))`, attentionID, dualID, []byte(`{}`)); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = db.Exec(`DELETE FROM device_command WHERE command_id = ?`, attentionID) })
	nextAttention := strconv.Itoa(attentionCount(beforeAttention["command_attention"]) + 1)
	waitReadyValue(t, base, "command_attention", nextAttention)
	if body := getRaw(t, base+"/metrics"); !strings.Contains(body, "iot_command_attention "+nextAttention) {
		t.Fatalf("attention metric = %s", body)
	}
	if _, err := db.Exec(`DELETE FROM device_command WHERE command_id = ?`, attentionID); err != nil {
		t.Fatal(err)
	}
	waitReadyValue(t, base, "command_attention", beforeAttention["command_attention"])

	if err := exec.Command("docker", "stop", "iot-dev-redis").Run(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = exec.Command("docker", "start", "iot-dev-redis").Run() })
	waitNotReady(t, base)
	if loginStatus(t, base, "admin", os.Getenv("IOT_SEED_ADMIN_PASSWORD")) != http.StatusServiceUnavailable {
		t.Fatal("login allowed while redis is down")
	}
	if err := exec.Command("docker", "start", "iot-dev-redis").Run(); err != nil {
		t.Fatal(err)
	}
	waitReady(t, base)

	replayID := newDevice(t, base, admin)
	grant(t, base, admin, replayID, "operator", "manage")
	replaySim := startDevice(t, replayID, false, 0)
	unlock := lockTablesRead(t)
	t.Cleanup(unlock)
	publish(t, replaySim.client, mqttmsg.DeviceTopic(replayID, "report"), sampleReport(replayID, "replay-db", true), false)
	waitLogs(t, []string{logPath, log2}, "mqtt dispatch failed", secrets)
	unlock()
	waitProperty(t, base, admin, replayID, "switch_light", true)

	if err := exec.Command("docker", "stop", "iot-dev-mysql").Run(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = exec.Command("docker", "start", "iot-dev-mysql").Run() })
	time.Sleep(time.Second)
	assertConnectDenied(t, deviceID, os.Getenv("IOT_MQTT_DEVICE_PASSWORD"), secrets)
	if err := exec.Command("docker", "start", "iot-dev-mysql").Run(); err != nil {
		t.Fatal(err)
	}
	waitReady(t, base)
	scanLogs(t, []string{logPath, log2}, secrets)
}

func loadSecrets(t *testing.T) []string {
	t.Helper()
	var out []string
	for _, key := range []string{
		"IOT_MYSQL_PASSWORD", "IOT_MYSQL_ROOT_PASSWORD", "IOT_REDIS_PASSWORD",
		"IOT_MQTT_SERVICE_PASSWORD", "IOT_MQTT_DEVICE_PASSWORD", "IOT_MQTT_HOOK_TOKEN",
		"IOT_EMQX_API_PASSWORD", "IOT_SEED_ADMIN_PASSWORD", "IOT_SEED_OPERATOR_PASSWORD",
		"EMQX_DEV_DASHBOARD_PASSWORD", "EMQX_DEV_APP_PASSWORD", "EMQX_NODE_COOKIE",
	} {
		value := os.Getenv(key)
		if value == "" {
			t.Fatalf("missing %s", key)
		}
		if len(value) >= 12 {
			out = append(out, value)
		}
	}
	return out
}

func moduleRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("go.mod not found")
		}
		dir = parent
	}
}

func startServer(t *testing.T, bin, logPath string, overrides map[string]string, secrets []string) func() {
	t.Helper()
	if overrides["IOT_HTTP_ADDR"] != "" {
		ln, err := net.Listen("tcp", overrides["IOT_HTTP_ADDR"])
		if err != nil {
			t.Fatalf("address busy %s", overrides["IOT_HTTP_ADDR"])
		}
		ln.Close()
	}
	logFile, err := os.Create(logPath)
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(bin)
	cmd.Env = childEnv(overrides)
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	return func() {
		_ = cmd.Process.Signal(os.Interrupt)
		done := make(chan struct{})
		go func() {
			_ = cmd.Wait()
			close(done)
		}()
		select {
		case <-done:
		case <-time.After(8 * time.Second):
			_ = cmd.Process.Kill()
		}
		_ = logFile.Close()
		body, _ := os.ReadFile(logPath)
		if leaked(string(body), secrets) {
			t.Errorf("secret appeared in %s", filepath.Base(logPath))
		} else if t.Failed() {
			fmt.Fprintln(os.Stderr, scrub(tail(string(body)), secrets))
		}
	}
}

func childEnv(overrides map[string]string) []string {
	keys := []string{
		"PATH", "IOT_CONFIG_VERSION", "IOT_LOG_LEVEL", "IOT_SHUTDOWN_TIMEOUT", "IOT_TENANT_ID",
		"IOT_COMMAND_DEADLINE", "IOT_MYSQL_ADDR", "IOT_MYSQL_USER", "IOT_MYSQL_PASSWORD", "IOT_MYSQL_DATABASE",
		"IOT_REDIS_ADDR", "IOT_REDIS_PASSWORD", "IOT_MQTT_BROKER_URL", "IOT_MQTT_SERVICE_USERNAME",
		"IOT_MQTT_SERVICE_PASSWORD", "IOT_MQTT_DEVICE_USERNAME", "IOT_MQTT_DEVICE_PASSWORD", "IOT_MQTT_HOOK_TOKEN",
		"IOT_EMQX_API_URL", "IOT_EMQX_API_USER", "IOT_EMQX_API_PASSWORD", "IOT_SEED_ADMIN_PASSWORD", "IOT_SEED_OPERATOR_PASSWORD",
		"IOT_STOP_DELAY", "IOT_VIEWER_JOIN_GRACE", "IOT_CALL_RING_TIMEOUT",
		"IOT_HOOK_ADDR", "IOT_SFU_ADDR", "IOT_ENABLE_TEST_CONTROLS",
	}
	env := []string{"TZ=UTC", "GOTOOLCHAIN=local"}
	for _, key := range keys {
		if value, ok := overrides[key]; ok {
			env = append(env, key+"="+value)
			continue
		}
		if value, ok := os.LookupEnv(key); ok {
			env = append(env, key+"="+value)
		}
	}
	for key, value := range overrides {
		if key == "IOT_HTTP_ADDR" || key == "IOT_INSTANCE_ID" {
			env = append(env, key+"="+value)
		}
	}
	return env
}

func waitReady(t *testing.T, base string) {
	t.Helper()
	waitCode(t, base+"/health/ready", http.StatusOK, 45*time.Second)
}

func waitNotReady(t *testing.T, base string) {
	t.Helper()
	waitCode(t, base+"/health/ready", http.StatusServiceUnavailable, 20*time.Second)
}

func waitCode(t *testing.T, raw string, want int, limit time.Duration) {
	t.Helper()
	deadline := time.Now().Add(limit)
	var last int
	for time.Now().Before(deadline) {
		resp, err := http.Get(raw)
		if err == nil {
			last = resp.StatusCode
			resp.Body.Close()
			if last == want {
				return
			}
		}
		time.Sleep(250 * time.Millisecond)
	}
	t.Fatalf("%s stayed %d", raw, last)
}

func waitLogs(t *testing.T, paths []string, text string, secrets []string) {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		for _, path := range paths {
			body, err := os.ReadFile(path)
			if err != nil {
				continue
			}
			if leaked(string(body), secrets) {
				t.Fatal("secret appeared in log")
			}
			if strings.Contains(string(body), text) {
				return
			}
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatalf("log missing %s", text)
}

func lockTablesRead(t *testing.T) func() {
	t.Helper()
	dsn := fmt.Sprintf("%s@tcp(%s)/%s?timeout=3s&readTimeout=30s&writeTimeout=30s",
		url.UserPassword("root", os.Getenv("IOT_MYSQL_ROOT_PASSWORD")).String(),
		os.Getenv("IOT_MYSQL_ADDR"), os.Getenv("IOT_MYSQL_DATABASE"))
	db, err := sql.Open("mysql", dsn)
	if err != nil {
		t.Fatal("root open failed")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	conn, err := db.Conn(ctx)
	cancel()
	if err != nil {
		db.Close()
		t.Fatal("root conn failed")
	}
	ctx, cancel = context.WithTimeout(context.Background(), 5*time.Second)
	_, err = conn.ExecContext(ctx, "FLUSH TABLES WITH READ LOCK")
	cancel()
	if err != nil {
		conn.Close()
		db.Close()
		t.Fatal("read lock failed")
	}
	var once sync.Once
	return func() {
		once.Do(func() {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			_, _ = conn.ExecContext(ctx, "UNLOCK TABLES")
			conn.Close()
			db.Close()
		})
	}
}

func waitLog(t *testing.T, path, text string, secrets []string) {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		body, err := os.ReadFile(path)
		if err == nil {
			if leaked(string(body), secrets) {
				t.Fatal("secret appeared in log")
			}
			if strings.Contains(string(body), text) {
				return
			}
		}
		time.Sleep(200 * time.Millisecond)
	}
	body, _ := os.ReadFile(path)
	t.Fatalf("log missing %s\n%s", text, scrub(tail(string(body)), secrets))
}

func login(t *testing.T, base, username, password string, secrets []string) string {
	t.Helper()
	var out struct {
		Token string `json:"token"`
	}
	status := postJSON(t, base+"/api/v1/auth/login", "", map[string]string{"username": username, "password": password}, &out)
	if status != http.StatusOK || out.Token == "" {
		t.Fatalf("login %s status %d", username, status)
	}
	if leaked(out.Token, secrets) {
		t.Fatal("token matched a secret")
	}
	return out.Token
}

func loginStatus(t *testing.T, base, username, password string) int {
	t.Helper()
	return postJSON(t, base+"/api/v1/auth/login", "", map[string]string{"username": username, "password": password}, nil)
}

func newDevice(t *testing.T, base, token string) string {
	t.Helper()
	buf := make([]byte, 6)
	if _, err := rand.Read(buf); err != nil {
		t.Fatal(err)
	}
	id := hex.EncodeToString(buf)
	status := postJSON(t, base+"/api/v1/devices", token, map[string]string{"device_id": id, "product_id": "controller"}, nil)
	if status != http.StatusCreated {
		t.Fatalf("create device %d", status)
	}
	return id
}

func grant(t *testing.T, base, token, deviceID, username, permission string) {
	t.Helper()
	status := postJSON(t, base+"/api/v1/devices/"+deviceID+"/grants", token, map[string]string{"username": username, "permission": permission}, nil)
	if status != http.StatusOK {
		t.Fatalf("grant %s %d", permission, status)
	}
}

func postJSON(t *testing.T, raw, token string, body any, dest any) int {
	t.Helper()
	payload, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	req, err := http.NewRequest(http.MethodPost, raw, bytes.NewReader(payload))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	if dest != nil && resp.StatusCode < 300 {
		if err := json.Unmarshal(data, dest); err != nil {
			t.Fatal(err)
		}
	}
	return resp.StatusCode
}

func getJSON(t *testing.T, raw, token string, dest any) {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, raw, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("get %s %d", raw, resp.StatusCode)
	}
	if err := json.Unmarshal(data, dest); err != nil {
		t.Fatal(err)
	}
}

func getStatus(t *testing.T, raw, token string) int {
	t.Helper()
	req, _ := http.NewRequest(http.MethodGet, raw, nil)
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	return resp.StatusCode
}

func getRaw(t *testing.T, raw string) string {
	t.Helper()
	resp, err := http.Get(raw)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	return string(data)
}

func waitStatusField(t *testing.T, base, token, deviceID, field string, want any) {
	t.Helper()
	deadline := time.Now().Add(12 * time.Second)
	for time.Now().Before(deadline) {
		body := map[string]any{}
		getJSON(t, base+"/api/v1/devices/"+deviceID+"/status", token, &body)
		if body[field] == want {
			return
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatalf("status %s did not become %#v", field, want)
}

func waitProperty(t *testing.T, base, token, deviceID, identifier string, want bool) {
	t.Helper()
	deadline := time.Now().Add(12 * time.Second)
	for time.Now().Before(deadline) {
		body := struct {
			Current []struct {
				Identifier string `json:"identifier"`
				Value      any    `json:"value"`
			} `json:"current"`
		}{}
		getJSON(t, base+"/api/v1/devices/"+deviceID+"/properties", token, &body)
		for _, item := range body.Current {
			got, _ := item.Value.(bool)
			if item.Identifier == identifier && got == want {
				return
			}
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatalf("property %s did not become %v", identifier, want)
}

func waitSimCount(t *testing.T, sim *deviceSim, want int) {
	t.Helper()
	deadline := time.Now().Add(12 * time.Second)
	for time.Now().Before(deadline) {
		if sim.count() >= want {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("device saw %d sets, want %d", sim.count(), want)
}

func publishAsService(t *testing.T, topic string, payload []byte) {
	t.Helper()
	opts := clientOptions(mqttmsg.ServiceClientID("iot-dev-probe"), mqttmsg.ServiceUsername, os.Getenv("IOT_MQTT_SERVICE_PASSWORD"))
	opts.SetCleanSession(true)
	client := mqtt.NewClient(opts)
	token := client.Connect()
	if !token.WaitTimeout(8*time.Second) || token.Error() != nil {
		reason := "timeout"
		if token.Error() != nil {
			reason = token.Error().Error()
		}
		t.Fatal(scrub("service publish connect failed: "+reason, []string{os.Getenv("IOT_MQTT_SERVICE_PASSWORD"), os.Getenv("IOT_MQTT_DEVICE_PASSWORD")}))
	}
	defer client.Disconnect(100)
	publish(t, client, topic, payload, false)
}

func waitReadyValue(t *testing.T, base, key, want string) {
	t.Helper()
	deadline := time.Now().Add(12 * time.Second)
	var last map[string]string
	for time.Now().Before(deadline) {
		last = map[string]string{}
		getJSON(t, base+"/health/ready", "", &last)
		if last["status"] == "ready" && last[key] == want {
			return
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatalf("ready %s = %#v, want %s", key, last, want)
}

func attentionCount(value string) int {
	n, err := strconv.Atoi(value)
	if err != nil {
		return 0
	}
	return n
}

func waitCommand(t *testing.T, base, token, commandID, want string) {
	t.Helper()
	deadline := time.Now().Add(12 * time.Second)
	var last string
	for time.Now().Before(deadline) {
		body := map[string]any{}
		getJSON(t, base+"/api/v1/commands/"+commandID, token, &body)
		last, _ = body["state"].(string)
		if last == want {
			return
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatalf("command %s stayed %s", commandID, last)
}

type deviceSim struct {
	mu       sync.Mutex
	client   mqtt.Client
	sets     [][]byte
	deferred [][]byte
	executed map[string]struct{}
	replies  int
	pause    bool
	reply    bool
	code     int
	deviceID string
}

func (s *deviceSim) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.sets)
}

func (s *deviceSim) executions() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.executed)
}

func (s *deviceSim) replyCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.replies
}

func (s *deviceSim) pauseReplies() {
	s.mu.Lock()
	s.pause = true
	s.mu.Unlock()
}

func (s *deviceSim) resumeReplies() {
	s.mu.Lock()
	s.pause = false
	batch := s.deferred
	s.deferred = nil
	client := s.client
	s.mu.Unlock()
	for _, payload := range batch {
		s.respond(client, payload)
	}
}

func (s *deviceSim) respond(client mqtt.Client, payload []byte) {
	if !s.reply || client == nil {
		return
	}
	var doc struct {
		MsgID string          `json:"msgId"`
		Data  map[string]bool `json:"data"`
	}
	if json.Unmarshal(payload, &doc) != nil || doc.MsgID == "" {
		return
	}
	s.mu.Lock()
	_, done := s.executed[doc.MsgID]
	if !done {
		if s.executed == nil {
			s.executed = map[string]struct{}{}
		}
		s.executed[doc.MsgID] = struct{}{}
	}
	s.replies++
	code := s.code
	deviceID := s.deviceID
	s.mu.Unlock()
	replyBody := fmt.Sprintf(`{"deviceId":"%s","msgId":"%s","time":%d,"code":%d}`, deviceID, doc.MsgID, time.Now().UnixMilli(), code)
	client.Publish(mqttmsg.DeviceTopic(deviceID, "reply"), 1, false, replyBody).WaitTimeout(3 * time.Second)
	if done {
		return
	}
	for identifier, value := range doc.Data {
		report := fmt.Sprintf(`{"deviceId":"%s","msgId":"%s","time":%d,"data":{"%s":{"value":%t,"time":%d}}}`, deviceID, "r-"+doc.MsgID, time.Now().UnixMilli(), identifier, value, time.Now().UnixMilli())
		client.Publish(mqttmsg.DeviceTopic(deviceID, "report"), 1, false, report).WaitTimeout(3 * time.Second)
	}
}

func startDevice(t *testing.T, deviceID string, reply bool, code int) *deviceSim {
	t.Helper()
	sim := &deviceSim{reply: reply, code: code, deviceID: deviceID}
	ready := make(chan struct{})
	var once sync.Once
	opts := clientOptions(deviceID, "root", os.Getenv("IOT_MQTT_DEVICE_PASSWORD"))
	opts.SetOrderMatters(false)
	opts.SetWill(mqttmsg.DeviceTopic(deviceID, "status"), "offline", 1, true)
	opts.SetOnConnectHandler(func(client mqtt.Client) {
		client.Publish(mqttmsg.DeviceTopic(deviceID, "status"), 1, true, "online")
		token := client.Subscribe(mqttmsg.DeviceTopic(deviceID, "set"), 1, func(_ mqtt.Client, msg mqtt.Message) {
			payload := append([]byte(nil), msg.Payload()...)
			sim.mu.Lock()
			sim.sets = append(sim.sets, payload)
			paused := sim.pause
			if paused {
				sim.deferred = append(sim.deferred, payload)
			}
			sim.mu.Unlock()
			if paused {
				return
			}
			sim.respond(client, payload)
		})
		go func() {
			token.WaitTimeout(3 * time.Second)
			once.Do(func() { close(ready) })
		}()
	})
	client := mqtt.NewClient(opts)
	token := client.Connect()
	if !token.WaitTimeout(8*time.Second) || token.Error() != nil {
		reason := "timeout"
		if token.Error() != nil {
			reason = token.Error().Error()
		}
		t.Fatal(scrub("device connect failed: "+reason, []string{os.Getenv("IOT_MQTT_DEVICE_PASSWORD"), os.Getenv("IOT_MQTT_SERVICE_PASSWORD")}))
	}
	select {
	case <-ready:
	case <-time.After(8 * time.Second):
		t.Fatal("device subscribe failed")
	}
	sim.client = client
	t.Cleanup(func() { client.Disconnect(100) })
	return sim
}

func clientOptions(clientID, username, password string) *mqtt.ClientOptions {
	opts := mqtt.NewClientOptions()
	opts.AddBroker(os.Getenv("IOT_MQTT_BROKER_URL"))
	opts.SetClientID(clientID)
	opts.SetUsername(username)
	opts.SetPassword(password)
	opts.SetProtocolVersion(4)
	opts.SetCleanSession(true)
	opts.SetAutoReconnect(false)
	opts.SetConnectTimeout(8 * time.Second)
	return opts
}

func postAuth(t *testing.T, base, hook string) {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, base+"/internal/mqtt/auth", bytes.NewReader([]byte(`{"username":"root","password":"wrong","clientid":"00aabbccddee"}`)))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Iot-Hook-Token", hook)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("auth hook status %d", resp.StatusCode)
	}
}

func assertConnectDenied(t *testing.T, clientID, password string, secrets []string) {
	t.Helper()
	username := "root"
	if strings.HasPrefix(clientID, "app-") {
		username = "app:someone"
	}
	assertTokenDenied(t, clientOptions(clientID, username, password), secrets)
}

func assertTokenDenied(t *testing.T, opts *mqtt.ClientOptions, secrets []string) {
	t.Helper()
	client := mqtt.NewClient(opts)
	token := client.Connect()
	ok := token.WaitTimeout(8 * time.Second)
	err := token.Error()
	// 先记下连接结果，再断开。断开后的 IsConnected 不能当作认证被拒绝。
	connected := client.IsConnected()
	client.Disconnect(50)
	if ok && err == nil && connected {
		t.Fatal("connection was allowed")
	}
	if err != nil && leaked(err.Error(), secrets) {
		t.Fatal("secret appeared in connect error")
	}
}

func publish(t *testing.T, client mqtt.Client, topic string, payload []byte, retained bool) {
	t.Helper()
	token := client.Publish(topic, 1, retained, payload)
	if !token.WaitTimeout(5*time.Second) || token.Error() != nil {
		t.Fatalf("publish %s failed", topic)
	}
}

func waitDisconnect(t *testing.T, client mqtt.Client) {
	t.Helper()
	deadline := time.Now().Add(8 * time.Second)
	for time.Now().Before(deadline) {
		if !client.IsConnected() {
			return
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatal("client stayed connected")
}

func sampleReport(deviceID, msgID string, light bool) []byte {
	return []byte(fmt.Sprintf(`{"deviceId":"%s","msgId":"%s","time":1762070400000,"data":{"temp_humidity_sens_2":{"value":"H:23.7C,49%%;C:23.8C,48%%","time":1762070400000},"switch_light":{"value":%t,"time":1762070400000}}}`, deviceID, msgID, light))
}

func openDB(t *testing.T) *sql.DB {
	t.Helper()
	dsn := fmt.Sprintf("%s@tcp(%s)/%s?parseTime=true&loc=UTC&charset=utf8mb4&timeout=3s&readTimeout=5s&writeTimeout=5s",
		url.UserPassword(os.Getenv("IOT_MYSQL_USER"), os.Getenv("IOT_MYSQL_PASSWORD")).String(),
		os.Getenv("IOT_MYSQL_ADDR"), os.Getenv("IOT_MYSQL_DATABASE"))
	db, err := sql.Open("mysql", dsn)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := db.PingContext(ctx); err != nil {
		t.Fatal("db ping failed")
	}
	return db
}

func scanLogs(t *testing.T, paths []string, secrets []string) {
	t.Helper()
	for _, path := range paths {
		body, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if leaked(string(body), secrets) {
			t.Fatalf("secret appeared in %s", filepath.Base(path))
		}
	}
}

func leaked(text string, secrets []string) bool {
	for _, secret := range secrets {
		if secret != "" && strings.Contains(text, secret) {
			return true
		}
	}
	return false
}

func tail(text string) string {
	lines := strings.Split(text, "\n")
	if len(lines) > 40 {
		lines = lines[len(lines)-40:]
	}
	return strings.Join(lines, "\n")
}

func scrub(text string, secrets []string) string {
	for _, secret := range secrets {
		text = strings.ReplaceAll(text, secret, "[redacted]")
	}
	return text
}
