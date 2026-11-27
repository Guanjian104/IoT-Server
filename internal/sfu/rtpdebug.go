package sfu

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/pion/rtp"
	"github.com/pion/webrtc/v4"

	"iot-server/internal/logger"
)

const (
	defaultRTPDebugDir   = "/tmp/rtp-debug"
	rtpDebugQueueSize    = 256
	defaultRTPDebugBytes = 1 << 20
)

var (
	ErrRTPDebugOption   = errors.New("rtp debug option")
	ErrRTPDebugNotFound = errors.New("rtp debug room not found")
	ErrRTPDebugBusy     = errors.New("rtp debug already running")
	ErrRTPDebugIdle     = errors.New("rtp debug is not running")
	ErrRTPDebugDir      = errors.New("rtp debug directory")
)

// RTPDebugOptions 打开一次设备上行诊断。Log 和 Capture 都为 false 时拒绝。
// Capture 才会把 Payload 写到磁盘，默认关闭。
type RTPDebugOptions struct {
	Log        bool
	Capture    bool
	Seconds    int
	MaxPackets int
	MaxBytes   int
	Dir        string
	Duration   time.Duration
}

type rtpSubscriber struct {
	id     string
	sender *webrtc.RTPSender
}

type rtpObservation struct {
	sample  debugPacket
	payload []byte
}

type rtpDebug struct {
	sessionID  string
	sessionDir string
	opt        RTPDebugOptions
	log        logger.Logger
	onStop     func(RTPReport)

	queue    chan rtpObservation
	stopCh   chan struct{}
	finished chan struct{}
	stopOnce sync.Once
	endOnce  sync.Once

	alive   atomic.Bool
	nextID  atomic.Uint64
	dropped atomic.Int64

	mu       sync.Mutex
	samples  []debugPacket
	stored   int
	bytes    int
	reason   string
	final    *RTPReport
	meta     *os.File
	payload  *os.File
	metaPath string
	binPath  string
}

type uplinkMeta struct {
	SequenceNumber uint16 `json:"sequence_number"`
	Timestamp      uint32 `json:"timestamp"`
	PayloadType    uint8  `json:"payload_type"`
	SSRC           uint32 `json:"ssrc"`
	Marker         bool   `json:"marker"`
	PayloadLength  int    `json:"payload_length"`
	PayloadOffset  int    `json:"payload_offset"`
	PayloadSHA256  string `json:"payload_sha256"`
}

// CapturedUplink 是按序号恢复后的一条设备上行 Payload，不含 RTP 头。
type CapturedUplink struct {
	SequenceNumber uint16
	Timestamp      uint32
	PayloadType    uint8
	SSRC           uint32
	Payload        []byte
	SHA256         string
}

func (t *forwardTrack) addSub(id string, sender *webrtc.RTPSender) {
	if t == nil || id == "" {
		return
	}
	t.subMu.Lock()
	defer t.subMu.Unlock()
	for i := range t.subs {
		if t.subs[i].id == id {
			t.subs[i].sender = sender
			return
		}
	}
	t.subs = append(t.subs, rtpSubscriber{id: id, sender: sender})
}

func (t *forwardTrack) appSubscribers() []rtpSubscriber {
	if t == nil {
		return nil
	}
	t.subMu.Lock()
	defer t.subMu.Unlock()
	out := make([]rtpSubscriber, 0, len(t.subs))
	for _, sub := range t.subs {
		side, ok := voiceCallSide(sub.id)
		if ok && side == "APP" {
			out = append(out, sub)
		}
	}
	return out
}

func (r *Room) noteDeviceUplink(track *forwardTrack, remote *webrtc.TrackRemote, raw []byte, writeErr error) {
	d := r.uplink.Load()
	if d == nil || track == nil || remote == nil || remote.Kind() != webrtc.RTPCodecTypeAudio {
		return
	}
	side, ok := voiceCallSide(track.publisherID)
	if !ok || side != "DEVICE" {
		return
	}
	d.observe(raw, track.publisherID, track.id, remote.Codec().RTPCodecCapability, track.appSubscribers(), writeErr)
}

func (r *Room) startUplinkDebug(sessionID string, opt RTPDebugOptions, log logger.Logger, onStop func(RTPReport)) error {
	opt, err := normalizeRTPDebug(opt)
	if err != nil {
		return err
	}
	r.mu.Lock()
	closed := r.closed
	r.mu.Unlock()
	if closed {
		return ErrRTPDebugNotFound
	}
	r.debugMu.Lock()
	defer r.debugMu.Unlock()
	if current := r.uplink.Load(); current != nil && current.alive.Load() {
		return ErrRTPDebugBusy
	}
	opened, err := openRTPDebug(sessionID, opt, log, onStop)
	if err != nil {
		if log != nil {
			log.Warn("rtp debug directory", "session_id", sessionID, "dir", opt.Dir, "err", err.Error())
		}
		return err
	}
	r.uplink.Store(opened)
	go opened.loop()
	if log != nil {
		log.Info("rtp debug start", "session_id", sessionID, "capture", opt.Capture, "seconds", opt.Duration.Seconds(), "max_packets", opt.MaxPackets, "max_bytes", opt.MaxBytes, "dir", opened.sessionDir)
	}
	return nil
}

func (r *Room) stopUplinkDebug() {
	if r == nil {
		return
	}
	d := r.uplink.Load()
	if d == nil {
		return
	}
	d.stop("room_closed")
	d.wait(time.Second)
}

// UseRTPDebugDir 设置诊断文件目录。空值保持进程默认的 /tmp/rtp-debug。
func (n *Node) UseRTPDebugDir(dir string) {
	if n == nil {
		return
	}
	n.mu.Lock()
	n.debugDir = strings.TrimSpace(dir)
	n.mu.Unlock()
}

// StartRTPDebug 按会话打开设备上行诊断。房间要已经存在，会话 ID 可以用项圈 MAC，也可以用 pion- 开头的房间 ID。
func (n *Node) StartRTPDebug(sessionID string, opt RTPDebugOptions) error {
	room, canonical, err := n.roomByDebugKey(sessionID)
	if err != nil {
		return err
	}
	n.mu.Lock()
	log := n.log
	if opt.Dir == "" {
		opt.Dir = n.debugDir
	}
	n.mu.Unlock()
	return room.startUplinkDebug(canonical, opt, log, func(report RTPReport) {
		n.saveRTPReport(canonical, report)
	})
}

// StopRTPDebug 提前结束诊断并返回报告。已经停过时返回上一份报告。
func (n *Node) StopRTPDebug(sessionID string) (RTPReport, error) {
	room, canonical, err := n.roomByDebugKey(sessionID)
	if err != nil {
		return RTPReport{}, err
	}
	d := room.uplink.Load()
	if d == nil || !d.alive.Load() {
		report, ok := n.savedRTPReport(canonical)
		if !ok {
			return RTPReport{}, ErrRTPDebugIdle
		}
		return report, nil
	}
	d.stop("stop")
	return d.wait(3 * time.Second), nil
}

// RTPDebugReport 返回进行中的汇总，或最近一次停止后的报告。
func (n *Node) RTPDebugReport(sessionID string) (RTPReport, error) {
	room, canonical, err := n.roomByDebugKey(sessionID)
	if err != nil && !errors.Is(err, ErrRTPDebugNotFound) {
		return RTPReport{}, err
	}
	if room != nil {
		if d := room.uplink.Load(); d != nil && d.alive.Load() {
			return d.snapshot(), nil
		}
	}
	if canonical == "" {
		canonical = sessionID
	}
	report, ok := n.savedRTPReport(canonical)
	if !ok {
		return RTPReport{}, ErrRTPDebugNotFound
	}
	return report, nil
}

func (n *Node) roomByDebugKey(key string) (*Room, string, error) {
	if n == nil || !sessionPattern.MatchString(key) {
		return nil, "", ErrRTPDebugOption
	}
	n.mu.Lock()
	defer n.mu.Unlock()
	if id, ok := n.bySession[key]; ok {
		return n.rooms[id], key, nil
	}
	if room, ok := n.rooms[key]; ok {
		session := n.sessionOf[key]
		if session == "" {
			session = key
		}
		return room, session, nil
	}
	return nil, "", ErrRTPDebugNotFound
}

func (n *Node) saveRTPReport(sessionID string, report RTPReport) {
	if n == nil || sessionID == "" {
		return
	}
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.rtpReports == nil {
		n.rtpReports = map[string]RTPReport{}
	}
	n.rtpReports[sessionID] = report
}

func (n *Node) savedRTPReport(sessionID string) (RTPReport, bool) {
	n.mu.Lock()
	defer n.mu.Unlock()
	report, ok := n.rtpReports[sessionID]
	return report, ok
}

func normalizeRTPDebug(opt RTPDebugOptions) (RTPDebugOptions, error) {
	if !opt.Log && !opt.Capture {
		return opt, ErrRTPDebugOption
	}
	if opt.Dir == "" {
		opt.Dir = defaultRTPDebugDir
	}
	if !validDebugDir(opt.Dir) {
		return opt, ErrRTPDebugDir
	}
	opt.Dir = filepath.Clean(opt.Dir)
	if opt.MaxPackets <= 0 {
		opt.MaxPackets = 500
	}
	if opt.MaxPackets > 5000 {
		opt.MaxPackets = 5000
	}
	if opt.MaxBytes <= 0 {
		opt.MaxBytes = defaultRTPDebugBytes
	}
	if opt.MaxBytes > 8<<20 {
		opt.MaxBytes = 8 << 20
	}
	if opt.Duration <= 0 {
		seconds := opt.Seconds
		if seconds <= 0 {
			seconds = 8
		}
		if seconds > 30 {
			seconds = 30
		}
		opt.Seconds = seconds
		opt.Duration = time.Duration(seconds) * time.Second
	} else if opt.Duration > 30*time.Second {
		opt.Duration = 30 * time.Second
	}
	return opt, nil
}

func validDebugDir(path string) bool {
	if len(path) < 2 || len(path) > 200 || !strings.HasPrefix(path, "/") || strings.Contains(path, "..") || strings.ContainsRune(path, 0) {
		return false
	}
	return path != "/"
}

func openRTPDebug(sessionID string, opt RTPDebugOptions, log logger.Logger, onStop func(RTPReport)) (*rtpDebug, error) {
	sessionDir := filepath.Join(opt.Dir, sessionID)
	if err := os.MkdirAll(sessionDir, 0o755); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrRTPDebugDir, err)
	}
	d := &rtpDebug{
		sessionID:  sessionID,
		sessionDir: sessionDir,
		opt:        opt,
		log:        log,
		onStop:     onStop,
		queue:      make(chan rtpObservation, rtpDebugQueueSize),
		stopCh:     make(chan struct{}),
		finished:   make(chan struct{}),
	}
	if opt.Capture {
		inDir := filepath.Join(sessionDir, "inbound")
		if err := os.MkdirAll(inDir, 0o755); err != nil {
			return nil, fmt.Errorf("%w: %v", ErrRTPDebugDir, err)
		}
		meta, err := os.OpenFile(filepath.Join(inDir, "metadata.jsonl"), os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
		if err != nil {
			return nil, fmt.Errorf("%w: %v", ErrRTPDebugDir, err)
		}
		payload, err := os.OpenFile(filepath.Join(inDir, "payload.bin"), os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
		if err != nil {
			_ = meta.Close()
			return nil, fmt.Errorf("%w: %v", ErrRTPDebugDir, err)
		}
		d.meta = meta
		d.payload = payload
		d.metaPath = meta.Name()
		d.binPath = payload.Name()
	}
	d.alive.Store(true)
	return d, nil
}

func (d *rtpDebug) loop() {
	defer d.finish()
	timer := time.NewTimer(d.opt.Duration)
	defer timer.Stop()
	for {
		select {
		case <-d.stopCh:
			d.drain()
			return
		case <-timer.C:
			d.stop("time")
			d.drain()
			return
		case obs := <-d.queue:
			if !d.consume(obs) {
				d.drain()
				return
			}
		}
	}
}

func (d *rtpDebug) drain() {
	for {
		select {
		case obs := <-d.queue:
			if !d.consume(obs) {
				return
			}
		default:
			return
		}
	}
}

func (d *rtpDebug) consume(obs rtpObservation) bool {
	d.mu.Lock()
	if d.stored >= d.opt.MaxPackets {
		d.mu.Unlock()
		d.stop("packets")
		return false
	}
	next := d.bytes + len(obs.payload)
	if d.opt.Capture && len(obs.payload) > 0 && next > d.opt.MaxBytes {
		d.mu.Unlock()
		d.stop("bytes")
		return false
	}
	offset := d.bytes
	d.samples = append(d.samples, obs.sample)
	d.stored++
	if d.opt.Capture {
		d.bytes = next
	}
	d.mu.Unlock()
	if !d.writeCapture(obs.sample, obs.payload, offset) {
		return false
	}
	d.logObs(obs.sample)
	return true
}

func (d *rtpDebug) writeCapture(sample debugPacket, payload []byte, offset int) bool {
	if !d.opt.Capture || d.meta == nil || d.payload == nil {
		return true
	}
	if len(payload) > 0 {
		if _, err := d.payload.Write(payload); err != nil {
			d.stop("write")
			return false
		}
	}
	line, err := json.Marshal(uplinkMeta{
		SequenceNumber: sample.Seq,
		Timestamp:      sample.Timestamp,
		PayloadType:    sample.PT,
		SSRC:           sample.SSRC,
		Marker:         sample.Marker,
		PayloadLength:  sample.Length,
		PayloadOffset:  offset,
		PayloadSHA256:  sample.Hash,
	})
	if err != nil {
		d.stop("write")
		return false
	}
	if _, err := d.meta.Write(append(line, '\n')); err != nil {
		d.stop("write")
		return false
	}
	return true
}

func (d *rtpDebug) logObs(sample debugPacket) {
	if d.log == nil || !d.opt.Log {
		return
	}
	targets := make([]string, 0, len(sample.Out))
	for _, out := range sample.Out {
		if out.Target != "" {
			targets = append(targets, out.Target)
		}
	}
	d.log.Info("RTP_UPLINK",
		"session_id", d.sessionID,
		"source", sample.Source,
		"target", strings.Join(targets, ","),
		"track_id", sample.TrackID,
		"direction", "uplink",
		"payload_type", sample.PT,
		"sequence_number", sample.Seq,
		"timestamp", sample.Timestamp,
		"ssrc", sample.SSRC,
		"marker", sample.Marker,
		"payload_length", sample.Length,
		"payload_sha256", sample.Hash,
		"payload_prefix", sample.Prefix,
	)
	for _, out := range sample.Out {
		verdict, rewritten := classify(sample, out)
		d.log.Info("RTP_FORWARD",
			"session_id", d.sessionID,
			"source", sample.Source,
			"target", out.Target,
			"track_id", sample.TrackID,
			"direction", "forward",
			"packet_id", sample.ID,
			"verdict", verdict,
			"payload_type", sample.PT,
			"out_payload_type", out.PT,
			"sequence_number", sample.Seq,
			"out_sequence_number", out.Seq,
			"timestamp", sample.Timestamp,
			"out_timestamp", out.Timestamp,
			"ssrc", sample.SSRC,
			"out_ssrc", out.SSRC,
			"payload_length", sample.Length,
			"out_payload_length", out.Length,
			"payload_sha256", sample.Hash,
			"out_payload_sha256", out.Hash,
			"header_rewritten", rewritten,
		)
	}
}

func (d *rtpDebug) observe(raw []byte, source, trackID string, codec webrtc.RTPCodecCapability, subs []rtpSubscriber, writeErr error) {
	if d == nil || !d.alive.Load() {
		return
	}
	var pkt rtp.Packet
	if err := pkt.Unmarshal(raw); err != nil {
		return
	}
	hash := payloadSHA256(pkt.Payload)
	sample := debugPacket{
		ID:        d.nextID.Add(1),
		Source:    source,
		TrackID:   trackID,
		PT:        pkt.PayloadType,
		Seq:       pkt.SequenceNumber,
		Timestamp: pkt.Timestamp,
		SSRC:      pkt.SSRC,
		Marker:    pkt.Marker,
		Length:    len(pkt.Payload),
		Hash:      hash,
		Prefix:    payloadPrefix(pkt.Payload),
	}
	if len(subs) == 0 {
		sample.Out = []debugOut{{Seq: sample.Seq, Timestamp: sample.Timestamp, Marker: sample.Marker, Length: sample.Length, Hash: hash, OK: false}}
	} else {
		for _, sub := range subs {
			pt, ssrc, bound := senderSnapshot(sub.sender, codec)
			sample.Out = append(sample.Out, debugOut{
				Target:    sub.id,
				PT:        pt,
				Seq:       sample.Seq,
				Timestamp: sample.Timestamp,
				SSRC:      ssrc,
				Marker:    sample.Marker,
				Length:    sample.Length,
				Hash:      hash,
				OK:        writeErr == nil && bound,
			})
		}
	}
	var body []byte
	if d.opt.Capture {
		body = append([]byte(nil), pkt.Payload...)
	}
	d.enqueue(rtpObservation{sample: sample, payload: body})
}

func (d *rtpDebug) enqueue(obs rtpObservation) {
	select {
	case <-d.stopCh:
		return
	default:
	}
	select {
	case <-d.stopCh:
	case d.queue <- obs:
	default:
		d.dropped.Add(1)
	}
}

func senderSnapshot(sender *webrtc.RTPSender, codec webrtc.RTPCodecCapability) (uint8, uint32, bool) {
	if sender == nil {
		return 0, 0, false
	}
	params := sender.GetParameters()
	if len(params.Encodings) == 0 || params.Encodings[0].SSRC == 0 {
		return 0, 0, false
	}
	ssrc := uint32(params.Encodings[0].SSRC)
	if params.Encodings[0].PayloadType != 0 {
		return uint8(params.Encodings[0].PayloadType), ssrc, true
	}
	name := codecName(codec)
	for _, item := range params.Codecs {
		if codecName(item.RTPCodecCapability) == name && (codec.ClockRate == 0 || item.ClockRate == codec.ClockRate) {
			return uint8(item.PayloadType), ssrc, true
		}
	}
	return 0, ssrc, true
}

func (d *rtpDebug) stop(reason string) {
	d.stopOnce.Do(func() {
		d.mu.Lock()
		if d.reason == "" {
			d.reason = reason
		}
		d.mu.Unlock()
		close(d.stopCh)
	})
}

func (d *rtpDebug) finish() {
	d.endOnce.Do(func() {
		d.alive.Store(false)
		d.mu.Lock()
		samples := append([]debugPacket(nil), d.samples...)
		reason := d.reason
		if reason == "" {
			reason = "stop"
		}
		d.reason = reason
		d.mu.Unlock()
		report := buildReport(d.sessionID, d.sessionDir, reason, false, int(d.dropped.Load()), samples)
		d.writeReport(report)
		d.closeFiles()
		d.mu.Lock()
		d.final = &report
		d.mu.Unlock()
		if d.log != nil {
			d.log.Info("rtp debug stop", "session_id", d.sessionID, "reason", reason, "packets", len(samples), "dropped", d.dropped.Load())
		}
		if d.onStop != nil {
			d.onStop(report)
		}
		close(d.finished)
	})
}

func (d *rtpDebug) writeReport(report RTPReport) {
	if d.sessionDir == "" {
		return
	}
	_ = os.WriteFile(filepath.Join(d.sessionDir, "report.txt"), []byte(report.Text), 0o644)
}

func (d *rtpDebug) closeFiles() {
	if d.meta != nil {
		_ = d.meta.Close()
		d.meta = nil
	}
	if d.payload != nil {
		_ = d.payload.Close()
		d.payload = nil
	}
}

func (d *rtpDebug) wait(limit time.Duration) RTPReport {
	timer := time.NewTimer(limit)
	defer timer.Stop()
	select {
	case <-d.finished:
	case <-timer.C:
	}
	return d.snapshot()
}

func (d *rtpDebug) snapshot() RTPReport {
	d.mu.Lock()
	final := d.final
	samples := append([]debugPacket(nil), d.samples...)
	reason := d.reason
	d.mu.Unlock()
	if final != nil {
		copied := *final
		return copied
	}
	return buildReport(d.sessionID, d.sessionDir, reason, d.alive.Load(), int(d.dropped.Load()), samples)
}

func payloadSHA256(payload []byte) string {
	sum := sha256.Sum256(payload)
	return hex.EncodeToString(sum[:])
}

func payloadPrefix(payload []byte) string {
	if len(payload) > 16 {
		payload = payload[:16]
	}
	return hex.EncodeToString(payload)
}

// ReadUplinkCapture 按序号读回抓到的 Payload。文件里的顺序可以是乱的。
func ReadUplinkCapture(sessionDir string) ([]CapturedUplink, error) {
	inDir := filepath.Join(sessionDir, "inbound")
	metaRaw, err := os.ReadFile(filepath.Join(inDir, "metadata.jsonl"))
	if err != nil {
		return nil, err
	}
	body, err := os.ReadFile(filepath.Join(inDir, "payload.bin"))
	if err != nil {
		return nil, err
	}
	var items []uplinkMeta
	for _, line := range strings.Split(string(metaRaw), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var item uplinkMeta
		if err := json.Unmarshal([]byte(line), &item); err != nil {
			return nil, err
		}
		items = append(items, item)
	}
	type ordered struct {
		ext  int64
		item CapturedUplink
	}
	var prev int64
	var has bool
	rows := make([]ordered, 0, len(items))
	for _, item := range items {
		if item.PayloadOffset < 0 || item.PayloadLength < 0 || item.PayloadOffset+item.PayloadLength > len(body) {
			return nil, errors.New("capture payload is out of range")
		}
		ext := int64(item.SequenceNumber)
		if has {
			ext = prev + int64(int16(item.SequenceNumber-uint16(prev)))
		}
		prev = ext
		has = true
		payload := append([]byte(nil), body[item.PayloadOffset:item.PayloadOffset+item.PayloadLength]...)
		rows = append(rows, ordered{ext: ext, item: CapturedUplink{
			SequenceNumber: item.SequenceNumber,
			Timestamp:      item.Timestamp,
			PayloadType:    item.PayloadType,
			SSRC:           item.SSRC,
			Payload:        payload,
			SHA256:         item.PayloadSHA256,
		}})
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].ext < rows[j].ext })
	out := make([]CapturedUplink, len(rows))
	for i := range rows {
		out[i] = rows[i].item
	}
	return out, nil
}
