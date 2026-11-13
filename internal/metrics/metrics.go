// Package metrics 保存进程内计数，并输出 Prometheus 文本。
// 标签只有固定的依赖名和构建信息，不放 device_id。
package metrics

import (
	"fmt"
	"strconv"
	"sync"
	"sync/atomic"
	"time"
)

// Metrics 是单进程计数器。
type Metrics struct {
	mu          sync.Mutex
	authDeny    atomic.Int64
	inbound     atomic.Int64
	cmdTimeout  atomic.Int64
	mysqlUp     atomic.Int64
	redisUp     atomic.Int64
	mqttUp      atomic.Int64
	windowStart time.Time
	windowCount int
	version     string
	revision    string
	instanceID  string
}

// New 创建计数器。构建信息只写入低基数的 info 指标。
func New(version, revision, instanceID string) *Metrics {
	return &Metrics{version: version, revision: revision, instanceID: instanceID}
}

// NoteAuthDeny 记录一次 MQTT 认证拒绝。当前 60 秒窗口里的第 5 次返回 true，供调用方打一条告警日志。
func (m *Metrics) NoteAuthDeny(now time.Time) bool {
	m.authDeny.Add(1)
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.windowStart.IsZero() || now.Sub(m.windowStart) >= time.Minute {
		m.windowStart = now
		m.windowCount = 1
		return false
	}
	m.windowCount++
	return m.windowCount == 5
}

// NoteInbound 记录一条已处理的设备上行。
func (m *Metrics) NoteInbound() { m.inbound.Add(1) }

// NoteCommandTimeout 增加命令超时计数。
func (m *Metrics) NoteCommandTimeout(n int) {
	if n > 0 {
		m.cmdTimeout.Add(int64(n))
	}
}

// SetDependency 设置某个依赖当前是否可用。name 只能是 mysql、redis、mqtt。
func (m *Metrics) SetDependency(name string, up bool) {
	value := int64(0)
	if up {
		value = 1
	}
	switch name {
	case "mysql":
		m.mysqlUp.Store(value)
	case "redis":
		m.redisUp.Store(value)
	case "mqtt":
		m.mqttUp.Store(value)
	}
}

// Text 返回 Prometheus 文本。
func (m *Metrics) Text() string {
	var b []byte
	b = appendCounter(b, "iot_mqtt_auth_deny_total", "MQTT authentication denials.", m.authDeny.Load())
	b = appendCounter(b, "iot_mqtt_inbound_total", "Inbound device messages accepted or rejected after persistence.", m.inbound.Load())
	b = appendCounter(b, "iot_command_timeout_total", "Commands moved to timeout.", m.cmdTimeout.Load())
	b = appendGauge(b, "iot_dependency_up", "Dependency health.", "dependency", "mysql", m.mysqlUp.Load())
	b = appendGauge(b, "iot_dependency_up", "", "dependency", "redis", m.redisUp.Load())
	b = appendGauge(b, "iot_dependency_up", "", "dependency", "mqtt", m.mqttUp.Load())
	b = fmt.Appendf(b, "iot_build_info{version=%s,revision=%s,instance=%s} 1\n",
		strconv.Quote(m.version), strconv.Quote(m.revision), strconv.Quote(m.instanceID))
	return string(b)
}

func appendCounter(dst []byte, name, help string, value int64) []byte {
	dst = fmt.Appendf(dst, "# TYPE %s counter\n", name)
	if help != "" {
		dst = fmt.Appendf(dst, "# HELP %s %s\n", name, help)
	}
	return fmt.Appendf(dst, "%s %d\n", name, value)
}

func appendGauge(dst []byte, name, help, label, labelValue string, value int64) []byte {
	if help != "" {
		dst = fmt.Appendf(dst, "# TYPE %s gauge\n# HELP %s %s\n", name, name, help)
	}
	return fmt.Appendf(dst, "%s{%s=%s} %d\n", name, label, strconv.Quote(labelValue), value)
}
