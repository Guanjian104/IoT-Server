package metrics

import (
	"strings"
	"testing"
	"time"
)

func TestAuthSpikeAndTextHasNoDeviceLabel(t *testing.T) {
	m := New("dev", "none", "instance-1")
	now := time.Unix(1000, 0)
	var spiked bool
	for i := 0; i < 5; i++ {
		if m.NoteAuthDeny(now.Add(time.Duration(i) * time.Second)) {
			spiked = true
		}
	}
	if !spiked || m.NoteAuthDeny(now.Add(6*time.Second)) {
		t.Fatal("spike edge")
	}
	m.SetDependency("mysql", true)
	m.NoteKick(true)
	m.NoteKick(false)
	m.SetCommandAttention(3)
	text := m.Text()
	if strings.Contains(text, "device_id") || !strings.Contains(text, "iot_mqtt_auth_deny_total 6") {
		t.Fatal(text)
	}
	if !strings.Contains(text, `dependency="mysql"`) || strings.Contains(text, "94a990cb56a8") {
		t.Fatal(text)
	}
	if !strings.Contains(text, `iot_device_kick_total{result="ok"} 1`) || !strings.Contains(text, `iot_device_kick_total{result="failed"} 1`) || !strings.Contains(text, "iot_command_attention 3") {
		t.Fatal(text)
	}
}
