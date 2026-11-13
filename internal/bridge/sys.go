package bridge

import (
	"encoding/json"
	"regexp"

	"iot-server/internal/mqttmsg"
	"iot-server/internal/presence"
)

var sysTopic = regexp.MustCompile(`^\$SYS/brokers/[^/]+/clients/([^/]+)/(connected|disconnected)$`)

// ParseSys 从 Broker 会话事件取出设备 ID 和代数时间。服务账号的事件会被忽略。
func ParseSys(topic string, payload []byte) (string, presence.Event, bool) {
	match := sysTopic.FindStringSubmatch(topic)
	if match == nil {
		return "", presence.Event{}, false
	}
	var doc struct {
		Username       string `json:"username"`
		ClientID       string `json:"clientid"`
		ConnectedAt    int64  `json:"connected_at"`
		DisconnectedAt int64  `json:"disconnected_at"`
		TS             int64  `json:"ts"`
	}
	if err := json.Unmarshal(payload, &doc); err != nil {
		return "", presence.Event{}, false
	}
	deviceID := doc.ClientID
	if deviceID == "" {
		deviceID = match[1]
	}
	if !mqttmsg.ValidDeviceID(deviceID) {
		return "", presence.Event{}, false
	}
	if doc.Username != "" && doc.Username != mqttmsg.DeviceUsername {
		return "", presence.Event{}, false
	}
	event := presence.Event{
		ConnectedAt:  millis(doc.ConnectedAt),
		EventAt:      millis(doc.TS),
		Disconnected: millis(doc.DisconnectedAt),
	}
	if match[2] == "disconnected" {
		event.Kind = presence.Disconnected
	} else {
		event.Kind = presence.Connected
		if event.ConnectedAt == 0 {
			event.ConnectedAt = event.EventAt
		}
	}
	return deviceID, event, true
}

func millis(value int64) int64 {
	if value > 0 && value < 1_000_000_000_000 {
		return value * 1000
	}
	return value
}
