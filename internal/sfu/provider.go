package sfu

import (
	"context"
	"errors"
	"regexp"
	"strings"
	"time"

	"iot-server/internal/media"
)

// Provider 把本机 SFU 适配成业务层的 media.Provider。调用方看不到 Pion 类型。
type Provider struct {
	node *Node
}

var _ media.Provider = (*Provider)(nil)

var (
	sessionPattern  = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)
	identityPattern = regexp.MustCompile(`^(viewer|device|call):[A-Za-z0-9_-]{1,64}$`)
)

// Media 返回与这个节点共用房间和密钥的媒体适配器。
func (n *Node) Media() media.Provider {
	if n == nil {
		return nil
	}
	return &Provider{node: n}
}

// Capabilities 声明这个 SFU 转发音频和视频。
func (p *Provider) Capabilities() media.Capabilities {
	return media.Capabilities{SupportsAudio: true, SupportsVideo: true}
}

// CreateRoom 按会话建立房间。同一个会话重复调用返回同一个资源。
func (p *Provider) CreateRoom(_ context.Context, req media.RoomRequest) (media.Room, error) {
	id, created, err := p.node.create(req.SessionID)
	if err != nil {
		return media.Room{}, err
	}
	return media.Room{ID: id, SessionID: req.SessionID, Provider: "pion", Created: created}, nil
}

// DeleteRoom 关闭房间。不存在时没有副作用。
func (p *Provider) DeleteRoom(_ context.Context, resourceID string) error {
	return p.node.deleteRoom(resourceID)
}

// CreateJoinCredential 签发最多十分钟的凭证。
// viewer 只能订阅。device 发布公开流。call 发布通话上行，不转给观看者。
func (p *Provider) CreateJoinCredential(_ context.Context, req media.JoinRequest) (media.JoinCredential, error) {
	if !identityPattern.MatchString(req.Identity) {
		return media.JoinCredential{}, errors.New("invalid identity")
	}
	ttl := time.Until(req.ExpiresAt)
	if ttl <= 0 || ttl > 10*time.Minute {
		return media.JoinCredential{}, errors.New("invalid expiry")
	}
	p.node.mu.Lock()
	_, ok := p.node.rooms[req.ResourceID]
	p.node.mu.Unlock()
	if !ok {
		return media.JoinCredential{}, errors.New("room not found")
	}
	role := "subscribe"
	if strings.HasPrefix(req.Identity, "device:") || strings.HasPrefix(req.Identity, "call:") {
		role = "publish"
	}
	token, err := sign(p.node.secret, req.ResourceID, req.Identity, role, req.ExpiresAt)
	if err != nil {
		return media.JoinCredential{}, err
	}
	return media.JoinCredential{
		Token:       token,
		Provider:    "pion",
		Connectable: true,
		ExpiresAt:   req.ExpiresAt.UTC(),
	}, nil
}

// RemoveParticipant 让一个成员离开房间。
func (p *Provider) RemoveParticipant(_ context.Context, resourceID, identity string) error {
	room := p.node.room(resourceID)
	if room == nil {
		return nil
	}
	return room.Remove(identity)
}

// GetRoom 按资源 ID 返回房间。
func (p *Provider) GetRoom(_ context.Context, resourceID string) (media.Room, error) {
	p.node.mu.Lock()
	defer p.node.mu.Unlock()
	if _, ok := p.node.rooms[resourceID]; !ok {
		return media.Room{}, errors.New("room not found")
	}
	return media.Room{ID: resourceID, SessionID: p.node.sessionOf[resourceID]}, nil
}

// GetParticipant 报告成员是否还在房间里。
func (p *Provider) GetParticipant(_ context.Context, resourceID, identity string) (media.Participant, error) {
	room := p.node.room(resourceID)
	if room == nil || !room.joined(identity) {
		return media.Participant{Identity: identity, State: "absent"}, nil
	}
	return media.Participant{Identity: identity, State: "joined"}, nil
}

// GetTracks 返回这个成员已经在房间里转发的轨道。
func (p *Provider) GetTracks(_ context.Context, resourceID, identity string) ([]media.Track, error) {
	room := p.node.room(resourceID)
	if room == nil {
		return nil, nil
	}
	published := room.published(identity)
	out := make([]media.Track, 0, len(published))
	for _, track := range published {
		out = append(out, media.Track{ID: track.id, Kind: track.kind})
	}
	return out, nil
}

// SubscribeURL 是观看端提交 offer 的地址。
func (p *Provider) SubscribeURL(resourceID string) string {
	return p.node.base + "/whep/" + resourceID
}

// PublishURL 是发布端提交 offer 的地址。
func (p *Provider) PublishURL(resourceID string) string {
	return p.node.base + "/whip/" + resourceID
}

func resourceID(sessionID string) (string, error) {
	if !sessionPattern.MatchString(sessionID) {
		return "", errors.New("invalid session")
	}
	return "pion-" + sessionID, nil
}

func (n *Node) deleteRoom(resourceID string) error {
	n.mu.Lock()
	room := n.rooms[resourceID]
	sessionID := n.sessionOf[resourceID]
	delete(n.rooms, resourceID)
	delete(n.sessionOf, resourceID)
	if sessionID != "" {
		delete(n.bySession, sessionID)
	}
	prefix := resourceID + "\n"
	for key := range n.boxes {
		if strings.HasPrefix(key, prefix) {
			delete(n.boxes, key)
		}
	}
	n.mu.Unlock()
	if room == nil {
		return nil
	}
	return room.Close()
}
