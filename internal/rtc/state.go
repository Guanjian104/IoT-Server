// Package rtc 保存控制平面状态机。业务表的更新必须先经过这些转移。
package rtc

import (
	"errors"
	"time"
)

// ErrState 表示当前状态不接受这个事件。
var ErrState = errors.New("state")

// AcceptEvent 只接受与当前轮次相同的事件。更早的轮次不能改写新会话。
func AcceptEvent(current, event int64) bool {
	return current > 0 && event == current
}

// SameRound 要求事件中的会话和轮次就是这路通话自己的。
func SameRound(callSession string, callGeneration int64, eventSession string, eventGeneration int64) bool {
	return callSession != "" && eventSession == callSession && AcceptEvent(callGeneration, eventGeneration)
}

// CanOperateCall 判断这个用户是不是这路通话里可以做该操作的人。
// 主叫可以取消振铃并结束已接听的通话。被叫可以接听、拒绝，或在振铃时挂断。
// 实际接听人可以结束自己接通后的通话。其他人即使有设备通话权限也不能操作。
func CanOperateCall(actor, caller, callee, answeredBy int64, status, requested string) bool {
	if actor <= 0 || caller <= 0 {
		return false
	}
	switch requested {
	case "accept", "reject":
		return status == "RINGING" && callee > 0 && actor == callee && actor != caller
	case "end":
		switch status {
		case "RINGING":
			if actor == caller {
				return true
			}
			return callee > 0 && actor == callee && actor != caller
		case "ACCEPTED", "CONNECTED":
			if actor == caller {
				return true
			}
			return answeredBy > 0 && actor == answeredBy
		default:
			return false
		}
	default:
		return false
	}
}

// NextSession 计算会话的下一个状态。mark 和 resume 在条件不足时保持原状态。
func NextSession(from, event string, deviceReady, mediaReady bool) (string, error) {
	switch event {
	case "open":
		if from == "CREATED" {
			return "WAITING_DEVICE", nil
		}
	case "start":
		if from == "WAITING_DEVICE" {
			return "STREAM_STARTING", nil
		}
	case "mark":
		if from == "STREAM_STARTING" && deviceReady && mediaReady {
			return "ACTIVE", nil
		}
		if from == "STREAM_STARTING" {
			return from, nil
		}
	case "empty":
		if from == "ACTIVE" {
			return "STOP_PENDING", nil
		}
	case "resume":
		if from == "STOP_PENDING" && deviceReady && mediaReady {
			return "ACTIVE", nil
		}
		if from == "STOP_PENDING" {
			return from, nil
		}
	case "stop":
		if from == "STOP_PENDING" {
			return "STOPPING", nil
		}
	case "closed":
		if from == "STOPPING" {
			return "CLOSED", nil
		}
	case "fail":
		switch from {
		case "CREATED", "WAITING_DEVICE", "STREAM_STARTING", "STOPPING":
			return "FAILED", nil
		}
	case "interrupt":
		switch from {
		case "CREATED", "WAITING_DEVICE", "STREAM_STARTING", "ACTIVE", "STOP_PENDING":
			return "FAILED", nil
		case "STOPPING":
			return "CLOSED", nil
		}
	}
	return from, ErrState
}

// NextViewer 计算观看者的下一个状态。
func NextViewer(from, event string) (string, error) {
	switch event {
	case "activate":
		if from == "JOINING" {
			return "ACTIVE", nil
		}
	case "leave":
		switch from {
		case "JOINING", "ACTIVE", "DISCONNECTED", "LEAVING":
			return "LEFT", nil
		}
	case "drop":
		switch from {
		case "JOINING", "ACTIVE":
			return "DISCONNECTED", nil
		}
	case "reconnect":
		if from == "DISCONNECTED" {
			return "JOINING", nil
		}
	case "heartbeat":
		switch from {
		case "JOINING", "ACTIVE":
			return from, nil
		}
	}
	return from, ErrState
}

// NextCall 计算主通话的下一个状态。
func NextCall(from, event string) (string, error) {
	switch event {
	case "ring":
		if from == "CREATED" {
			return "RINGING", nil
		}
	case "accept":
		if from == "RINGING" {
			return "ACCEPTED", nil
		}
	case "connect":
		if from == "ACCEPTED" {
			return "CONNECTED", nil
		}
	case "reject":
		if from == "RINGING" {
			return "REJECTED", nil
		}
	case "timeout":
		switch from {
		case "RINGING", "ACCEPTED":
			return "TIMEOUT", nil
		}
	case "cancel":
		switch from {
		case "CREATED", "RINGING":
			return "ENDED", nil
		}
	case "end":
		switch from {
		case "ACCEPTED", "CONNECTED":
			return "ENDED", nil
		}
	case "fail":
		switch from {
		case "CREATED", "RINGING", "ACCEPTED":
			return "FAILED", nil
		}
	}
	return from, ErrState
}

// ViewerOccupies 判断观看者是否仍算作流的使用者。
// JOINING 和 ACTIVE 都只在心跳宽限期内占用。心跳停了就不再占着会话。
func ViewerOccupies(status string, age, grace time.Duration) bool {
	switch status {
	case "ACTIVE", "JOINING":
		return age >= 0 && age < grace
	default:
		return false
	}
}

// CallOccupies 判断通话是否仍占用它关联的会话。
func CallOccupies(status string) bool {
	switch status {
	case "CREATED", "RINGING", "ACCEPTED", "CONNECTED":
		return true
	default:
		return false
	}
}

// TerminalSession 报告会话是否已经结束。
func TerminalSession(status string) bool {
	return status == "CLOSED" || status == "FAILED"
}

// TerminalCall 报告通话是否已经结束。
func TerminalCall(status string) bool {
	switch status {
	case "REJECTED", "TIMEOUT", "ENDED", "FAILED":
		return true
	default:
		return false
	}
}
