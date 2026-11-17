package mqttmsg

import (
	"testing"
)

func TestPointsMatchProtocol(t *testing.T) {
	if len(Points) != 7 {
		t.Fatalf("points = %d", len(Points))
	}
	want := []string{"温湿度", "照明", "雾化", "通风", "喂食", "加热垫", "晒背灯"}
	switches := 0
	for i, point := range Points {
		if point.Name != want[i] {
			t.Fatalf("name %d = %s", i, point.Name)
		}
		if point.Writable {
			switches++
		}
	}
	if switches != 6 || IsSwitch(SensorID) || !IsSwitch("switch_bask") {
		t.Fatal("switch set mismatch")
	}
}

func TestParseReportAndRejectsMissingAsUnset(t *testing.T) {
	payload := []byte(`{
	  "deviceId":"94a990cb56a8",
	  "msgId":"s3-12",
	  "time":1762070400000,
	  "data":{
	    "temp_humidity_sens_2":{"value":"H:23.7C,49%;C:23.8C,48%","time":1762070400000},
	    "switch_light":{"value":true,"time":1762070400000},
	    "switch_mist":{"value":false,"time":1762070400000}
	  }
	}`)
	report, err := ParseReport("device/94a990cb56a8/property/report", payload)
	if err != nil {
		t.Fatal(err)
	}
	if report.MsgID != "s3-12" || len(report.Fields) != 3 {
		t.Fatalf("report = %+v", report)
	}
	seen := map[string]bool{}
	for _, field := range report.Fields {
		seen[field.Identifier] = true
		if field.Identifier == "switch_fan" {
			t.Fatal("missing switch was materialized")
		}
	}
	if seen["switch_fan"] {
		t.Fatal("missing field present")
	}
	if _, err := ParseReport("device/94a990cb56a8/property/report", []byte(`{"deviceId":"94a990cb56a8","msgId":"s3-1","time":1,"data":{"switch_light":{"value":"true","time":1}}}`)); err == nil {
		t.Fatal("quoted bool accepted")
	}
	if _, err := ParseReport("device/ffffffffffffffff/property/report", payload); err == nil {
		t.Fatal("device mismatch accepted")
	}
}

func TestCanonicalSetRejectsSensor(t *testing.T) {
	payload, err := CanonicalSet("srv-1", 1762070400000, "switch_light", false)
	if err != nil {
		t.Fatal(err)
	}
	if string(payload) != `{"msgId":"srv-1","time":1762070400000,"data":{"switch_light":false}}` {
		t.Fatalf("payload = %s", payload)
	}
	if _, err := CanonicalSet("srv-1", 1, SensorID, false); err == nil {
		t.Fatal("sensor write accepted")
	}
}

func TestReplyDoesNotReopenTerminalState(t *testing.T) {
	next, ok := ReplyState("sent", 0)
	if !ok || next != "accepted" {
		t.Fatalf("accepted = %s %v", next, ok)
	}
	if _, ok := ReplyState("timeout", 0); ok {
		t.Fatal("late reply reopened timeout")
	}
	if next, ok := ReplyState("sending", 1002); !ok || next != "failed" {
		t.Fatalf("failed = %s %v", next, ok)
	}
	if next, ok := ReplyState("uncertain", 0); !ok || next != "accepted" {
		t.Fatalf("uncertain accepted = %s %v", next, ok)
	}
	if next, ok := ReplyState("uncertain", 1002); !ok || next != "failed" {
		t.Fatalf("uncertain failed = %s %v", next, ok)
	}
	if _, ok := ReplyState("needs_attention", 0); ok {
		t.Fatal("attention reopened")
	}
	if _, ok := ReplyState("cancelled", 0); ok {
		t.Fatal("cancelled reopened")
	}
}

func TestAccessMatrix(t *testing.T) {
	const id = "94a990cb56a8"
	cases := []struct {
		kind, device, action, topic, want string
	}{
		{"device", id, "publish", "device/94a990cb56a8/property/report", "allow"},
		{"device", id, "publish", "device/94a990cb56a8/property/set_reply", "allow"},
		{"device", id, "publish", "device/94a990cb56a8/status", "allow"},
		{"device", id, "subscribe", "device/94a990cb56a8/property/set", "allow"},
		{"device", id, "publish", "device/94a990cb56a8/property/set", "deny"},
		{"device", id, "publish", "device/000000000000/property/report", "deny"},
		{"device", id, "subscribe", "#", "deny"},
		{"device", id, "subscribe", "device/94a990cb56a8/#", "deny"},
		{"device", id, "subscribe", "$SYS/brokers/+/clients/+/connected", "deny"},
		{"service", "", "publish", "device/94a990cb56a8/property/set", "allow"},
		{"service", "", "publish", "device/94a990cb56a8/property/report", "deny"},
		{"service", "", "subscribe", "$share/iot-dev/device/+/property/report", "allow"},
		{"service", "", "subscribe", "$SYS/brokers/+/clients/+/disconnected", "allow"},
		{"service", "", "subscribe", "#", "deny"},
		{"app", "", "subscribe", "device/94a990cb56a8/property/set", "deny"},
		{"unknown", "", "publish", "device/94a990cb56a8/status", "deny"},
	}
	for _, tc := range cases {
		if got := Decide(tc.kind, tc.device, tc.action, tc.topic); got != tc.want {
			t.Fatalf("%+v got %s", tc, got)
		}
	}
}

func TestClassifyKeepsAppAwayFromRoot(t *testing.T) {
	kind, id := Classify("root", "94a990cb56a8")
	if kind != "device" || id != "94a990cb56a8" {
		t.Fatalf("device = %s %s", kind, id)
	}
	kind, _ = Classify("iot-dev-service", "iot-dev-service-a")
	if kind != "service" {
		t.Fatal(kind)
	}
	kind, _ = Classify("app:user", "app-user-1")
	if kind != "app" {
		t.Fatal(kind)
	}
	kind, _ = Classify("root", "iot-dev-service-a")
	if kind != "unknown" {
		t.Fatal(kind)
	}
	if SameSecret("alpha", "beta") || !SameSecret("alpha", "alpha") || SameSecret("alpha", "") {
		t.Fatal("secret compare")
	}
}
