package mqttmsg

import "testing"

// 人翻宠：eco-iot 向具体项圈 audio 发 preset 分片，设备订自己的 audio、发自己的录音分片。
func TestAudioTopicAccess(t *testing.T) {
	const mac = "9030d6cb2703"
	audio := "pet/collar/1_0pro/" + mac + "/audio"
	cases := []struct {
		kind, device, model, action, topic, want string
	}{
		{"eco-iot", "", "", "publish", audio, "allow"},
		{"eco-iot", "", "", "publish", "pet/collar/1_0pro/+/audio", "deny"},
		{"eco-iot", "", "", "subscribe", "pet/collar/1_0pro/+/audio", "allow"},
		{"device", mac, Model10Pro, "subscribe", audio, "allow"},
		{"device", mac, Model10Pro, "publish", audio, "allow"},
		{"device", mac, Model10Pro, "subscribe", "pet/collar/1_0pro/9030d6cb0000/audio", "deny"},
		{"device", mac, Model10Pro, "subscribe", "pet/collar/2_0_test/" + mac + "/audio", "deny"},
		{"service", "", "", "publish", audio, "deny"},
		{"device", mac, ModelWatch, "publish", audio, "deny"},
	}
	for _, c := range cases {
		if got := Decide(c.kind, c.device, c.model, c.action, c.topic); got != c.want {
			t.Errorf("%s %s %s = %s, want %s", c.kind, c.action, c.topic, got, c.want)
		}
	}
	if Topic(Model10Pro, mac, "audio") != audio {
		t.Fatalf("topic %q", Topic(Model10Pro, mac, "audio"))
	}
}
