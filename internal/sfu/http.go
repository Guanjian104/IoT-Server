package sfu

import (
	"context"
	"crypto/rand"
	"errors"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/pion/webrtc/v4"
)

// Node 在本机提供 WHIP、WHEP，并保管这些房间的短期凭证密钥。
type Node struct {
	api    *webrtc.API
	secret []byte
	base   string
	http   *http.Server

	mu        sync.Mutex
	rooms     map[string]*Room
	bySession map[string]string
	sessionOf map[string]string
	boxes     map[string]chan webrtc.SessionDescription
}

// Listen 在本机地址上启动 WHIP 和 WHEP。端口可以是 0。
func Listen(addr string) (*Node, error) {
	secret := make([]byte, 32)
	if _, err := rand.Read(secret); err != nil {
		return nil, err
	}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, err
	}
	node := &Node{
		api:       NewAPI(),
		secret:    secret,
		base:      "http://" + ln.Addr().String(),
		rooms:     map[string]*Room{},
		bySession: map[string]string{},
		sessionOf: map[string]string{},
		boxes:     map[string]chan webrtc.SessionDescription{},
	}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /whip/{roomID}", node.whip)
	mux.HandleFunc("GET /whip/{roomID}", node.whipOffer)
	mux.HandleFunc("PUT /whip/{roomID}", node.whipAnswer)
	mux.HandleFunc("POST /whep/{roomID}", node.whep)
	mux.HandleFunc("GET /whep/{roomID}", node.whepOffer)
	mux.HandleFunc("PUT /whep/{roomID}", node.whepAnswer)
	node.http = &http.Server{
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       20 * time.Second,
		WriteTimeout:      20 * time.Second,
		IdleTimeout:       60 * time.Second,
	}
	go func() { _ = node.http.Serve(ln) }()
	return node, nil
}

// Close 停止 HTTP 并关闭全部房间。
func (n *Node) Close() error {
	if n == nil {
		return nil
	}
	var err error
	if n.http != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		err = n.http.Shutdown(ctx)
		cancel()
	}
	n.mu.Lock()
	rooms := n.rooms
	n.rooms = map[string]*Room{}
	n.bySession = map[string]string{}
	n.sessionOf = map[string]string{}
	n.boxes = map[string]chan webrtc.SessionDescription{}
	n.mu.Unlock()
	for _, room := range rooms {
		if closeErr := room.Close(); err == nil {
			err = closeErr
		}
	}
	return err
}

func (n *Node) whip(w http.ResponseWriter, r *http.Request) {
	n.exchange(w, r, "publish")
}

func (n *Node) whep(w http.ResponseWriter, r *http.Request) {
	n.exchange(w, r, "subscribe")
}

func (n *Node) whipOffer(w http.ResponseWriter, r *http.Request) {
	n.serverOffer(w, r, "publish")
}

func (n *Node) whipAnswer(w http.ResponseWriter, r *http.Request) {
	n.serverAnswer(w, r, "publish")
}

// whepOffer 返回 SFU 在增加轨道后发出的 offer。应答不能新增媒体行，所以订阅端要再完成这一轮。
func (n *Node) whepOffer(w http.ResponseWriter, r *http.Request) {
	n.serverOffer(w, r, "subscribe")
}

func (n *Node) whepAnswer(w http.ResponseWriter, r *http.Request) {
	n.serverAnswer(w, r, "subscribe")
}

func (n *Node) serverOffer(w http.ResponseWriter, r *http.Request, role string) {
	roomID := r.PathValue("roomID")
	identity, ok := n.authorize(r, roomID, role)
	if !ok {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	if n.room(roomID) == nil {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	timer := time.NewTimer(10 * time.Second)
	defer timer.Stop()
	select {
	case offer := <-n.offerBox(roomID, identity):
		w.Header().Set("Content-Type", "application/sdp")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(offer.SDP))
	case <-timer.C:
		w.WriteHeader(http.StatusNoContent)
	case <-r.Context().Done():
	}
}

func (n *Node) serverAnswer(w http.ResponseWriter, r *http.Request, role string) {
	roomID := r.PathValue("roomID")
	identity, ok := n.authorize(r, roomID, role)
	if !ok {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	room := n.room(roomID)
	if room == nil {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	answer, err := readSDP(r, webrtc.SDPTypeAnswer)
	if err != nil {
		http.Error(w, "invalid", http.StatusBadRequest)
		return
	}
	if err := room.HandleAnswer(identity, answer); err != nil {
		writeSignalError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (n *Node) exchange(w http.ResponseWriter, r *http.Request, role string) {
	roomID := r.PathValue("roomID")
	identity, ok := n.authorize(r, roomID, role)
	if !ok {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	room := n.room(roomID)
	if room == nil {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	offer, err := readSDP(r, webrtc.SDPTypeOffer)
	if err != nil {
		http.Error(w, "invalid", http.StatusBadRequest)
		return
	}
	if err := room.Join(identity); err != nil {
		writeSignalError(w, err)
		return
	}
	answer, err := room.HandleOffer(identity, offer)
	if err != nil {
		writeSignalError(w, err)
		return
	}
	w.Header().Set("Content-Type", "application/sdp")
	w.WriteHeader(http.StatusCreated)
	_, _ = w.Write([]byte(answer.SDP))
}

func (n *Node) authorize(r *http.Request, roomID, role string) (string, bool) {
	header, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !ok || header == "" || strings.Contains(header, " ") {
		return "", false
	}
	resource, identity, got, exp, err := openToken(n.secret, header)
	if err != nil || resource != roomID || got != role || !time.Now().Before(exp) {
		return "", false
	}
	return identity, true
}

func (n *Node) create(sessionID string) (string, bool, error) {
	id, err := resourceID(sessionID)
	if err != nil {
		return "", false, err
	}
	n.mu.Lock()
	defer n.mu.Unlock()
	if existing, ok := n.bySession[sessionID]; ok {
		return existing, false, nil
	}
	room, err := NewRoom(id, n.api, func(peerID string, offer webrtc.SessionDescription) {
		n.pushOffer(id, peerID, offer)
	})
	if err != nil {
		return "", false, err
	}
	n.rooms[id] = room
	n.bySession[sessionID] = id
	n.sessionOf[id] = sessionID
	return id, true, nil
}

func (n *Node) room(id string) *Room {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.rooms[id]
}

func (n *Node) offerBox(roomID, peerID string) chan webrtc.SessionDescription {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.boxLocked(roomID, peerID)
}

func (n *Node) pushOffer(roomID, peerID string, offer webrtc.SessionDescription) {
	n.mu.Lock()
	ch := n.boxLocked(roomID, peerID)
	n.mu.Unlock()
	select {
	case ch <- offer:
	default:
	}
}

func (n *Node) boxLocked(roomID, peerID string) chan webrtc.SessionDescription {
	key := roomID + "\n" + peerID
	ch := n.boxes[key]
	if ch == nil {
		ch = make(chan webrtc.SessionDescription, 4)
		n.boxes[key] = ch
	}
	return ch
}

func readSDP(r *http.Request, kind webrtc.SDPType) (webrtc.SessionDescription, error) {
	if !strings.Contains(r.Header.Get("Content-Type"), "application/sdp") {
		return webrtc.SessionDescription{}, errors.New("content type")
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<16+1))
	if err != nil || len(body) == 0 || len(body) > 1<<16 {
		return webrtc.SessionDescription{}, errors.New("sdp")
	}
	return webrtc.SessionDescription{Type: kind, SDP: string(body)}, nil
}

func writeSignalError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, errPeerExists):
		http.Error(w, "conflict", http.StatusConflict)
	case errors.Is(err, errRoomClosed), errors.Is(err, errPeerMissing):
		http.Error(w, "not found", http.StatusNotFound)
	default:
		http.Error(w, "invalid", http.StatusBadRequest)
	}
}
