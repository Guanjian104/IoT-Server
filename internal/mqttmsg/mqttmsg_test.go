package mqttmsg

import "testing"

func TestPointsMatchProtocol(t *testing.T) {
	want := []string{"find_light", "buzzer", "gps_oneshot", "find_mode", "trip_track", "activate", "mic_record", "speaker_play", "voice_call"}
	if len(Points) != len(want) {
		t.Fatalf("points = %d", len(Points))
	}
	for i, point := range Points {
		if point.Identifier != want[i] || !point.Writable || !KnownCommand(point.Identifier) {
			t.Fatalf("point %d = %+v", i, point)
		}
	}
	if KnownCommand("switch_light") {
		t.Fatal("old switch accepted")
	}
}

func TestCanonicalCommandsMatchProtocol(t *testing.T) {
	on := true
	off := false
	light, err := CanonicalCommand("find_light", &on, []byte(`{"color":16711680,"freq_hz":3,"duration_min":1}`))
	if err != nil || string(light) != `{"cmd":"find_light","on":true,"params":{"color":16711680,"duration_min":1,"freq_hz":3}}` {
		t.Fatalf("find_light = %s %v", light, err)
	}
	trip, err := CanonicalCommand("trip_track", &on, []byte(`{"interval_min":5}`))
	if err != nil || string(trip) != `{"cmd":"trip_track","on":true,"params":{"interval_min":5}}` {
		t.Fatalf("trip = %s %v", trip, err)
	}
	stop, err := CanonicalCommand("trip_track", &off, nil)
	if err != nil || string(stop) != `{"cmd":"trip_track","on":false}` {
		t.Fatalf("stop = %s %v", stop, err)
	}
	gps, err := CanonicalCommand("gps_oneshot", nil, nil)
	if err != nil || string(gps) != `{"cmd":"gps_oneshot"}` {
		t.Fatalf("gps = %s %v", gps, err)
	}
	call, err := CanonicalCommand("voice_call", &on, []byte(`{"channel":"936193","uid":1001,"token":"...","duration_min":15}`))
	if err != nil || string(call) != `{"cmd":"voice_call","on":true,"params":{"channel":"936193","duration_min":15,"token":"...","uid":1001}}` {
		t.Fatalf("call = %s %v", call, err)
	}
	if _, err := CanonicalCommand("find_light", &on, []byte(`{"freq_hz":2}`)); err == nil {
		t.Fatal("bad freq accepted")
	}
	if _, err := CanonicalCommand("voice_call", &on, []byte(`{"channel":"936193"}`)); err == nil {
		t.Fatal("call without token accepted")
	}
	if _, err := CanonicalCommand("gps_oneshot", &off, nil); err == nil {
		t.Fatal("oneshot off accepted")
	}
}

func TestParseStateMessages(t *testing.T) {
	const id = "9030d6cb176c"
	topic := DeviceTopic(id, "state")
	reply, err := ParseState(topic, []byte(`{"ts":1760580001,"type":"cmd_result","cmd":"trip_track","result":"accepted","code":0}`))
	if err != nil || reply.Kind != "cmd_result" || reply.Reply.Cmd != "trip_track" || reply.Reply.Result != "accepted" || reply.Reply.Code != 0 {
		t.Fatalf("reply = %+v %v", reply, err)
	}
	location, err := ParseState(topic, []byte(`{"ts":1760580002,"type":"location","reason":"oneshot","lat":22.54,"lon":113.94,"sats":8,"valid":true}`))
	if err != nil || location.Kind != "report" || len(location.Report.Fields) != 1 || location.Report.Fields[0].Time != 1760580002000 {
		t.Fatalf("location = %+v %v", location, err)
	}
	mode, err := ParseState(topic, []byte(`{"type":"mode_state","find_mode":true,"trip_track":false,"find_remain_s":280,"trip_interval_s":300}`))
	if err != nil || mode.Report.Fields[0].Identifier != "mode_state" {
		t.Fatalf("mode = %+v %v", mode, err)
	}
	dp, err := ParseState(topic, []byte(`{"data":{"pet_action":"static"}}`))
	if err != nil || len(dp.Report.Fields) != 1 || dp.Report.Fields[0].Identifier != "pet_action" || string(dp.Report.Fields[0].Value) != `"static"` {
		t.Fatalf("dp = %+v %v", dp, err)
	}
	status, err := ParseState(topic, []byte("online"))
	if err != nil || status.Kind != "status" || status.Status != "online" {
		t.Fatalf("status = %+v %v", status, err)
	}
	if _, err := ParseState(topic, []byte(`{"type":"location","reason":"lost","lat":1,"lon":2,"sats":0,"valid":false,"ts":1}`)); err == nil {
		t.Fatal("bad reason accepted")
	}
	if _, err := ParseState(DeviceTopic(id, "cmd"), []byte("online")); err == nil {
		t.Fatal("cmd topic parsed as state")
	}
}

func TestReplyStateUsesResult(t *testing.T) {
	next, ok := ReplyState("sent", "success")
	if !ok || next != "accepted" {
		t.Fatalf("success = %s %v", next, ok)
	}
	if _, ok := ReplyState("timeout", "success"); ok {
		t.Fatal("late reply reopened timeout")
	}
	if next, ok := ReplyState("sending", "busy"); !ok || next != "failed" {
		t.Fatalf("busy = %s %v", next, ok)
	}
	if next, ok := ReplyState("uncertain", "ended"); !ok || next != "accepted" {
		t.Fatalf("ended = %s %v", next, ok)
	}
	if _, ok := ReplyState("needs_attention", "accepted"); ok {
		t.Fatal("attention reopened")
	}
	if _, ok := ReplyState("cancelled", "failed"); ok {
		t.Fatal("cancelled reopened")
	}
}

func TestAccessMatrix(t *testing.T) {
	const id = "9030d6cb176c"
	cases := []struct {
		kind, device, model, action, topic, want string
	}{
		{"device", id, Model10Pro, "publish", "pet/collar/1_0pro/9030d6cb176c/state", "allow"},
		{"device", id, Model10Pro, "subscribe", "pet/collar/1_0pro/9030d6cb176c/cmd", "allow"},
		{"device", id, Model10Pro, "publish", "pet/collar/1_0pro/9030d6cb176c/cmd", "deny"},
		{"device", id, Model10Pro, "publish", "pet/collar/1_0pro/000000000000/state", "deny"},
		{"device", id, Model10Pro, "subscribe", "pet/collar/2_0_test/9030d6cb176c/cmd", "deny"},
		{"device", id, Model20Test, "subscribe", "pet/collar/2_0_test/9030d6cb176c/cmd", "allow"},
		{"device", id, Model20Test, "publish", "pet/collar/2_0_test/9030d6cb176c/state", "allow"},
		{"device", id, Model20Test, "publish", "pet/collar/1_0pro/9030d6cb176c/state", "deny"},
		{"device", id, Model20Test, "subscribe", "pet/collar/#", "deny"},
		{"device", id, ModelWatch, "subscribe", "pet/collar/2_0_test/+/state", "allow"},
		{"device", id, ModelWatch, "subscribe", "pet/collar/1_0pro/" + id + "/state", "allow"},
		{"device", id, ModelWatch, "subscribe", "pet/collar/2_0_test/" + id + "/cmd", "deny"},
		{"device", id, ModelWatch, "subscribe", "pet/collar/#", "deny"},
		{"device", id, ModelWatch, "publish", "pet/collar/1_0pro/" + id + "/state", "deny"},
		{"device", id, Model10Pro, "subscribe", "#", "deny"},
		{"device", id, Model10Pro, "subscribe", "pet/collar/1_0pro/9030d6cb176c/#", "deny"},
		{"device", id, Model10Pro, "subscribe", "$SYS/brokers/+/clients/+/connected", "deny"},
		{"service", "", "", "publish", "pet/collar/1_0pro/9030d6cb176c/cmd", "allow"},
		{"service", "", "", "publish", "pet/collar/2_0_test/9030d6cb176c/cmd", "allow"},
		{"service", "", "", "publish", "pet/collar/1_0pro/9030d6cb176c/state", "deny"},
		{"service", "", "", "subscribe", "$share/iot-dev/pet/collar/1_0pro/+/state", "allow"},
		{"service", "", "", "subscribe", "$share/iot-dev/pet/collar/2_0_test/+/state", "allow"},
		{"service", "", "", "subscribe", "$SYS/brokers/+/clients/+/disconnected", "allow"},
		{"service", "", "", "subscribe", "#", "deny"},
		{"app", "", "", "subscribe", "pet/collar/1_0pro/9030d6cb176c/cmd", "deny"},
		{"unknown", "", "", "publish", "pet/collar/1_0pro/9030d6cb176c/state", "deny"},
	}
	for _, tc := range cases {
		if got := Decide(tc.kind, tc.device, tc.model, tc.action, tc.topic); got != tc.want {
			t.Fatalf("%+v got %s", tc, got)
		}
	}
}

func TestClassifyKeepsAppAwayFromRoot(t *testing.T) {
	kind, id := Classify("root", "9030d6cb176c")
	if kind != "device" || id != "9030d6cb176c" {
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
