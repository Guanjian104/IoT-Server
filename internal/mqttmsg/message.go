package mqttmsg

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

var (
	deviceIDPattern = regexp.MustCompile(`^[0-9a-f]{12}$`)
	msgIDPattern    = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)
)

// ValidDeviceID 判断设备 ID 是否为 12 位小写十六进制。
func ValidDeviceID(id string) bool {
	return deviceIDPattern.MatchString(id)
}

// Hash 返回报文的十六进制 SHA-256，用于和 msgId 一起去重。
func Hash(payload []byte) string {
	sum := sha256.Sum256(payload)
	return fmt.Sprintf("%x", sum[:])
}

// Field 是上报 data 里的一个功能点。Time 是报文里的毫秒数，不是解析后的日历时间。
type Field struct {
	Identifier string
	Bool       *bool
	Text       *string
	Time       int64
}

// Report 是 property/report 的业务内容。
type Report struct {
	DeviceID string
	MsgID    string
	Time     int64
	Fields   []Field
}

// Reply 是 property/set_reply。
type Reply struct {
	DeviceID string
	MsgID    string
	Time     int64
	Code     int
}

// ParseReport 解析设备上报。未知字段、缺失开关被当成关闭、以及非协议类型都会拒绝。
func ParseReport(topic string, payload []byte) (Report, error) {
	deviceID, kind, err := SplitTopic(topic)
	if err != nil || kind != "report" {
		return Report{}, errors.New("invalid report topic")
	}
	dec := json.NewDecoder(bytes.NewReader(payload))
	dec.DisallowUnknownFields()
	var doc struct {
		DeviceID string                     `json:"deviceId"`
		MsgID    string                     `json:"msgId"`
		Time     *int64                     `json:"time"`
		Data     map[string]json.RawMessage `json:"data"`
	}
	if err := dec.Decode(&doc); err != nil {
		return Report{}, errors.New("invalid report")
	}
	if dec.More() {
		return Report{}, errors.New("invalid report")
	}
	if doc.DeviceID != deviceID || !msgIDPattern.MatchString(doc.MsgID) || doc.Time == nil || *doc.Time < 0 {
		return Report{}, errors.New("invalid report")
	}
	if doc.Data == nil {
		return Report{}, errors.New("invalid report")
	}
	report := Report{DeviceID: deviceID, MsgID: doc.MsgID, Time: *doc.Time}
	for _, point := range Points {
		raw, ok := doc.Data[point.Identifier]
		if !ok {
			continue
		}
		field, err := parseField(point, raw, report.Time)
		if err != nil {
			return Report{}, err
		}
		report.Fields = append(report.Fields, field)
		delete(doc.Data, point.Identifier)
	}
	if len(doc.Data) > 0 {
		return Report{}, errors.New("invalid report")
	}
	return report, nil
}

func parseField(point Point, raw json.RawMessage, fallback int64) (Field, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	var doc struct {
		Value json.RawMessage `json:"value"`
		Time  *int64          `json:"time"`
	}
	if err := dec.Decode(&doc); err != nil || len(doc.Value) == 0 {
		return Field{}, errors.New("invalid report")
	}
	when := fallback
	if doc.Time != nil {
		if *doc.Time < 0 {
			return Field{}, errors.New("invalid report")
		}
		when = *doc.Time
	}
	field := Field{Identifier: point.Identifier, Time: when}
	if point.Writable {
		var value bool
		if err := json.Unmarshal(doc.Value, &value); err != nil || !jsonBool(doc.Value) {
			return Field{}, errors.New("invalid report")
		}
		field.Bool = &value
		return field, nil
	}
	var text string
	if err := json.Unmarshal(doc.Value, &text); err != nil || !validSensor(text) {
		return Field{}, errors.New("invalid report")
	}
	field.Text = &text
	return field, nil
}

func jsonBool(raw json.RawMessage) bool {
	text := strings.TrimSpace(string(raw))
	return text == "true" || text == "false"
}

func validSensor(text string) bool {
	hot, cold, ok := strings.Cut(text, ";")
	if !ok || !validZone(hot, "H:") || !validZone(cold, "C:") {
		return false
	}
	return true
}

func validZone(zone, prefix string) bool {
	body, ok := strings.CutPrefix(zone, prefix)
	if !ok {
		return false
	}
	if body == "--" {
		return true
	}
	temp, humidity, ok := strings.Cut(body, ",")
	if !ok || !strings.HasSuffix(temp, "C") || !strings.HasSuffix(humidity, "%") {
		return false
	}
	if _, err := strconv.ParseFloat(strings.TrimSuffix(temp, "C"), 64); err != nil {
		return false
	}
	if _, err := strconv.Atoi(strings.TrimSuffix(humidity, "%")); err != nil {
		return false
	}
	return true
}

// ParseReply 解析下发应答。
func ParseReply(topic string, payload []byte) (Reply, error) {
	deviceID, kind, err := SplitTopic(topic)
	if err != nil || kind != "reply" {
		return Reply{}, errors.New("invalid reply topic")
	}
	dec := json.NewDecoder(bytes.NewReader(payload))
	dec.DisallowUnknownFields()
	var doc struct {
		DeviceID string `json:"deviceId"`
		MsgID    string `json:"msgId"`
		Time     *int64 `json:"time"`
		Code     *int   `json:"code"`
	}
	if err := dec.Decode(&doc); err != nil || dec.More() {
		return Reply{}, errors.New("invalid reply")
	}
	if doc.DeviceID != deviceID || !msgIDPattern.MatchString(doc.MsgID) || doc.Time == nil || *doc.Time < 0 || doc.Code == nil {
		return Reply{}, errors.New("invalid reply")
	}
	return Reply{DeviceID: deviceID, MsgID: doc.MsgID, Time: *doc.Time, Code: *doc.Code}, nil
}

// ParseStatus 解析保留状态文本。
func ParseStatus(topic string, payload []byte) (string, error) {
	deviceID, kind, err := SplitTopic(topic)
	if err != nil || kind != "status" || !ValidDeviceID(deviceID) {
		return "", errors.New("invalid status topic")
	}
	switch string(payload) {
	case "online", "offline":
		return string(payload), nil
	default:
		return "", errors.New("invalid status")
	}
}

// CanonicalSet 生成唯一的下发 JSON。温湿度和空开关集合会被拒绝。
func CanonicalSet(msgID string, now int64, identifier string, value bool) ([]byte, error) {
	if !msgIDPattern.MatchString(msgID) || now < 0 || !IsSwitch(identifier) {
		return nil, errors.New("invalid command")
	}
	payload, err := json.Marshal(struct {
		MsgID string          `json:"msgId"`
		Time  int64           `json:"time"`
		Data  map[string]bool `json:"data"`
	}{
		MsgID: msgID,
		Time:  now,
		Data:  map[string]bool{identifier: value},
	})
	if err != nil {
		return nil, errors.New("invalid command")
	}
	return payload, nil
}

// ReplyState 把应答码映射到命令终态。sending、sent 和 uncertain 可以收敛。
// uncertain 收到回执说明命令已经到达设备，按回执收成成功或失败，不再保持不确定。
func ReplyState(current string, code int) (string, bool) {
	if current != "sending" && current != "sent" && current != "uncertain" {
		return current, false
	}
	if code == 0 {
		return "accepted", true
	}
	return "failed", true
}
