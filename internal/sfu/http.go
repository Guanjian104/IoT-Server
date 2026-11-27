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

	"iot-server/internal/logger"
)

// Node 在本机提供 WHIP、WHEP，并保管这些房间的短期凭证密钥。
type Node struct {
	api       *webrtc.API
	secret    []byte
	base      string
	publicURL string
	http      *http.Server
	log       logger.Logger

	mu         sync.Mutex
	rooms      map[string]*Room
	bySession  map[string]string
	sessionOf  map[string]string
	boxes      map[string]chan webrtc.SessionDescription
	debugDir   string
	rtpReports map[string]RTPReport
}

// Listen 在本机地址上启动 WHIP 和 WHEP。端口可以是 0。
func Listen(addr string) (*Node, error) {
	return ListenEndpoint(addr, Endpoint{})
}

// ListenEndpoint 在本机地址上启动 WHIP 和 WHEP。Endpoint 把发给设备的地址改成公网地址。
func ListenEndpoint(addr string, ep Endpoint) (*Node, error) {
	secret := make([]byte, 32)
	if _, err := rand.Read(secret); err != nil {
		return nil, err
	}
	api, err := buildAPI(ep)
	if err != nil {
		return nil, err
	}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, err
	}
	node := &Node{
		api:       api,
		secret:    secret,
		base:      "http://" + ln.Addr().String(),
		publicURL: strings.TrimRight(ep.PublicURL, "/"),
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

// UseLog 把拒绝 SDP 的原因写到进程日志。没有日志器时保持静默。
func (n *Node) UseLog(log logger.Logger) {
	if n == nil {
		return
	}
	n.log = log
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
	side, call := voiceCallSide(identity)
	if !ok {
		if role == "publish" {
			n.logVoice("UNKNOWN", "whip", "sent", "room", roomID, "method", r.Method, "status", http.StatusUnauthorized)
		}
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	if call {
		n.logVoice(side, "whip", "sent", "room", roomID, "identity", identity, "method", r.Method)
	}
	if n.room(roomID) == nil {
		if call {
			n.logVoice(side, "whip", "received", "room", roomID, "identity", identity, "status", http.StatusNotFound)
		}
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	timer := time.NewTimer(10 * time.Second)
	defer timer.Stop()
	select {
	case offer := <-n.offerBox(roomID, identity):
		if call {
			n.logVoice(side, "whip", "received", "room", roomID, "identity", identity, "status", http.StatusOK, "sdp", sdpBrief(offer.SDP))
		}
		w.Header().Set("Content-Type", "application/sdp")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(offer.SDP))
	case <-timer.C:
		if call {
			n.logVoice(side, "whip", "received", "room", roomID, "identity", identity, "status", http.StatusNoContent)
		}
		w.WriteHeader(http.StatusNoContent)
	case <-r.Context().Done():
	}
}

func (n *Node) serverAnswer(w http.ResponseWriter, r *http.Request, role string) {
	roomID := r.PathValue("roomID")
	identity, ok := n.authorize(r, roomID, role)
	side, call := voiceCallSide(identity)
	if !ok {
		if role == "publish" {
			n.logVoice("UNKNOWN", "whip", "sent", "room", roomID, "method", r.Method, "status", http.StatusUnauthorized)
		}
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	room := n.room(roomID)
	if room == nil {
		if call {
			n.logVoice(side, "whip", "received", "room", roomID, "identity", identity, "status", http.StatusNotFound)
		}
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	answer, body, err := readSDP(r, webrtc.SDPTypeAnswer)
	if call {
		n.logVoice(side, "whip", "sent", "room", roomID, "identity", identity, "method", r.Method, "sdp", sdpBrief(string(body)))
	}
	if err != nil {
		n.logSDP(roomID, "answer", r.Header.Get("Content-Type"), body, err)
		if call {
			n.logVoice(side, "whip", "received", "room", roomID, "identity", identity, "status", http.StatusBadRequest)
		}
		http.Error(w, "invalid", http.StatusBadRequest)
		return
	}
	if err := room.HandleAnswer(identity, answer); err != nil {
		if sdpRejected(err) {
			n.logSDP(roomID, "answer", r.Header.Get("Content-Type"), body, err)
		}
		if call {
			n.logVoice(side, "whip", "received", "room", roomID, "identity", identity, "status", signalStatus(err))
		}
		writeSignalError(w, err)
		return
	}
	if call {
		n.logVoice(side, "whip", "received", "room", roomID, "identity", identity, "status", http.StatusNoContent)
	}
	w.WriteHeader(http.StatusNoContent)
}

func (n *Node) exchange(w http.ResponseWriter, r *http.Request, role string) {
	roomID := r.PathValue("roomID")
	identity, ok := n.authorize(r, roomID, role)
	side, call := voiceCallSide(identity)
	if !ok {
		if role == "publish" {
			n.logVoice("UNKNOWN", "whip", "sent", "room", roomID, "method", r.Method, "status", http.StatusUnauthorized)
		}
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	room := n.room(roomID)
	if room == nil {
		if call {
			n.logVoice(side, "whip", "received", "room", roomID, "identity", identity, "status", http.StatusNotFound)
		}
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	offer, body, err := readSDP(r, webrtc.SDPTypeOffer)
	if call {
		n.logVoice(side, "whip", "sent", "room", roomID, "identity", identity, "method", r.Method, "sdp", sdpBrief(string(body)))
	}
	if err != nil {
		n.logSDP(roomID, "offer", r.Header.Get("Content-Type"), body, err)
		if call {
			n.logVoice(side, "whip", "received", "room", roomID, "identity", identity, "status", http.StatusBadRequest)
		}
		http.Error(w, "invalid", http.StatusBadRequest)
		return
	}
	if err := room.Join(identity); err != nil {
		if sdpRejected(err) {
			n.logSDP(roomID, "offer", r.Header.Get("Content-Type"), body, err)
		}
		if call {
			n.logVoice(side, "whip", "received", "room", roomID, "identity", identity, "status", signalStatus(err))
		}
		writeSignalError(w, err)
		return
	}
	answer, err := room.HandleOffer(identity, offer)
	if err != nil {
		if sdpRejected(err) {
			n.logSDP(roomID, "offer", r.Header.Get("Content-Type"), body, err)
		}
		if call {
			n.logVoice(side, "whip", "received", "room", roomID, "identity", identity, "status", signalStatus(err))
		}
		writeSignalError(w, err)
		return
	}
	if call {
		n.logVoice(side, "whip", "received", "room", roomID, "identity", identity, "status", http.StatusCreated, "sdp", sdpBrief(answer.SDP))
	}
	w.Header().Set("Content-Type", "application/sdp")
	w.WriteHeader(http.StatusCreated)
	_, _ = w.Write([]byte(answer.SDP))
}

func voiceCallSide(identity string) (string, bool) {
	switch {
	case strings.HasPrefix(identity, "call:u"):
		return "APP", true
	case strings.HasPrefix(identity, "call:"):
		return "DEVICE", true
	default:
		return "", false
	}
}

func (n *Node) logVoice(side, hop, direction string, args ...any) {
	if n == nil || n.log == nil || side == "" {
		return
	}
	n.log.Info("voice-call "+side, append([]any{"hop", hop, "direction", direction}, args...)...)
}

func sdpBrief(sdp string) string {
	var lines []string
	for _, line := range strings.Split(strings.ReplaceAll(sdp, "\r\n", "\n"), "\n") {
		switch {
		case strings.HasPrefix(line, "m="),
			strings.HasPrefix(line, "a=rtpmap:"),
			strings.HasPrefix(line, "a=sendrecv"),
			strings.HasPrefix(line, "a=recvonly"),
			strings.HasPrefix(line, "a=sendonly"),
			strings.HasPrefix(line, "a=inactive"),
			strings.HasPrefix(line, "a=setup:"):
			lines = append(lines, strings.TrimSpace(line))
		}
	}
	text := strings.Join(lines, " | ")
	if len(text) > 500 {
		return text[:500]
	}
	return text
}

func signalStatus(err error) int {
	switch {
	case errors.Is(err, errPeerExists):
		return http.StatusConflict
	case errors.Is(err, errRoomClosed), errors.Is(err, errPeerMissing):
		return http.StatusNotFound
	default:
		return http.StatusBadRequest
	}
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

func readSDP(r *http.Request, kind webrtc.SDPType) (webrtc.SessionDescription, []byte, error) {
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<16+1))
	if err != nil {
		return webrtc.SessionDescription{}, nil, err
	}
	if !strings.Contains(r.Header.Get("Content-Type"), "application/sdp") {
		return webrtc.SessionDescription{}, body, errors.New("content type")
	}
	if len(body) == 0 || len(body) > 1<<16 {
		return webrtc.SessionDescription{}, body, errors.New("sdp")
	}
	return webrtc.SessionDescription{Type: kind, SDP: string(body)}, body, nil
}

func sdpRejected(err error) bool {
	return err != nil && !errors.Is(err, errPeerExists) && !errors.Is(err, errRoomClosed) && !errors.Is(err, errPeerMissing)
}

func (n *Node) logSDP(roomID, kind, contentType string, body []byte, err error) {
	if n == nil || n.log == nil || err == nil {
		return
	}
	n.log.Warn("sfu sdp rejected",
		"room_id", roomID,
		"kind", kind,
		"content_type", contentType,
		"bytes", len(body),
		"err", err.Error(),
		"sdp", string(body),
	)
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
