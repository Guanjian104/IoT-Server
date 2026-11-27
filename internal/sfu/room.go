// Package sfu 在房间内转发 RTP。第一版只转发，不转码。
package sfu

import (
	"context"
	"errors"
	"io"
	"net"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/pion/ice/v4"
	"github.com/pion/webrtc/v4"
)

// OfferHandler 收到 SFU 发给某个成员的新 offer。回调里完成对端应答后调用 HandleAnswer。
type OfferHandler func(peerID string, offer webrtc.SessionDescription)

var (
	errRoomClosed  = errors.New("room is closed")
	errPeerExists  = errors.New("peer already joined")
	errPeerMissing = errors.New("peer not joined")
)

// Room 把一个成员发布的轨道转发给其他成员。
type Room struct {
	id      string
	api     *webrtc.API
	onOffer OfferHandler

	mu     sync.Mutex
	ctx    context.Context
	cancel context.CancelFunc
	closed bool
	err    error
	peers  map[string]*peer
	tracks map[string]*forwardTrack

	debugMu sync.Mutex
	uplink  atomic.Pointer[rtpDebug]
}

type peer struct {
	id          string
	pc          *webrtc.PeerConnection
	signal      sync.Mutex
	established bool
	dirty       bool
	life        sync.Mutex
	stopped     bool
	done        chan struct{}
	neg         chan struct{}
	downlink    *webrtc.TrackLocalStaticRTP
}

type forwardTrack struct {
	publisherID string
	id          string
	kind        string
	local       *webrtc.TrackLocalStaticRTP
	cancel      context.CancelFunc
	subMu       sync.Mutex
	subs        []rtpSubscriber
}

// NewAPI 创建只走本机 IPv4、关闭 mDNS 的 WebRTC API。房间和测试端点共用它。
func NewAPI() *webrtc.API {
	api, err := buildAPI(Endpoint{})
	if err != nil {
		panic(err)
	}
	return api
}

// Endpoint 是设备从公网加入时使用的地址。留空时候选地址仍是本机 IPv4。
type Endpoint struct {
	PublicIP  string
	PublicURL string
	UDPMin    uint16
	UDPMax    uint16
}

func buildAPI(ep Endpoint) (*webrtc.API, error) {
	var settings webrtc.SettingEngine
	settings.SetICEMulticastDNSMode(ice.MulticastDNSModeDisabled)
	settings.SetNetworkTypes([]webrtc.NetworkType{webrtc.NetworkTypeUDP4})
	if ep.UDPMin != 0 || ep.UDPMax != 0 {
		if err := settings.SetEphemeralUDPPortRange(ep.UDPMin, ep.UDPMax); err != nil {
			return nil, err
		}
	}
	if ep.PublicIP != "" {
		if err := settings.SetICEAddressRewriteRules(webrtc.ICEAddressRewriteRule{
			External:        []string{ep.PublicIP},
			AsCandidateType: webrtc.ICECandidateTypeHost,
			Mode:            webrtc.ICEAddressRewriteReplace,
		}); err != nil {
			return nil, err
		}
	}
	return webrtc.NewAPI(webrtc.WithSettingEngine(settings)), nil
}

// NewRoom 创建空房间。onOffer 在需要重新协商时被调用，且不会持有房间锁。
func NewRoom(id string, api *webrtc.API, onOffer OfferHandler) (*Room, error) {
	if id == "" {
		return nil, errors.New("room id is empty")
	}
	if api == nil {
		return nil, errors.New("webrtc api is nil")
	}
	ctx, cancel := context.WithCancel(context.Background())
	return &Room{
		id:      id,
		api:     api,
		onOffer: onOffer,
		ctx:     ctx,
		cancel:  cancel,
		peers:   map[string]*peer{},
		tracks:  map[string]*forwardTrack{},
	}, nil
}

// ID 返回房间标识。
func (r *Room) ID() string { return r.id }

// delivers 决定一条轨道要不要转给这个成员。
// 观看者只收设备公开流。通话上行可以到设备和通话对端，不进入观看者。
func delivers(to, from string) bool {
	if to == "" || from == "" || to == from || strings.HasPrefix(from, "viewer:") {
		return false
	}
	if strings.HasPrefix(to, "viewer:") {
		return strings.HasPrefix(from, "device:")
	}
	return true
}

// Join 为成员建立 SFU 侧的 PeerConnection。发布端随后用 HandleOffer 把轨道送进来。
// 同一身份再次加入时关掉上一轮连接，避免 DTLS 失败后的残留把重试拒成 409。
func (r *Room) Join(id string) error {
	if id == "" {
		return errors.New("peer id is empty")
	}
	pc, err := r.api.NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		return err
	}
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		_ = pc.Close()
		return errRoomClosed
	}
	previous := r.peers[id]
	r.mu.Unlock()
	if previous != nil {
		r.release(previous)
	}

	member := &peer{id: id, pc: pc, done: make(chan struct{}), neg: make(chan struct{}, 1)}
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		_ = pc.Close()
		return errRoomClosed
	}
	if _, exists := r.peers[id]; exists {
		r.mu.Unlock()
		_ = pc.Close()
		return errPeerExists
	}
	r.peers[id] = member
	r.mu.Unlock()

	pc.OnConnectionStateChange(func(state webrtc.PeerConnectionState) {
		if state != webrtc.PeerConnectionStateFailed && state != webrtc.PeerConnectionStateClosed {
			return
		}
		go r.release(member)
	})
	pc.OnTrack(func(track *webrtc.TrackRemote, _ *webrtc.RTPReceiver) {
		r.forward(id, track)
	})
	go member.loop(r)
	return nil
}

// compatibleTracks 取出这份 Offer 能够接收的已有音频。编码不在 Offer 里的轨道留在房间，不接到这条新连接上。
func (r *Room) compatibleTracks(id string, offer webrtc.SessionDescription) []*forwardTrack {
	r.mu.Lock()
	defer r.mu.Unlock()
	var tracks []*forwardTrack
	for _, track := range r.tracks {
		if !delivers(id, track.publisherID) || !offerSupports(offer, track.local.Codec()) {
			continue
		}
		tracks = append(tracks, track)
	}
	return tracks
}

func offerSupports(offer webrtc.SessionDescription, codec webrtc.RTPCodecCapability) bool {
	name := codec.MimeType
	if i := strings.LastIndex(name, "/"); i >= 0 {
		name = name[i+1:]
	}
	name = strings.ToLower(strings.TrimSpace(name))
	if name == "" {
		return false
	}
	needle := name + "/"
	if codec.ClockRate != 0 {
		needle += strconv.FormatUint(uint64(codec.ClockRate), 10)
	}
	for _, line := range strings.Split(offer.SDP, "\n") {
		line = strings.ToLower(strings.TrimSpace(strings.TrimRight(line, "\r")))
		if !strings.HasPrefix(line, "a=rtpmap:") {
			continue
		}
		_, value, ok := strings.Cut(line, " ")
		if ok && strings.HasPrefix(strings.TrimSpace(value), needle) {
			return true
		}
	}
	return false
}

// HandleOffer 处理成员发来的 offer，返回已经收集完本机候选地址的 answer。
func (r *Room) HandleOffer(id string, offer webrtc.SessionDescription) (webrtc.SessionDescription, error) {
	member, err := r.member(id)
	if err != nil {
		return webrtc.SessionDescription{}, err
	}
	tracks := r.compatibleTracks(id, offer)
	member.signal.Lock()
	defer member.signal.Unlock()
	for _, track := range tracks {
		sender, addErr := member.pc.AddTrack(track.local)
		if addErr != nil {
			continue
		}
		track.addSub(id, sender)
		member.dirty = true
		go readRTCP(r.ctx, sender)
	}
	if len(tracks) == 0 {
		if err := r.attachDownlink(member, offer); err != nil {
			return webrtc.SessionDescription{}, err
		}
	}
	if err := member.pc.SetRemoteDescription(offer); err != nil {
		return webrtc.SessionDescription{}, err
	}
	answer, err := member.pc.CreateAnswer(nil)
	if err != nil {
		return webrtc.SessionDescription{}, err
	}
	if err := member.pc.SetLocalDescription(answer); err != nil {
		return webrtc.SessionDescription{}, err
	}
	<-webrtc.GatheringCompletePromise(member.pc)
	local := member.pc.LocalDescription()
	if local == nil {
		return webrtc.SessionDescription{}, errors.New("local description is empty")
	}
	member.established = true
	if member.dirty {
		r.requestNegotiation(id)
	}
	return *local, nil
}

// HandleAnswer 处理成员对 SFU offer 的应答。
func (r *Room) HandleAnswer(id string, answer webrtc.SessionDescription) error {
	member, err := r.member(id)
	if err != nil {
		return err
	}
	member.signal.Lock()
	err = member.pc.SetRemoteDescription(answer)
	dirty := member.dirty
	member.signal.Unlock()
	if err != nil {
		return err
	}
	if dirty {
		r.requestNegotiation(id)
	}
	return nil
}

// Err 返回转发过程中的第一个错误。
func (r *Room) Err() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.err
}

// Close 停止转发并关闭房间内的连接。
func (r *Room) Close() error {
	r.stopUplinkDebug()
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return nil
	}
	r.closed = true
	r.cancel()
	peers := make([]*peer, 0, len(r.peers))
	for _, member := range r.peers {
		peers = append(peers, member)
	}
	tracks := make([]*forwardTrack, 0, len(r.tracks))
	for _, track := range r.tracks {
		tracks = append(tracks, track)
	}
	r.peers = map[string]*peer{}
	r.tracks = map[string]*forwardTrack{}
	r.mu.Unlock()

	var result error
	for _, track := range tracks {
		track.cancel()
	}
	for _, member := range peers {
		if err := member.pc.Close(); err != nil && result == nil {
			result = err
		}
	}
	return result
}

func (r *Room) forward(publisherID string, remote *webrtc.TrackRemote) {
	codec := remote.Codec().RTPCodecCapability
	local, err := webrtc.NewTrackLocalStaticRTP(codec, remote.ID(), remote.StreamID())
	if err != nil {
		r.fail(err)
		return
	}
	ctx, cancel := context.WithCancel(r.ctx)
	kind := "video"
	if remote.Kind() == webrtc.RTPCodecTypeAudio {
		kind = "audio"
	}
	track := &forwardTrack{publisherID: publisherID, id: remote.ID(), kind: kind, local: local, cancel: cancel}
	key := publisherID + "/" + remote.ID()

	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		cancel()
		return
	}
	if _, exists := r.tracks[key]; exists {
		r.mu.Unlock()
		cancel()
		return
	}
	r.tracks[key] = track
	targets := make([]*peer, 0, len(r.peers))
	var ready []*webrtc.TrackLocalStaticRTP
	for id, member := range r.peers {
		if !delivers(id, publisherID) {
			continue
		}
		if member.downlink != nil && sameCodec(member.downlink.Codec(), codec) {
			ready = append(ready, member.downlink)
			continue
		}
		targets = append(targets, member)
	}
	r.mu.Unlock()

	for _, member := range targets {
		sender, addErr := member.pc.AddTrack(local)
		if addErr != nil {
			r.fail(addErr)
			continue
		}
		track.addSub(member.id, sender)
		go readRTCP(r.ctx, sender)
		member.signal.Lock()
		member.dirty = true
		member.signal.Unlock()
		r.requestNegotiation(member.id)
	}
	go r.pump(ctx, remote, local, ready, track)
}

func (r *Room) pump(ctx context.Context, remote *webrtc.TrackRemote, local *webrtc.TrackLocalStaticRTP, ready []*webrtc.TrackLocalStaticRTP, track *forwardTrack) {
	buffer := make([]byte, 1500)
	for {
		if ctx.Err() != nil {
			return
		}
		if err := remote.SetReadDeadline(time.Now().Add(200 * time.Millisecond)); err != nil {
			r.fail(err)
			return
		}
		n, _, err := remote.Read(buffer)
		if err != nil {
			var netErr net.Error
			if errors.As(err, &netErr) && netErr.Timeout() {
				continue
			}
			if errors.Is(err, io.EOF) || ctx.Err() != nil {
				return
			}
			r.fail(err)
			return
		}
		_, err = local.Write(buffer[:n])
		// 调试关闭时只读一个空指针，不改这一包的内容，也不改写给谁。
		r.noteDeviceUplink(track, remote, buffer[:n], err)
		if err != nil && !errors.Is(err, io.ErrClosedPipe) {
			r.fail(err)
			return
		}
		for _, extra := range ready {
			_, _ = extra.Write(buffer[:n])
		}
	}
}

// attachDownlink 在项圈先加入、对端还没有音轨时先放一条同编码的发送轨。
// 这样第一份应答就是 sendrecv，项圈不用再协商一次也能听到后来的下行。
func (r *Room) attachDownlink(member *peer, offer webrtc.SessionDescription) error {
	side, ok := voiceCallSide(member.id)
	if !ok || side != "DEVICE" || !offerWantsReceive(offer) {
		return nil
	}
	codec, ok := offeredVoiceCodec(offer)
	if !ok {
		return nil
	}
	track, err := webrtc.NewTrackLocalStaticRTP(codec, "downlink", "downlink")
	if err != nil {
		return err
	}
	sender, err := member.pc.AddTrack(track)
	if err != nil {
		return err
	}
	r.mu.Lock()
	member.downlink = track
	r.mu.Unlock()
	go readRTCP(r.ctx, sender)
	return nil
}

func offerWantsReceive(offer webrtc.SessionDescription) bool {
	sendOnly := false
	for _, line := range strings.Split(offer.SDP, "\n") {
		line = strings.ToLower(strings.TrimSpace(strings.TrimRight(line, "\r")))
		switch line {
		case "a=sendrecv", "a=recvonly":
			return true
		case "a=sendonly":
			sendOnly = true
		}
	}
	return !sendOnly
}

func offeredVoiceCodec(offer webrtc.SessionDescription) (webrtc.RTPCodecCapability, bool) {
	for _, codec := range []webrtc.RTPCodecCapability{
		{MimeType: webrtc.MimeTypeG722, ClockRate: 8000},
		{MimeType: webrtc.MimeTypePCMA, ClockRate: 8000},
	} {
		if offerSupports(offer, codec) {
			return codec, true
		}
	}
	return webrtc.RTPCodecCapability{}, false
}

func sameCodec(left, right webrtc.RTPCodecCapability) bool {
	return codecName(left) == codecName(right) && left.ClockRate == right.ClockRate && codecName(left) != ""
}

func codecName(codec webrtc.RTPCodecCapability) string {
	name := codec.MimeType
	if i := strings.LastIndex(name, "/"); i >= 0 {
		name = name[i+1:]
	}
	return strings.ToLower(strings.TrimSpace(name))
}

func (r *Room) offer(member *peer) {
	r.mu.Lock()
	current := r.peers[member.id]
	closed := r.closed
	r.mu.Unlock()
	if closed || current != member {
		return
	}
	select {
	case <-member.done:
		return
	default:
	}
	member.signal.Lock()
	if !member.established || !member.dirty || member.pc.SignalingState() != webrtc.SignalingStateStable {
		retry := member.established && member.dirty
		member.signal.Unlock()
		if retry {
			time.AfterFunc(25*time.Millisecond, func() { r.requestNegotiation(member.id) })
		}
		return
	}
	offer, err := member.pc.CreateOffer(nil)
	if err != nil {
		member.signal.Unlock()
		r.fail(err)
		return
	}
	if err := member.pc.SetLocalDescription(offer); err != nil {
		member.signal.Unlock()
		r.fail(err)
		return
	}
	member.dirty = false
	pc := member.pc
	member.signal.Unlock()

	select {
	case <-r.ctx.Done():
		return
	case <-webrtc.GatheringCompletePromise(pc):
	}
	local := pc.LocalDescription()
	if local != nil && r.onOffer != nil {
		r.onOffer(member.id, *local)
	}
}

func (r *Room) requestNegotiation(id string) {
	member, err := r.member(id)
	if err != nil {
		return
	}
	select {
	case <-r.ctx.Done():
	case member.neg <- struct{}{}:
	default:
		time.AfterFunc(25*time.Millisecond, func() { r.requestNegotiation(id) })
	}
}

// Remove 关闭一个成员。房间和其他成员继续保留。
func (r *Room) Remove(id string) error {
	r.mu.Lock()
	member := r.peers[id]
	r.mu.Unlock()
	if member == nil {
		return nil
	}
	r.release(member)
	return nil
}

// release 停掉这个成员自己的协商循环，并卸下它发布的轨道。
// 只有映射里仍是这一份连接时才从房间删除，避免关掉已被替换的旧连接时误删新连接。
func (r *Room) release(member *peer) {
	if member == nil {
		return
	}
	member.stop()
	r.mu.Lock()
	var stale []*forwardTrack
	if r.peers[member.id] == member {
		delete(r.peers, member.id)
		for key, track := range r.tracks {
			if track.publisherID == member.id {
				stale = append(stale, track)
				delete(r.tracks, key)
			}
		}
	}
	r.mu.Unlock()
	for _, track := range stale {
		track.cancel()
	}
	_ = member.pc.Close()
}

func (p *peer) stop() {
	p.life.Lock()
	defer p.life.Unlock()
	if p.stopped {
		return
	}
	p.stopped = true
	close(p.done)
}

func (r *Room) joined(id string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	_, ok := r.peers[id]
	return ok
}

func (r *Room) published(publisherID string) []publishedTrack {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]publishedTrack, 0, len(r.tracks))
	for _, track := range r.tracks {
		if track.publisherID == publisherID {
			out = append(out, publishedTrack{id: track.id, kind: track.kind})
		}
	}
	return out
}

type publishedTrack struct {
	id   string
	kind string
}

func (r *Room) member(id string) (*peer, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return nil, errRoomClosed
	}
	member := r.peers[id]
	if member == nil {
		return nil, errPeerMissing
	}
	return member, nil
}

func (r *Room) fail(err error) {
	if err == nil {
		return
	}
	r.mu.Lock()
	if r.err == nil {
		r.err = err
	}
	r.mu.Unlock()
}

func (p *peer) loop(r *Room) {
	for {
		select {
		case <-r.ctx.Done():
			return
		case <-p.done:
			return
		case <-p.neg:
			r.offer(p)
		}
	}
}

func readRTCP(ctx context.Context, sender *webrtc.RTPSender) {
	buffer := make([]byte, 1500)
	for ctx.Err() == nil {
		if _, _, err := sender.Read(buffer); err != nil {
			return
		}
	}
}
