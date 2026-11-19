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

var deviceIDPattern = regexp.MustCompile(`^[0-9a-f]{12}$`)

// ValidDeviceID 判断设备 ID 是否为 12 位小写十六进制。
func ValidDeviceID(id string) bool {
	return deviceIDPattern.MatchString(id)
}

// Hash 返回报文的十六进制 SHA-256。没有 msgId 时用它和设备一起去重。
func Hash(payload []byte) string {
	sum := sha256.Sum256(payload)
	return fmt.Sprintf("%x", sum[:])
}

// Field 是一条要写入当前值的上行内容。Time 为毫秒；报文没有 ts 时为 0。
type Field struct {
	Identifier string
	Value      json.RawMessage
	Time       int64
}

// Report 是 location、mode_state 或 DP 展开后的字段。
type Report struct {
	DeviceID string
	MsgID    string
	Time     int64
	Fields   []Field
}

// CmdResult 是 state 主题上的指令结果。
type CmdResult struct {
	Cmd    string
	Result string
	Code   int
	Time   int64
}

// State 是私有服 state 主题上的一种上行。
type State struct {
	DeviceID string
	Kind     string
	Status   string
	Reply    CmdResult
	Report   Report
}

var (
	errMessage = errors.New("invalid message")
	errCommand = errors.New("invalid command")
)

// ParseState 解析设备发到 state 主题的报文。
// 在线是纯文本 online 或 offline。其余按 type 区分；没有 type、只有 data 的对象是 DP。
func ParseState(topic string, payload []byte) (State, error) {
	deviceID, _, kind, err := SplitTopic(topic)
	if err != nil || kind != "state" {
		return State{}, errMessage
	}
	text := strings.TrimSpace(string(payload))
	if text == "online" || text == "offline" {
		return State{DeviceID: deviceID, Kind: "status", Status: text}, nil
	}
	var probe struct {
		Type string          `json:"type"`
		Data json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(payload, &probe); err != nil {
		return State{}, errMessage
	}
	switch probe.Type {
	case "cmd_result":
		reply, err := parseCmdResult(payload)
		if err != nil {
			return State{}, err
		}
		return State{DeviceID: deviceID, Kind: "cmd_result", Reply: reply}, nil
	case "location":
		report, err := parseLocation(deviceID, payload)
		if err != nil {
			return State{}, err
		}
		return State{DeviceID: deviceID, Kind: "report", Report: report}, nil
	case "mode_state":
		report, err := parseModeState(deviceID, payload)
		if err != nil {
			return State{}, err
		}
		return State{DeviceID: deviceID, Kind: "report", Report: report}, nil
	case "":
		if len(probe.Data) == 0 {
			return State{}, errMessage
		}
		report, err := parseDP(deviceID, payload)
		if err != nil {
			return State{}, err
		}
		return State{DeviceID: deviceID, Kind: "report", Report: report}, nil
	default:
		return State{}, errMessage
	}
}

func parseCmdResult(payload []byte) (CmdResult, error) {
	var doc struct {
		TS     *int64 `json:"ts"`
		Type   string `json:"type"`
		Cmd    string `json:"cmd"`
		Result string `json:"result"`
		Code   *int   `json:"code"`
	}
	if err := decodeExact(payload, &doc); err != nil || doc.Type != "cmd_result" || doc.TS == nil || *doc.TS < 0 || doc.Code == nil {
		return CmdResult{}, errMessage
	}
	if !KnownCommand(doc.Cmd) || !knownResult(doc.Result) {
		return CmdResult{}, errMessage
	}
	return CmdResult{Cmd: doc.Cmd, Result: doc.Result, Code: *doc.Code, Time: *doc.TS}, nil
}

func knownResult(result string) bool {
	switch result {
	case "accepted", "success", "failed", "ended", "busy":
		return true
	default:
		return false
	}
}

func parseLocation(deviceID string, payload []byte) (Report, error) {
	var doc struct {
		TS     *int64   `json:"ts"`
		Type   string   `json:"type"`
		Reason string   `json:"reason"`
		Lat    *float64 `json:"lat"`
		Lon    *float64 `json:"lon"`
		Sats   *int     `json:"sats"`
		Valid  *bool    `json:"valid"`
	}
	if err := decodeExact(payload, &doc); err != nil || doc.Type != "location" || doc.TS == nil || *doc.TS < 0 || doc.Lat == nil || doc.Lon == nil || doc.Sats == nil || *doc.Sats < 0 || doc.Valid == nil {
		return Report{}, errMessage
	}
	switch doc.Reason {
	case "oneshot", "find", "trip":
	default:
		return Report{}, errMessage
	}
	raw, err := json.Marshal(struct {
		Reason string  `json:"reason"`
		Lat    float64 `json:"lat"`
		Lon    float64 `json:"lon"`
		Sats   int     `json:"sats"`
		Valid  bool    `json:"valid"`
	}{doc.Reason, *doc.Lat, *doc.Lon, *doc.Sats, *doc.Valid})
	if err != nil {
		return Report{}, errMessage
	}
	return Report{
		DeviceID: deviceID,
		Time:     unixMillis(*doc.TS),
		Fields:   []Field{{Identifier: "location", Value: raw, Time: unixMillis(*doc.TS)}},
	}, nil
}

func parseModeState(deviceID string, payload []byte) (Report, error) {
	var doc struct {
		Type         string `json:"type"`
		FindMode     *bool  `json:"find_mode"`
		TripTrack    *bool  `json:"trip_track"`
		FindRemain   *int   `json:"find_remain_s"`
		TripInterval *int   `json:"trip_interval_s"`
	}
	if err := decodeExact(payload, &doc); err != nil || doc.Type != "mode_state" || doc.FindMode == nil || doc.TripTrack == nil || doc.FindRemain == nil || *doc.FindRemain < 0 || doc.TripInterval == nil || *doc.TripInterval < 0 {
		return Report{}, errMessage
	}
	raw, err := json.Marshal(struct {
		FindMode     bool `json:"find_mode"`
		TripTrack    bool `json:"trip_track"`
		FindRemain   int  `json:"find_remain_s"`
		TripInterval int  `json:"trip_interval_s"`
	}{*doc.FindMode, *doc.TripTrack, *doc.FindRemain, *doc.TripInterval})
	if err != nil {
		return Report{}, errMessage
	}
	return Report{DeviceID: deviceID, Fields: []Field{{Identifier: "mode_state", Value: raw}}}, nil
}

func parseDP(deviceID string, payload []byte) (Report, error) {
	var doc struct {
		Data map[string]json.RawMessage `json:"data"`
	}
	if err := decodeExact(payload, &doc); err != nil || len(doc.Data) == 0 {
		return Report{}, errMessage
	}
	report := Report{DeviceID: deviceID}
	for key, raw := range doc.Data {
		if !dpKey.MatchString(key) || !jsonScalar(raw) {
			return Report{}, errMessage
		}
		report.Fields = append(report.Fields, Field{Identifier: key, Value: append([]byte(nil), raw...)})
	}
	return report, nil
}

var dpKey = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]{0,63}$`)

func jsonScalar(raw json.RawMessage) bool {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var value any
	if err := dec.Decode(&value); err != nil || dec.More() {
		return false
	}
	switch value.(type) {
	case string, bool, json.Number:
		return true
	default:
		return false
	}
}

func unixMillis(ts int64) int64 {
	if ts < 1_000_000_000_000 {
		return ts * 1000
	}
	return ts
}

func decodeExact(payload []byte, dest any) error {
	dec := json.NewDecoder(bytes.NewReader(payload))
	dec.DisallowUnknownFields()
	if err := dec.Decode(dest); err != nil || dec.More() {
		return errMessage
	}
	return nil
}

// CanonicalCommand 生成下发到 cmd 主题的 JSON。一次性命令可以不带 on。
func CanonicalCommand(cmd string, on *bool, raw json.RawMessage) ([]byte, error) {
	if !KnownCommand(cmd) {
		return nil, errCommand
	}
	params, err := paramObject(raw)
	if err != nil {
		return nil, errCommand
	}
	out := map[string]any{}
	switch cmd {
	case "find_light":
		if on == nil {
			return nil, errCommand
		}
		if err := takeU32(params, out, "color", false); err != nil || takeChoice(params, out, "freq_hz", false, 1, 3) != nil || takeRange(params, out, "duration_min", false, 0, 60) != nil {
			return nil, errCommand
		}
	case "buzzer":
		if on == nil {
			return nil, errCommand
		}
		if takeChoice(params, out, "tone_hz", false, 2000, 3000) != nil || takeRange(params, out, "count", false, 1, 60) != nil {
			return nil, errCommand
		}
	case "gps_oneshot", "activate":
		if on != nil && !*on {
			return nil, errCommand
		}
	case "find_mode":
		if on == nil || takeRange(params, out, "duration_min", false, 1, 300) != nil || takeRange(params, out, "interval_s", false, 2, 30) != nil {
			return nil, errCommand
		}
	case "trip_track":
		if on == nil || takeRange(params, out, "interval_min", false, 1, 60) != nil {
			return nil, errCommand
		}
	case "mic_record":
		if on == nil || takeRange(params, out, "duration_s", false, 1, 60) != nil {
			return nil, errCommand
		}
	case "speaker_play":
		if on == nil || takeString(params, out, "file", false) != nil {
			return nil, errCommand
		}
	case "voice_call":
		if on == nil {
			return nil, errCommand
		}
		required := on != nil && *on
		if takeString(params, out, "channel", required) != nil || takeRange(params, out, "uid", required, 0, 1<<31-1) != nil || takeString(params, out, "token", required) != nil || takeRange(params, out, "duration_min", false, 1, 60) != nil {
			return nil, errCommand
		}
	default:
		return nil, errCommand
	}
	if len(params) > 0 {
		return nil, errCommand
	}
	payload, err := json.Marshal(downlink{Cmd: cmd, On: on, Params: out})
	if err != nil || len(out) == 0 && bytes.Contains(payload, []byte(`"params"`)) {
		return nil, errCommand
	}
	return payload, nil
}

type downlink struct {
	Cmd    string         `json:"cmd"`
	On     *bool          `json:"on,omitempty"`
	Params map[string]any `json:"params,omitempty"`
}

func paramObject(raw json.RawMessage) (map[string]json.RawMessage, error) {
	if len(bytes.TrimSpace(raw)) == 0 || string(bytes.TrimSpace(raw)) == "null" {
		return map[string]json.RawMessage{}, nil
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	var doc map[string]json.RawMessage
	if err := dec.Decode(&doc); err != nil || dec.More() || doc == nil {
		return nil, errCommand
	}
	return doc, nil
}

func takeU32(params map[string]json.RawMessage, out map[string]any, key string, required bool) error {
	return takeRange(params, out, key, required, 0, 4294967295)
}

func takeChoice(params map[string]json.RawMessage, out map[string]any, key string, required bool, choices ...int64) error {
	value, ok, err := takeInt(params, key, required)
	if err != nil || !ok {
		return err
	}
	for _, choice := range choices {
		if value == choice {
			out[key] = value
			return nil
		}
	}
	return errCommand
}

func takeRange(params map[string]json.RawMessage, out map[string]any, key string, required bool, min, max int64) error {
	value, ok, err := takeInt(params, key, required)
	if err != nil || !ok {
		return err
	}
	if value < min || value > max {
		return errCommand
	}
	out[key] = value
	return nil
}

func takeInt(params map[string]json.RawMessage, key string, required bool) (int64, bool, error) {
	raw, ok := params[key]
	if !ok {
		if required {
			return 0, false, errCommand
		}
		return 0, false, nil
	}
	delete(params, key)
	text := strings.TrimSpace(string(raw))
	if text == "" || strings.ContainsAny(text, ".eE+") {
		return 0, false, errCommand
	}
	value, err := strconv.ParseInt(text, 10, 64)
	if err != nil {
		return 0, false, errCommand
	}
	return value, true, nil
}

func takeString(params map[string]json.RawMessage, out map[string]any, key string, required bool) error {
	raw, ok := params[key]
	if !ok {
		if required {
			return errCommand
		}
		return nil
	}
	delete(params, key)
	var text string
	if err := json.Unmarshal(raw, &text); err != nil || text == "" || len(text) > 128 {
		return errCommand
	}
	out[key] = text
	return nil
}

// CommandName 读出已生成载荷里的 cmd。
func CommandName(payload []byte) string {
	var doc struct {
		Cmd string `json:"cmd"`
	}
	if json.Unmarshal(payload, &doc) != nil || !KnownCommand(doc.Cmd) {
		return ""
	}
	return doc.Cmd
}

// ReplyState 把 cmd_result 映射到命令终态。sending、sent 和 uncertain 可以收敛。
// accepted、success 和 ended 都表示这条下发已经有了设备侧的肯定结果。
func ReplyState(current, result string) (string, bool) {
	if current != "sending" && current != "sent" && current != "uncertain" {
		return current, false
	}
	switch result {
	case "accepted", "success", "ended":
		return "accepted", true
	case "failed", "busy":
		return "failed", true
	default:
		return current, false
	}
}
