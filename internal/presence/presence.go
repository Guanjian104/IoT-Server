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
// 同一代连接的重复上线不再改写，也不会在这次会话已经断开后把设备重新标成在线。
// 已经离线时，重复的断开也不再改写。
func Apply(current Snapshot, event Event) (Snapshot, bool) {
	switch event.Kind {
	case Connected:
		if event.ConnectedAt <= 0 || event.ConnectedAt <= current.Generation {
			return current, false
		}
		next := current
		next.Online = true
		next.Generation = event.ConnectedAt
		next.ConnectedAt = event.ConnectedAt
		return next, true
	case Disconnected:
		if !current.Online {
			return current, false
		}
		if event.ConnectedAt > 0 && event.ConnectedAt != current.Generation {
			return current, false
		}
		if event.ConnectedAt == 0 && event.EventAt < current.Generation {
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

// Calibrate 用 Broker 当前还能看见的会话修正在线位。
// 看见连接时，不把代数改到更早的时间；看不见连接时，只把仍标成在线的快照改成离线。
func Calibrate(current Snapshot, connected bool, connectedAt int64) (Snapshot, bool) {
	if connected {
		if connectedAt > current.Generation {
			next := current
			next.Online = true
			next.Generation = connectedAt
			next.ConnectedAt = connectedAt
			return next, true
		}
		if current.Online {
			return current, false
		}
		next := current
		next.Online = true
		return next, true
	}
	if !current.Online {
		return current, false
	}
	next := current
	next.Online = false
	return next, true
}
