// Package mqttmsg 实现项圈 1.0pro 的 MQTT 报文、主题和访问判定。
// 这里不连接 Broker，也不保存业务状态。主题和字段以 docs/MQTT_IOT_PROTOCOL.md 的私有服为准。
package mqttmsg

// Point 是登记到设备上的一条命令。中文名与协议里的功能说明一致。
type Point struct {
	Identifier string
	Name       string
	DataType   string
	Writable   bool
}

// Points 是私有服当前会下发的全部命令。
var Points = []Point{
	{Identifier: "find_light", Name: "寻宠闪灯", DataType: "command", Writable: true},
	{Identifier: "buzzer", Name: "寻宠响铃", DataType: "command", Writable: true},
	{Identifier: "gps_oneshot", Name: "立即定位", DataType: "command", Writable: true},
	{Identifier: "find_mode", Name: "寻宠模式", DataType: "command", Writable: true},
	{Identifier: "trip_track", Name: "出行轨迹", DataType: "command", Writable: true},
	{Identifier: "activate", Name: "激活", DataType: "command", Writable: true},
	{Identifier: "mic_record", Name: "录音", DataType: "command", Writable: true},
	{Identifier: "speaker_play", Name: "播放预设音", DataType: "command", Writable: true},
	{Identifier: "voice_call", Name: "语音通话", DataType: "command", Writable: true},
}

// KnownCommand 报告这个名字是不是协议里的 cmd。
func KnownCommand(cmd string) bool {
	for _, point := range Points {
		if point.Identifier == cmd {
			return true
		}
	}
	return false
}
