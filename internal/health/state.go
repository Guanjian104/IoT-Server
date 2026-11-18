// Package health 保存进程是否可以接收业务流量的状态。
// 存活和就绪分开：进程仍在退出时可以存活，但不再就绪。
package health

import "sync"

// State 是就绪探针读取的共享状态。
type State struct {
	mu               sync.RWMutex
	ready            bool
	mqttSubscribed   bool
	commandAttention int
}

// Report 是一次就绪检查要给出去的结果。
type Report struct {
	Ready            bool
	MQTTSubscribed   bool
	CommandAttention int
}

// Publish 一次写入就绪、订阅完成和异常命令积压。
func (s *State) Publish(report Report) {
	s.mu.Lock()
	s.ready = report.Ready
	s.mqttSubscribed = report.MQTTSubscribed
	s.commandAttention = report.CommandAttention
	s.mu.Unlock()
}

// SetReady 只改就绪位。关闭过程用它把探针打成未就绪。
func (s *State) SetReady(ready bool) {
	s.mu.Lock()
	s.ready = ready
	s.mu.Unlock()
}

// Ready 报告当前是否就绪。
func (s *State) Ready() bool {
	return s.Report().Ready
}

// Report 返回最近一次发布的就绪结果。
func (s *State) Report() Report {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return Report{Ready: s.ready, MQTTSubscribed: s.mqttSubscribed, CommandAttention: s.commandAttention}
}
