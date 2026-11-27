package sfu

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/pion/rtp"
	"github.com/pion/webrtc/v4"

	"iot-server/internal/logger"
)

func TestDebugOffDoesNothing(t *testing.T) {
	var room Room
	room.noteDeviceUplink(nil, nil, []byte{1, 2, 3}, nil)
}

func TestAppSubscribersIgnoreDevice(t *testing.T) {
	track := &forwardTrack{}
	track.addSub("call:u1", nil)
	track.addSub("call:u1", nil)
	track.addSub("call:9030d6cb2703", nil)
	track.addSub("viewer:1", nil)
	subs := track.appSubscribers()
	if len(subs) != 1 || subs[0].id != "call:u1" {
		t.Fatalf("subs = %+v", subs)
	}
}

func TestClassifyPayloadAndHeader(t *testing.T) {
	in := debugPacket{PT: 9, Seq: 7, Timestamp: 160, SSRC: 11, Length: 4, Hash: "aaa"}
	same := debugOut{Target: "call:u1", PT: 9, Seq: 7, Timestamp: 160, SSRC: 99, Length: 4, Hash: "aaa", OK: true}
	verdict, rewritten := classify(in, same)
	if verdict != "IDENTICAL" || !rewritten {
		t.Fatalf("verdict = %s rewritten = %v", verdict, rewritten)
	}
	changed := same
	changed.Hash = "bbb"
	verdict, _ = classify(in, changed)
	if verdict != "PAYLOAD_CHANGED" {
		t.Fatalf("verdict = %s", verdict)
	}
	longer := same
	longer.Length = 5
	longer.Hash = "ccc"
	verdict, _ = classify(in, longer)
	if verdict != "PAYLOAD_LENGTH_CHANGED" {
		t.Fatalf("verdict = %s", verdict)
	}
	missing := same
	missing.OK = false
	verdict, _ = classify(in, missing)
	if verdict != "MISSING_OUTBOUND" {
		t.Fatalf("verdict = %s", verdict)
	}
}

func TestOneUplinkComparedPerApp(t *testing.T) {
	sample := debugPacket{
		ID: 7, PT: 9, Length: 4, Hash: "aaa",
		Out: []debugOut{
			{Target: "call:u1", PT: 111, SSRC: 1, Length: 4, Hash: "aaa", OK: true},
			{Target: "call:u2", PT: 9, SSRC: 2, Length: 4, Hash: "aaa", OK: true},
		},
	}
	got := comparisons(sample)
	if len(got) != 2 || got[0].PacketID != 7 || got[1].PacketID != 7 || got[0].Target != "call:u1" || got[1].Target != "call:u2" {
		t.Fatalf("comparisons = %+v", got)
	}
	if got[0].Verdict != "IDENTICAL" || got[1].Verdict != "IDENTICAL" {
		t.Fatalf("verdicts = %s %s", got[0].Verdict, got[1].Verdict)
	}
	report := buildReport("9030d6cb2703", "", "stop", false, 0, []debugPacket{sample, {
		ID: 8, PT: 9, Seq: 2, Length: 4, Hash: "aaa",
		Out: []debugOut{{Target: "call:u1", Length: 4, Hash: "zzz", OK: true}},
	}})
	if report.Targets != 2 || report.Identical != 2 || report.PayloadChanged != 1 || report.HeaderRewritten != 2 {
		t.Fatalf("report = %+v", report)
	}
	if report.G722Confirmed || !strings.Contains(report.Text, "是不是G.722：不能") {
		t.Fatalf("text = %s", report.Text)
	}
}

func TestSequenceAndTimestamp(t *testing.T) {
	loss, dup, reorder := sequenceStats([]debugPacket{{Seq: 1}, {Seq: 2}, {Seq: 4}})
	if loss != 1 || dup != 0 || reorder != 0 {
		t.Fatalf("gap = %d %d %d", loss, dup, reorder)
	}
	loss, dup, reorder = sequenceStats([]debugPacket{{Seq: 1}, {Seq: 3}, {Seq: 2}})
	if loss != 0 || dup != 0 || reorder != 1 {
		t.Fatalf("reorder = %d %d %d", loss, dup, reorder)
	}
	loss, dup, reorder = sequenceStats([]debugPacket{{Seq: 1}, {Seq: 2}, {Seq: 2}, {Seq: 3}})
	if loss != 0 || dup != 1 || reorder != 0 {
		t.Fatalf("dup = %d %d %d", loss, dup, reorder)
	}
	loss, _, _ = sequenceStats([]debugPacket{{Seq: 65534}, {Seq: 65535}, {Seq: 0}, {Seq: 1}})
	if loss != 0 {
		t.Fatalf("wrap loss = %d", loss)
	}
	loss, _, _ = sequenceStats([]debugPacket{{Seq: 65534}, {Seq: 65535}, {Seq: 0}, {Seq: 2}})
	if loss != 1 {
		t.Fatalf("wrap gap = %d", loss)
	}

	samples := []debugPacket{
		{PT: 9, Seq: 1, Timestamp: 0, Length: 80, Hash: "a", Out: []debugOut{{Target: "call:u1", PT: 9, Seq: 1, Length: 80, Hash: "a", OK: true}}},
		{PT: 9, Seq: 2, Timestamp: 160, Length: 80, Hash: "a", Out: []debugOut{{Target: "call:u1", PT: 9, Seq: 2, SSRC: 5, Length: 80, Hash: "a", OK: true}}},
		{PT: 9, Seq: 3, Timestamp: 320, Length: 160, Hash: "a", Out: []debugOut{{Target: "call:u1", PT: 9, Seq: 3, SSRC: 5, Length: 160, Hash: "a", OK: true}}},
	}
	report := buildReport("9030d6cb2703", "", "stop", false, 0, samples)
	if !report.PTIs9 || !report.TimestampMatchesG722 || report.TimestampStep160 != 2 || report.Loss != 0 {
		t.Fatalf("report = %+v", report)
	}
	if report.PayloadLengthMin != 80 || report.PayloadLengthMax != 160 || report.PayloadLengthAvg != 320.0/3 {
		t.Fatalf("length = %d %d %f", report.PayloadLengthMin, report.PayloadLengthMax, report.PayloadLengthAvg)
	}
	if !report.PayloadIdentical || !report.ForwardComplete || report.HeaderRewritten != 2 {
		t.Fatalf("forward = %+v", report)
	}
	if !strings.Contains(report.Text, "PT是否为9：是") {
		t.Fatalf("text = %s", report.Text)
	}
}

func TestUplinkLogRecordsPT9(t *testing.T) {
	dir := t.TempDir()
	var buf bytes.Buffer
	log, err := logger.New(&buf, "info")
	if err != nil {
		t.Fatal(err)
	}
	d := startTestDebug(t, dir, RTPDebugOptions{Log: true, Dir: dir, Duration: time.Hour, MaxPackets: 10}, log)
	payload := bytes.Repeat([]byte{0x11}, 80)
	d.observe(mustRTP(t, rtp.Header{PayloadType: 9, SequenceNumber: 1001, Timestamp: 160000, SSRC: 123456}, payload), "call:9030d6cb2703", "audio", g722Codec(), nil, nil)
	deadline := time.Now().Add(time.Second)
	for {
		d.mu.Lock()
		stored := d.stored
		d.mu.Unlock()
		if stored == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("packet was not recorded")
		}
		time.Sleep(5 * time.Millisecond)
	}
	d.stop("stop")
	_ = d.wait(time.Second)
	text := buf.String()
	sum := sha256.Sum256(payload)
	hash := hex.EncodeToString(sum[:])
	if !strings.Contains(text, "RTP_UPLINK") || !strings.Contains(text, "payload_type=9") || !strings.Contains(text, "sequence_number=1001") || !strings.Contains(text, "payload_sha256="+hash) {
		t.Fatalf("log = %s", text)
	}
	if !strings.Contains(text, "RTP_FORWARD") || !strings.Contains(text, "verdict=MISSING_OUTBOUND") {
		t.Fatalf("log = %s", text)
	}
	if strings.Contains(text, hex.EncodeToString(payload)) {
		t.Fatal("log included the full payload")
	}
}

func TestCaptureStopsAndRestoresOrder(t *testing.T) {
	dir := t.TempDir()
	d := startTestDebug(t, dir, RTPDebugOptions{Capture: true, Dir: dir, Duration: time.Hour, MaxPackets: 2, MaxBytes: 1 << 20}, nil)
	d.observe(mustRTP(t, rtp.Header{PayloadType: 9, SequenceNumber: 2, Timestamp: 320, SSRC: 9}, []byte("BBBB")), "call:9030d6cb2703", "audio", g722Codec(), nil, nil)
	d.observe(mustRTP(t, rtp.Header{PayloadType: 9, SequenceNumber: 1, Timestamp: 160, SSRC: 9}, []byte("AAAA")), "call:9030d6cb2703", "audio", g722Codec(), nil, nil)
	d.observe(mustRTP(t, rtp.Header{PayloadType: 9, SequenceNumber: 3, Timestamp: 480, SSRC: 9}, []byte("CCCC")), "call:9030d6cb2703", "audio", g722Codec(), nil, nil)
	report := waitDebug(t, d)
	if report.Packets != 2 || report.Reason != "packets" || report.Running {
		t.Fatalf("report = %+v", report)
	}
	got, err := ReadUplinkCapture(d.sessionDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || string(got[0].Payload) != "AAAA" || string(got[1].Payload) != "BBBB" || got[0].PayloadType != 9 {
		t.Fatalf("capture = %+v", got)
	}
	if got[0].SHA256 != payloadSHA256(got[0].Payload) {
		t.Fatal("hash does not match payload bytes")
	}
	raw, err := os.ReadFile(d.binPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(raw) != "BBBBAAAA" {
		t.Fatalf("file = %q", raw)
	}
}

func TestCaptureStopsOnSizeAndTime(t *testing.T) {
	dir := t.TempDir()
	sized := startTestDebug(t, dir, RTPDebugOptions{Capture: true, Dir: dir, Duration: time.Hour, MaxPackets: 10, MaxBytes: 10}, nil)
	sized.observe(mustRTP(t, rtp.Header{PayloadType: 9, SequenceNumber: 1}, bytes.Repeat([]byte{1}, 8)), "call:9030d6cb2703", "audio", g722Codec(), nil, nil)
	sized.observe(mustRTP(t, rtp.Header{PayloadType: 9, SequenceNumber: 2}, bytes.Repeat([]byte{2}, 8)), "call:9030d6cb2703", "audio", g722Codec(), nil, nil)
	report := waitDebug(t, sized)
	if report.Packets != 1 || report.Reason != "bytes" {
		t.Fatalf("report = %+v", report)
	}

	timed := startTestDebug(t, t.TempDir(), RTPDebugOptions{Log: true, Dir: t.TempDir(), Duration: 30 * time.Millisecond, MaxPackets: 10}, nil)
	report = waitDebug(t, timed)
	if report.Reason != "time" || report.Packets != 0 {
		t.Fatalf("report = %+v", report)
	}
}

func TestQueueDropsWithoutBlocking(t *testing.T) {
	d := &rtpDebug{
		queue:    make(chan rtpObservation, 1),
		stopCh:   make(chan struct{}),
		finished: make(chan struct{}),
		opt:      RTPDebugOptions{Log: true, MaxPackets: 10},
	}
	start := time.Now()
	for i := 0; i < 1000; i++ {
		d.enqueue(rtpObservation{})
	}
	if time.Since(start) > 200*time.Millisecond {
		t.Fatalf("enqueue took %s", time.Since(start))
	}
	if d.dropped.Load() != 999 {
		t.Fatalf("dropped = %d", d.dropped.Load())
	}
}

func TestRoomCloseStopsCapture(t *testing.T) {
	dir := t.TempDir()
	d := startTestDebug(t, dir, RTPDebugOptions{Capture: true, Dir: dir, Duration: time.Hour, MaxPackets: 10}, nil)
	d.observe(mustRTP(t, rtp.Header{PayloadType: 9, SequenceNumber: 1}, []byte("AAAA")), "call:9030d6cb2703", "audio", g722Codec(), nil, nil)
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		d.mu.Lock()
		n := d.stored
		d.mu.Unlock()
		if n == 1 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	d.stop("room_closed")
	report := d.wait(time.Second)
	if report.Reason != "room_closed" || report.Packets != 1 || d.payload != nil {
		t.Fatalf("report = %+v payload closed = %v", report, d.payload == nil)
	}
	info, err := os.Stat(d.binPath)
	if err != nil {
		t.Fatal(err)
	}
	d.observe(mustRTP(t, rtp.Header{PayloadType: 9, SequenceNumber: 2}, []byte("BBBB")), "call:9030d6cb2703", "audio", g722Codec(), nil, nil)
	time.Sleep(20 * time.Millisecond)
	again, err := os.Stat(d.binPath)
	if err != nil {
		t.Fatal(err)
	}
	if again.Size() != info.Size() {
		t.Fatalf("size %d -> %d", info.Size(), again.Size())
	}
	if _, err := os.Stat(d.sessionDir + "/report.txt"); err != nil {
		t.Fatal(err)
	}
}

func startTestDebug(t *testing.T, dir string, opt RTPDebugOptions, log logger.Logger) *rtpDebug {
	t.Helper()
	opt.Dir = dir
	normalized, err := normalizeRTPDebug(opt)
	if err != nil {
		t.Fatal(err)
	}
	d, err := openRTPDebug("9030d6cb2703", normalized, log, nil)
	if err != nil {
		t.Fatal(err)
	}
	go d.loop()
	t.Cleanup(func() {
		d.stop("stop")
		d.wait(time.Second)
	})
	return d
}

func waitDebug(t *testing.T, d *rtpDebug) RTPReport {
	t.Helper()
	select {
	case <-d.finished:
	case <-time.After(2 * time.Second):
		t.Fatal("debug did not stop")
	}
	return d.snapshot()
}

func mustRTP(t *testing.T, header rtp.Header, payload []byte) []byte {
	t.Helper()
	raw, err := (&rtp.Packet{Header: header, Payload: payload}).Marshal()
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func g722Codec() webrtc.RTPCodecCapability {
	return webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeG722, ClockRate: 8000}
}
