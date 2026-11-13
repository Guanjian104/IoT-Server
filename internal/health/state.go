// Package health 保存进程是否可以接收业务流量的状态。
// 存活和就绪分开：进程仍在退出时可以存活，但不再就绪。
package health

import "sync"

// State 是就绪探针读取的共享状态。
type State struct {
	mu    sync.RWMutex
	ready bool
}

// SetReady 设置就绪状态。
func (s *State) SetReady(ready bool) {
	s.mu.Lock()
	s.ready = ready
	s.mu.Unlock()
}

// Ready 报告当前是否就绪。
func (s *State) Ready() bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.ready
}
