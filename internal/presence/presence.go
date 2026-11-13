// Package presence 根据 Broker 会话事件维护在线代数。
// 设备自己发布的 status 文本不在这里合并，调用方单独保存。
package presence

// Snapshot 是某一设备当前的 MQTT 会话观察结果。
type Snapshot struct {
	Online         bool
	Generation     int64
	ConnectedAt    int64
	DisconnectedAt int64
}

// Event 是一次连接或断开。时间戳沿用 Broker 报文里的毫秒，不把它解释成日历。
type Event struct {
	Kind         string
	ConnectedAt  int64
	EventAt      int64
	Disconnected int64
}

const (
	Connected    = "connected"
	Disconnected = "disconnected"
)

// Apply 把一次会话事件叠到当前快照上。
// 返回的布尔值表示快照是否发生变化。更早的断线不能清掉更新的连接。
func Apply(current Snapshot, event Event) (Snapshot, bool) {
	switch event.Kind {
	case Connected:
		if event.ConnectedAt <= 0 || event.ConnectedAt < current.Generation {
			return current, false
		}
		next := current
		next.Online = true
		next.Generation = event.ConnectedAt
		next.ConnectedAt = event.ConnectedAt
		return next, true
	case Disconnected:
		if event.ConnectedAt > 0 && event.ConnectedAt != current.Generation {
			return current, false
		}
		if event.ConnectedAt == 0 && event.EventAt < current.Generation {
			return current, false
		}
		if !current.Online && current.Generation == 0 {
			return current, false
		}
		next := current
		next.Online = false
		switch {
		case event.Disconnected > 0:
			next.DisconnectedAt = event.Disconnected
		case event.EventAt > 0:
			next.DisconnectedAt = event.EventAt
		}
		return next, true
	default:
		return current, false
	}
}
