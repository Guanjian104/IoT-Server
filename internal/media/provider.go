// Package media 定义与厂商无关的媒体接口。P2 只有测试替身，不产生可连接的 WebRTC 凭证。
package media

import (
	"context"
	"time"
)

// Provider 是业务层可见的媒体能力。实现里不能出现厂商 SDK 类型。
type Provider interface {
	Capabilities() Capabilities
	CreateRoom(ctx context.Context, req RoomRequest) (Room, error)
	DeleteRoom(ctx context.Context, resourceID string) error
	CreateJoinCredential(ctx context.Context, req JoinRequest) (JoinCredential, error)
	RemoveParticipant(ctx context.Context, resourceID, identity string) error
	GetRoom(ctx context.Context, resourceID string) (Room, error)
	GetParticipant(ctx context.Context, resourceID, identity string) (Participant, error)
	GetTracks(ctx context.Context, resourceID, identity string) ([]Track, error)
}

// Capabilities 是测试替身或未来适配器声明的能力。
type Capabilities struct {
	SupportsAudio bool
	SupportsVideo bool
}

// RoomRequest 用业务会话标识请求资源，不把业务 ID 当作媒体资源 ID。
type RoomRequest struct {
	SessionID  string
	Generation int64
}

// Room 是统一资源模型。
type Room struct {
	ID        string
	SessionID string
}

// JoinRequest 描述一次短期加入。
type JoinRequest struct {
	ResourceID string
	Identity   string
	ExpiresAt  time.Time
}

// JoinCredential 与业务 Ticket 分离。Connectable 为 false 时不能当作真实入会凭证。
type JoinCredential struct {
	Token       string
	Provider    string
	Connectable bool
	ExpiresAt   time.Time
}

// Participant 是统一参与者。
type Participant struct {
	Identity string
	State    string
}

// Track 是统一轨道。
type Track struct {
	ID   string
	Kind string
}

// TestProvider 在进程内返回确定性的测试资源。
type TestProvider struct{}

// Capabilities 声明测试替身具备音视频，供控制平面演练。这不是设备能力清单。
func (TestProvider) Capabilities() Capabilities {
	return Capabilities{SupportsAudio: true, SupportsVideo: true}
}

// CreateRoom 生成与会话 ID 不同的资源 ID。
func (TestProvider) CreateRoom(_ context.Context, req RoomRequest) (Room, error) {
	return Room{ID: "test:" + req.SessionID, SessionID: req.SessionID}, nil
}

// DeleteRoom 对测试资源没有外部副作用。
func (TestProvider) DeleteRoom(context.Context, string) error { return nil }

// CreateJoinCredential 签发不可连接的测试凭证。
func (TestProvider) CreateJoinCredential(_ context.Context, req JoinRequest) (JoinCredential, error) {
	return JoinCredential{Token: req.Identity, Provider: "test", Connectable: false, ExpiresAt: req.ExpiresAt}, nil
}

// RemoveParticipant 对测试资源没有外部副作用。
func (TestProvider) RemoveParticipant(context.Context, string, string) error { return nil }

// GetRoom 按资源 ID 还原测试房间。
func (TestProvider) GetRoom(_ context.Context, resourceID string) (Room, error) {
	return Room{ID: resourceID}, nil
}

// GetParticipant 只报告测试身份。
func (TestProvider) GetParticipant(_ context.Context, _, identity string) (Participant, error) {
	return Participant{Identity: identity, State: "unknown"}, nil
}

// GetTracks 不伪造已经发布的真实轨道。
func (TestProvider) GetTracks(context.Context, string, string) ([]Track, error) {
	return nil, nil
}
