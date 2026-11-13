// Package mqttmsg 实现当前中控固件的 MQTT 报文、主题和访问判定。
// 这里不连接 Broker，也不保存业务状态。
package mqttmsg

// Point 是协议文档里的一个功能点。中文名必须与固件屏幕上的名字一致。
type Point struct {
	Identifier string
	Name       string
	DataType   string
	Writable   bool
}

const SensorID = "temp_humidity_sens_2"

// Points 是当前固件的全部功能点。标识符和中文名一一对应，最多 6 路开关。
var Points = []Point{
	{Identifier: SensorID, Name: "温湿度", DataType: "string", Writable: false},
	{Identifier: "switch_light", Name: "照明", DataType: "bool", Writable: true},
	{Identifier: "switch_mist", Name: "雾化", DataType: "bool", Writable: true},
	{Identifier: "switch_fan", Name: "通风", DataType: "bool", Writable: true},
	{Identifier: "switch_feed", Name: "喂食", DataType: "bool", Writable: true},
	{Identifier: "switch_heat", Name: "加热垫", DataType: "bool", Writable: true},
	{Identifier: "switch_bask", Name: "晒背灯", DataType: "bool", Writable: true},
}

// IsSwitch 报告这个标识符是不是可下发的开关。
func IsSwitch(identifier string) bool {
	for _, point := range Points {
		if point.Identifier == identifier {
			return point.Writable
		}
	}
	return false
}

// Known 报告这个标识符是否属于当前固件。
func Known(identifier string) bool {
	for _, point := range Points {
		if point.Identifier == identifier {
			return true
		}
	}
	return false
}
