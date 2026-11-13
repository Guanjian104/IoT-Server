// Package streamctl 定义设备开流和关流接口。P2 不向固件发送未定义的视频指令。
package streamctl

import (
	"context"
	"errors"
)

// ErrOutcome 表示测试脚本给出了无法识别的结果。
var ErrOutcome = errors.New("outcome")

// Controller 把业务请求和设备协议隔开。
type Controller interface {
	Start(ctx context.Context, req Request) (Result, error)
	Stop(ctx context.Context, req Request) (Result, error)
	Status(ctx context.Context, req Request) (Status, error)
}

// Request 绑定会话、请求和轮次。
type Request struct {
	DeviceID   string
	ChannelID  string
	StreamType string
	Resolution string
	SessionID  string
	RequestID  string
	Generation int64
}

// Result 是一次控制尝试的结果。unknown 表示命令可能已经发出。
type Result struct {
	Outcome string
}

// Status 是查询到的流状态。Known 为 false 时不能当作已经关闭。
type Status struct {
	Active bool
	Known  bool
}

// Interpret 把共享测试脚本转换成结果。空脚本按成功处理。
func Interpret(raw string) (string, error) {
	switch raw {
	case "", "success", "fail", "unknown", "timeout":
		if raw == "" {
			return "success", nil
		}
		return raw, nil
	default:
		return "", ErrOutcome
	}
}

// Script 是单进程测试控制器，用请求 ID 保持重复调用的结果稳定。
type Script struct {
	StartOutcome string
	StopOutcome  string
	StatusKnown  bool
	StatusActive bool
	seen         map[string]string
}

// Start 返回预设结果，并对同一个请求 ID 返回第一次的结果。
func (s *Script) Start(_ context.Context, req Request) (Result, error) {
	return s.once("start:"+req.RequestID, s.StartOutcome)
}

// Stop 返回预设结果，并对同一个请求 ID 返回第一次的结果。
func (s *Script) Stop(_ context.Context, req Request) (Result, error) {
	return s.once("stop:"+req.RequestID, s.StopOutcome)
}

// Status 返回预设查询结果。
func (s *Script) Status(context.Context, Request) (Status, error) {
	return Status{Active: s.StatusActive, Known: s.StatusKnown}, nil
}

func (s *Script) once(key, outcome string) (Result, error) {
	value, err := Interpret(outcome)
	if err != nil {
		return Result{}, err
	}
	if s.seen == nil {
		s.seen = map[string]string{}
	}
	if first, ok := s.seen[key]; ok {
		return Result{Outcome: first}, nil
	}
	s.seen[key] = value
	return Result{Outcome: value}, nil
}
