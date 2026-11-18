//go:build integration

package integration

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
)

func TestP2ControlPlane(t *testing.T) {
	secrets := loadSecrets(t)
	root := moduleRoot(t)
	bin := filepath.Join(t.TempDir(), "server")
	build := exec.Command("go", "build", "-o", bin, "./cmd/server")
	build.Dir = root
	build.Env = append(os.Environ(), "GOTOOLCHAIN=local")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build failed: %s", scrub(string(out), secrets))
	}
	logPath := filepath.Join(t.TempDir(), "server.log")
	stop := startServer(t, bin, logPath, map[string]string{
		"IOT_HTTP_ADDR":            "127.0.0.1:18080",
		"IOT_HOOK_ADDR":            hookListenAddr(),
		"IOT_SFU_ADDR":             "127.0.0.1:18090",
		"IOT_ENABLE_TEST_CONTROLS": "1",
		"IOT_INSTANCE_ID":          "iot-dev-1",
		"IOT_STOP_DELAY":           "1s",
		"IOT_VIEWER_JOIN_GRACE":    "2s",
		"IOT_CALL_RING_TIMEOUT":    "2s",
	}, secrets)
	t.Cleanup(stop)
	base := "http://127.0.0.1:18080"
	hook := "http://" + hookListenAddr()
	waitReady(t, base)
	admin := login(t, base, "admin", os.Getenv("IOT_SEED_ADMIN_PASSWORD"), secrets)
	operator := login(t, base, "operator", os.Getenv("IOT_SEED_OPERATOR_PASSWORD"), secrets)
	deviceID := newDevice(t, base, admin)
	ticket := issueTicket(t, base, admin, deviceID)
	secrets = append(secrets, ticket)
	if postJSON(t, base+"/api/v1/devices/"+deviceID+"/watch", operator, watchBody(ticket, "phone", "w1"), nil) != http.StatusForbidden {
		t.Fatal("ticket holder without view was allowed")
	}
	grant(t, base, admin, deviceID, "operator", "view")
	first := doWatch(t, base, operator, deviceID, ticket, "phone", "w1")
	if !first.Credential.Connectable || first.Credential.Provider != "pion" || first.SessionStatus == "ACTIVE" {
		t.Fatalf("watch credential provider=%s connectable=%t status=%s", first.Credential.Provider, first.Credential.Connectable, first.SessionStatus)
	}
	replay := doWatch(t, base, operator, deviceID, ticket, "phone", "w1")
	if !replay.Replayed && replay.ViewerID != first.ViewerID {
		t.Fatal("idempotent watch changed viewer")
	}
	if replay.ViewerID != first.ViewerID {
		t.Fatal("replay viewer mismatch")
	}
	if !replay.Credential.Connectable || replay.Credential.Provider != "pion" {
		t.Fatal("replay credential is not connectable")
	}
	second := doWatch(t, base, operator, deviceID, ticket, "desk", "w2")
	if second.ViewerID == first.ViewerID || second.SessionID != first.SessionID {
		t.Fatal("second client did not share the session")
	}
	waitSession(t, base, operator, first.SessionID, func(view sessionView) bool {
		return view.DeviceReady && !view.MediaReady && view.Status != "ACTIVE"
	})
	postEvent(t, hook, secrets, first.SessionID, first.Generation, "media.track_published", "device:"+deviceID)
	waitSession(t, base, operator, first.SessionID, func(view sessionView) bool {
		return view.Status == "ACTIVE" && view.MediaReady && view.Occupants >= 1
	})
	db := openDB(t)
	defer db.Close()
	var resourceID string
	if err := db.QueryRow(`SELECT provider_resource_id FROM rtc_media_resource WHERE tenant_id = 'dev' AND session_id = ?`, first.SessionID).Scan(&resourceID); err != nil {
		t.Fatal(err)
	}
	if resourceID == "" || resourceID != first.Credential.ResourceID || !strings.HasPrefix(resourceID, "pion-") {
		t.Fatalf("media resource %s", resourceID)
	}
	sharedID := "s" + deviceID
	if _, err := db.Exec(`
		INSERT INTO rtc_media_resource (tenant_id, session_id, generation, provider, resource_type, provider_resource_id)
		VALUES ('dev', ?, 9, 'test', 'room', ?)`, sharedID, resourceID); err != nil {
		t.Fatal(err)
	}
	var presence map[string]any
	getJSON(t, base+"/api/v1/devices/"+deviceID+"/status", operator, &presence)
	if presence["track_ready"] != true || presence["rtc_connected"] != false || presence["mqtt_online"] != false {
		t.Fatalf("presence mixed %v", presence)
	}
	postEvent(t, hook, secrets, first.SessionID, first.Generation, "media.participant_joined", "device:"+deviceID)
	getJSON(t, base+"/api/v1/devices/"+deviceID+"/status", operator, &presence)
	if presence["rtc_connected"] != true {
		t.Fatal("device participant did not set rtc_connected")
	}
	if postJSON(t, base+"/api/v1/viewers/"+first.ViewerID+"/heartbeat", operator, map[string]any{}, nil) != http.StatusOK {
		t.Fatal("heartbeat failed")
	}
	time.Sleep(700 * time.Millisecond)
	view := getSession(t, base, operator, first.SessionID)
	if view.Status != "ACTIVE" {
		t.Fatal("joining viewer did not keep the stream")
	}
	if postJSON(t, base+"/api/v1/viewers/"+first.ViewerID+"/leave", operator, map[string]any{}, nil) != http.StatusOK {
		t.Fatal("leave failed")
	}
	if postJSON(t, base+"/api/v1/viewers/"+first.ViewerID+"/leave", operator, map[string]any{}, nil) != http.StatusOK {
		t.Fatal("second leave failed")
	}
	if postJSON(t, base+"/api/v1/viewers/"+first.ViewerID+"/heartbeat", operator, map[string]any{}, nil) != http.StatusConflict {
		t.Fatal("heartbeat revived a left viewer")
	}
	if postJSON(t, base+"/api/v1/viewers/"+second.ViewerID+"/leave", operator, map[string]any{}, nil) != http.StatusOK {
		t.Fatal("second viewer leave failed")
	}
	waitSession(t, base, operator, first.SessionID, func(view sessionView) bool { return view.Status == "CLOSED" })
	var leftShared int
	if err := db.QueryRow(`SELECT COUNT(*) FROM rtc_media_resource WHERE tenant_id = 'dev' AND provider_resource_id = ?`, resourceID).Scan(&leftShared); err != nil || leftShared == 0 {
		t.Fatal("shared media resource was removed")
	}
	getJSON(t, base+"/api/v1/devices/"+deviceID+"/status", operator, &presence)
	if presence["track_ready"] != false {
		t.Fatal("closed session left track_ready set")
	}

	next := doWatch(t, base, operator, deviceID, ticket, "phone", "w3")
	if next.Generation <= first.Generation {
		t.Fatal("new round did not increase generation")
	}
	before := getSession(t, base, operator, next.SessionID)
	closedBefore := getSession(t, base, operator, first.SessionID)
	if before.MediaReady || before.Status == "ACTIVE" || closedBefore.Status != "CLOSED" {
		t.Fatal("new session was already publishing")
	}
	// track_published 会把媒体标成就绪。旧轮次打到新会话上不能产生这个变化。
	postEvent(t, hook, secrets, next.SessionID, first.Generation, "media.track_published", "stale-track")
	postEvent(t, hook, secrets, first.SessionID, first.Generation, "media.track_published", "stale-old")
	got := getSession(t, base, operator, next.SessionID)
	closed := getSession(t, base, operator, first.SessionID)
	// 设备启动任务可以让新会话离开 WAITING_DEVICE。旧轨道事件不能把它标成媒体就绪或 ACTIVE。
	if got.MediaReady || got.Status == "ACTIVE" || got.Status == "CLOSED" || got.Status == "FAILED" || got.Generation != next.Generation || closed.Status != closedBefore.Status || closed.MediaReady != closedBefore.MediaReady || closed.Generation != closedBefore.Generation {
		t.Fatalf("old event changed the current session: status=%s media=%t generation=%d closed=%s closed_media=%t", got.Status, got.MediaReady, got.Generation, closed.Status, closed.MediaReady)
	}
	postEvent(t, hook, secrets, next.SessionID, next.Generation, "media.track_published", "track")
	waitSession(t, base, operator, next.SessionID, func(view sessionView) bool { return view.Status == "ACTIVE" })
	fresh := issueTicketRotate(t, base, admin, deviceID)
	secrets = append(secrets, fresh)
	if postJSON(t, base+"/api/v1/viewers/"+next.ViewerID+"/heartbeat", operator, map[string]any{}, nil) != http.StatusOK {
		t.Fatal("rotate ended the current viewer")
	}
	if got := getSession(t, base, operator, next.SessionID); got.Status != "ACTIVE" || got.Generation != next.Generation {
		t.Fatal("rotate changed the current session")
	}
	if postJSON(t, base+"/api/v1/devices/"+deviceID+"/watch", operator, watchBody(ticket, "phone", "old-during"), nil) != http.StatusForbidden {
		t.Fatal("retired ticket was accepted during an open session")
	}
	leave(t, base, operator, next.ViewerID)

	rotated := issueTicketRotate(t, base, admin, deviceID)
	secrets = append(secrets, rotated)
	if postJSON(t, base+"/api/v1/devices/"+deviceID+"/watch", operator, watchBody(ticket, "phone", "old"), nil) != http.StatusForbidden {
		t.Fatal("retired ticket was accepted")
	}
	_ = doWatch(t, base, operator, deviceID, rotated, "phone", "w4")

	sharedDevice := newDevice(t, base, admin)
	sharedTicket := issueTicket(t, base, admin, sharedDevice)
	secrets = append(secrets, sharedTicket)
	grant(t, base, admin, sharedDevice, "operator", "view")
	var sessions sync.Map
	var group sync.WaitGroup
	for i := 0; i < 6; i++ {
		group.Add(1)
		go func(i int) {
			defer group.Done()
			watched := doWatch(t, base, operator, sharedDevice, sharedTicket, fmt.Sprintf("c%d", i), fmt.Sprintf("k%d", i))
			sessions.Store(watched.SessionID, true)
		}(i)
	}
	group.Wait()
	count := 0
	sessions.Range(func(_, _ any) bool { count++; return true })
	if count != 1 {
		t.Fatalf("concurrent watches created %d sessions", count)
	}

	callDevice := newDevice(t, base, admin)
	callTicket := issueTicket(t, base, admin, callDevice)
	secrets = append(secrets, callTicket)
	grant(t, base, admin, callDevice, "operator", "call")
	if postJSON(t, base+"/api/v1/calls/missing/accept", operator, map[string]any{}, nil) != http.StatusNotFound {
		t.Fatal("missing call was not rejected")
	}
	started := doCall(t, base, admin, callDevice, callTicket)
	if postJSON(t, base+"/api/v1/calls/"+started.CallID+"/accept", admin, map[string]any{}, nil) != http.StatusForbidden {
		t.Fatal("caller accepted the call")
	}
	if postJSON(t, base+"/api/v1/calls/"+started.CallID+"/accept", operator, map[string]any{}, nil) != http.StatusOK {
		t.Fatal("answerer could not accept")
	}
	postEvent(t, hook, secrets, started.SessionID, started.Generation, "call.connected", started.CallID)
	waitCall(t, base, operator, started.CallID, "CONNECTED")
	retiredCall := callTicket
	callTicket = issueTicketRotate(t, base, admin, callDevice)
	secrets = append(secrets, callTicket)
	waitCall(t, base, operator, started.CallID, "CONNECTED")
	if postJSON(t, base+"/api/v1/devices/"+callDevice+"/calls", admin, callBody(retiredCall), nil) != http.StatusForbidden {
		t.Fatal("retired ticket opened a call during an existing call")
	}
	if postJSON(t, base+"/api/v1/calls/"+started.CallID+"/end", admin, map[string]any{}, nil) != http.StatusOK {
		t.Fatal("end failed")
	}
	busy := make(chan int, 2)
	go func() {
		busy <- postJSON(t, base+"/api/v1/devices/"+callDevice+"/calls", admin, callBody(callTicket), nil)
	}()
	go func() {
		busy <- postJSON(t, base+"/api/v1/devices/"+callDevice+"/calls", admin, callBody(callTicket), nil)
	}()
	a, b := <-busy, <-busy
	if (a == http.StatusCreated) == (b == http.StatusCreated) {
		t.Fatalf("concurrent calls statuses %d %d", a, b)
	}
	var openID string
	if err := db.QueryRow(`SELECT call_id FROM rtc_call_hold WHERE tenant_id = 'dev' AND device_id = ?`, callDevice).Scan(&openID); err != nil {
		t.Fatal("hold missing")
	}
	if _, err := db.Exec(`
		INSERT INTO rtc_task (tenant_id, task_id, task_type, business_id, generation, status, run_at)
		VALUES ('dev', ?, 'release_call', ?, 1, 'pending', UTC_TIMESTAMP(3))`, "t"+callDevice, "old"+callDevice); err != nil {
		t.Fatal(err)
	}
	time.Sleep(700 * time.Millisecond)
	var still string
	if err := db.QueryRow(`SELECT call_id FROM rtc_call_hold WHERE tenant_id = 'dev' AND device_id = ?`, callDevice).Scan(&still); err != nil || still != openID {
		t.Fatal("old release removed the new hold")
	}

	timeoutDevice := newDevice(t, base, admin)
	grant(t, base, admin, timeoutDevice, "operator", "call")
	timeoutTicket := issueTicket(t, base, admin, timeoutDevice)
	secrets = append(secrets, timeoutTicket)
	timed := doCall(t, base, admin, timeoutDevice, timeoutTicket)
	waitCall(t, base, admin, timed.CallID, "TIMEOUT")
	again := doCall(t, base, admin, timeoutDevice, timeoutTicket)
	if again.CallID == timed.CallID {
		t.Fatal("timeout did not release the device")
	}

	unknownDevice := newDevice(t, base, admin)
	unknownTicket := issueTicket(t, base, admin, unknownDevice)
	secrets = append(secrets, unknownTicket)
	grant(t, base, admin, unknownDevice, "operator", "view")
	putPlan(t, hook, secrets, unknownDevice, "stop", "unknown")
	unknownWatch := doWatch(t, base, operator, unknownDevice, unknownTicket, "phone", "u1")
	postEvent(t, hook, secrets, unknownWatch.SessionID, unknownWatch.Generation, "media.track_published", "track")
	waitSession(t, base, operator, unknownWatch.SessionID, func(view sessionView) bool { return view.Status == "ACTIVE" })
	leave(t, base, operator, unknownWatch.ViewerID)
	waitSession(t, base, operator, unknownWatch.SessionID, func(view sessionView) bool { return view.Status == "STOPPING" })
	if postJSON(t, base+"/api/v1/devices/"+unknownDevice+"/watch", operator, watchBody(unknownTicket, "phone", "u2"), nil) != http.StatusConflict {
		t.Fatal("watch started while stop result was unknown")
	}
	putPlan(t, hook, secrets, unknownDevice, "status", "success")
	waitSession(t, base, operator, unknownWatch.SessionID, func(view sessionView) bool { return view.Status == "CLOSED" })
	if postSDP(t, unknownWatch.Credential.WHEPURL, unknownWatch.Credential.Token, "v=0\r\n") != http.StatusNotFound {
		t.Fatal("closed session left the room")
	}

	brokenDevice := newDevice(t, base, admin)
	brokenTicket := issueTicket(t, base, admin, brokenDevice)
	secrets = append(secrets, brokenTicket)
	grant(t, base, admin, brokenDevice, "operator", "view")
	broken := doWatch(t, base, operator, brokenDevice, brokenTicket, "phone", "b1")
	postEvent(t, hook, secrets, broken.SessionID, broken.Generation, "media.track_published", "device:"+brokenDevice)
	waitSession(t, base, operator, broken.SessionID, func(view sessionView) bool {
		return view.Status == "ACTIVE" && view.MediaReady
	})
	postEvent(t, hook, secrets, broken.SessionID, broken.Generation, "media.track_unpublished", "device:"+brokenDevice)
	waitSession(t, base, operator, broken.SessionID, func(view sessionView) bool {
		return view.Status == "FAILED" && !view.MediaReady
	})
	getJSON(t, base+"/api/v1/devices/"+brokenDevice+"/status", operator, &presence)
	if presence["track_ready"] != false {
		t.Fatal("unpublished track left the device occupied")
	}
	if postSDP(t, broken.Credential.WHEPURL, broken.Credential.Token, "v=0\r\n") != http.StatusNotFound {
		t.Fatal("unpublished track left the room")
	}
	replaced := doWatch(t, base, operator, brokenDevice, brokenTicket, "phone", "b2")
	if replaced.SessionID == broken.SessionID || replaced.Generation <= broken.Generation {
		t.Fatal("unpublished track did not release the device")
	}
	postEvent(t, hook, secrets, replaced.SessionID, replaced.Generation, "media.track_published", "device:"+brokenDevice)
	waitSession(t, base, operator, replaced.SessionID, func(view sessionView) bool { return view.Status == "ACTIVE" && view.MediaReady })
	postEvent(t, hook, secrets, replaced.SessionID, broken.Generation, "media.room_closed", "device:"+brokenDevice)
	postEvent(t, hook, secrets, replaced.SessionID, broken.Generation, "media.track_unpublished", "device:"+brokenDevice)
	if got := getSession(t, base, operator, replaced.SessionID); got.Status != "ACTIVE" || !got.MediaReady || got.Generation != replaced.Generation {
		t.Fatalf("old media loss changed the current session: status=%s media=%t generation=%d", got.Status, got.MediaReady, got.Generation)
	}
	if postSDP(t, replaced.Credential.WHEPURL, replaced.Credential.Token, "v=0\r\n") == http.StatusNotFound {
		t.Fatal("old media loss removed the current room")
	}
	postEvent(t, hook, secrets, replaced.SessionID, replaced.Generation, "media.room_closed", "device:"+brokenDevice)
	waitSession(t, base, operator, replaced.SessionID, func(view sessionView) bool {
		return view.Status == "FAILED" && !view.MediaReady
	})
	if postSDP(t, replaced.Credential.WHEPURL, replaced.Credential.Token, "v=0\r\n") != http.StatusNotFound {
		t.Fatal("closed room stayed reachable")
	}
	if again := doWatch(t, base, operator, brokenDevice, brokenTicket, "phone", "b3"); again.Generation <= replaced.Generation {
		t.Fatal("room close did not release the device")
	}

	lostDevice := newDevice(t, base, admin)
	lostTicket := issueTicket(t, base, admin, lostDevice)
	secrets = append(secrets, lostTicket)
	grant(t, base, admin, lostDevice, "operator", "call")
	lost := doCall(t, base, admin, lostDevice, lostTicket)
	if postJSON(t, base+"/api/v1/calls/"+lost.CallID+"/accept", operator, map[string]any{}, nil) != http.StatusOK {
		t.Fatal("answerer could not accept the interrupted call")
	}
	postEvent(t, hook, secrets, lost.SessionID, lost.Generation, "call.connected", lost.CallID)
	waitCall(t, base, operator, lost.CallID, "CONNECTED")
	postEvent(t, hook, secrets, lost.SessionID, lost.Generation, "media.room_closed", "device:"+lostDevice)
	waitCall(t, base, operator, lost.CallID, "ENDED")
	waitSession(t, base, admin, lost.SessionID, func(view sessionView) bool { return view.Status == "FAILED" })
	restarted := doCall(t, base, admin, lostDevice, lostTicket)
	if restarted.CallID == lost.CallID {
		t.Fatal("room close did not release the call")
	}

	// 请求路径先握住设备锁，再改观看者。并发加入、退出和全局回收要在这把锁后面排好，不能互相卡住。
	staggerDevice := newDevice(t, base, admin)
	staggerTicket := issueTicket(t, base, admin, staggerDevice)
	secrets = append(secrets, staggerTicket)
	grant(t, base, admin, staggerDevice, "operator", "view")
	stagger := doWatch(t, base, operator, staggerDevice, staggerTicket, "phone", "s0")
	postEvent(t, hook, secrets, stagger.SessionID, stagger.Generation, "media.track_published", "device:"+staggerDevice)
	waitSession(t, base, operator, stagger.SessionID, func(view sessionView) bool {
		return view.Status == "ACTIVE" && view.MediaReady
	})
	if _, err := db.Exec(`UPDATE rtc_viewer SET heartbeat_at = DATE_SUB(UTC_TIMESTAMP(3), INTERVAL 1 HOUR) WHERE tenant_id = 'dev' AND viewer_id = ?`, stagger.ViewerID); err != nil {
		t.Fatal(err)
	}
	hold, err := db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	var locked int
	if err := hold.QueryRow(`SELECT 1 FROM device WHERE tenant_id = 'dev' AND device_id = ? FOR UPDATE`, staggerDevice).Scan(&locked); err != nil {
		_ = hold.Rollback()
		t.Fatal(err)
	}
	joined := make([]watchResult, 4)
	var staggerGroup sync.WaitGroup
	staggerGroup.Add(len(joined) + 1)
	for i := range joined {
		go func(i int) {
			defer staggerGroup.Done()
			joined[i] = doWatch(t, base, operator, staggerDevice, staggerTicket, fmt.Sprintf("s%d", i+1), fmt.Sprintf("sk%d", i+1))
		}(i)
	}
	go func() {
		defer staggerGroup.Done()
		leave(t, base, operator, stagger.ViewerID)
	}()
	time.Sleep(2 * time.Second)
	touchCtx, touchCancel := context.WithTimeout(context.Background(), 5*time.Second)
	_, touchErr := hold.ExecContext(touchCtx, `UPDATE rtc_viewer SET heartbeat_at = UTC_TIMESTAMP(3) WHERE tenant_id = 'dev' AND viewer_id = ?`, stagger.ViewerID)
	touchCancel()
	var commitErr error
	if touchErr != nil {
		_ = hold.Rollback()
	} else {
		commitErr = hold.Commit()
	}
	staggerGroup.Wait()
	if touchErr != nil {
		t.Fatal(touchErr)
	}
	if commitErr != nil {
		t.Fatal(commitErr)
	}
	for i := range joined {
		if joined[i].SessionID != joined[0].SessionID {
			t.Fatalf("concurrent join created another session")
		}
		if postJSON(t, base+"/api/v1/viewers/"+joined[i].ViewerID+"/heartbeat", operator, map[string]any{}, nil) != http.StatusOK {
			t.Fatal("joined viewer heartbeat failed")
		}
	}
	waitSession(t, base, operator, joined[0].SessionID, func(view sessionView) bool { return view.Status == "ACTIVE" })
	for i := range joined {
		if postJSON(t, base+"/api/v1/viewers/"+joined[i].ViewerID+"/heartbeat", operator, map[string]any{}, nil) != http.StatusOK {
			t.Fatal("joined viewer heartbeat failed")
		}
	}
	time.Sleep(1300 * time.Millisecond)
	if got := getSession(t, base, operator, joined[0].SessionID); got.Status != "ACTIVE" || got.Generation != joined[0].Generation {
		t.Fatalf("stop counted a live join as gone: status=%s generation=%d", got.Status, got.Generation)
	}
	var sessionStatus, viewerStatus string
	if err := db.QueryRow(`
		SELECT s.status, v.status FROM rtc_viewer v
		JOIN rtc_session s ON s.tenant_id = v.tenant_id AND s.session_id = v.session_id
		WHERE v.tenant_id = 'dev' AND v.viewer_id = ?`, joined[0].ViewerID).Scan(&sessionStatus, &viewerStatus); err != nil {
		t.Fatal(err)
	}
	if sessionStatus != "ACTIVE" || (viewerStatus != "JOINING" && viewerStatus != "ACTIVE") {
		t.Fatalf("joiner landed on session=%s viewer=%s", sessionStatus, viewerStatus)
	}
	var liveSessions int
	if err := db.QueryRow(`
		SELECT COUNT(*) FROM rtc_session
		WHERE tenant_id = 'dev' AND device_id = ? AND status NOT IN ('CLOSED', 'FAILED')`, staggerDevice).Scan(&liveSessions); err != nil || liveSessions != 1 {
		t.Fatalf("live sessions = %d", liveSessions)
	}
	var staleStatus string
	if err := db.QueryRow(`SELECT status FROM rtc_viewer WHERE tenant_id = 'dev' AND viewer_id = ?`, stagger.ViewerID).Scan(&staleStatus); err != nil {
		t.Fatal(err)
	}
	if staleStatus == "JOINING" || staleStatus == "ACTIVE" {
		t.Fatalf("stale viewer still occupies as %s", staleStatus)
	}

	log2 := filepath.Join(t.TempDir(), "server-2.log")
	stop2 := startServer(t, bin, log2, map[string]string{
		"IOT_HTTP_ADDR":            "127.0.0.1:18081",
		"IOT_HOOK_ADDR":            "",
		"IOT_SFU_ADDR":             "",
		"IOT_ENABLE_TEST_CONTROLS": "0",
		"IOT_INSTANCE_ID":          "iot-dev-2",
		"IOT_STOP_DELAY":           "1s",
		"IOT_VIEWER_JOIN_GRACE":    "2s",
		"IOT_CALL_RING_TIMEOUT":    "2s",
	}, secrets)
	t.Cleanup(stop2)
	waitReady(t, "http://127.0.0.1:18081")
	dualDevice := newDevice(t, base, admin)
	grant(t, base, admin, dualDevice, "operator", "call")
	dualTicket := issueTicket(t, base, admin, dualDevice)
	secrets = append(secrets, dualTicket)
	dualStatus := make(chan int, 2)
	go func() {
		dualStatus <- postJSON(t, base+"/api/v1/devices/"+dualDevice+"/calls", admin, callBody(dualTicket), nil)
	}()
	go func() {
		dualStatus <- postJSON(t, "http://127.0.0.1:18081/api/v1/devices/"+dualDevice+"/calls", admin, callBody(dualTicket), nil)
	}()
	leftStatus, rightStatus := <-dualStatus, <-dualStatus
	if (leftStatus == http.StatusCreated) == (rightStatus == http.StatusCreated) {
		t.Fatalf("two instances both returned %d and %d", leftStatus, rightStatus)
	}

	rdb := redis.NewClient(&redis.Options{Addr: os.Getenv("IOT_REDIS_ADDR"), Password: os.Getenv("IOT_REDIS_PASSWORD")})
	defer rdb.Close()
	if err := rdb.FlushDB(context.Background()).Err(); err != nil {
		t.Fatal("redis flush failed")
	}
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if n, err := rdb.ZCard(context.Background(), "rtc:task-index").Result(); err == nil && n > 0 {
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	if n, err := rdb.ZCard(context.Background(), "rtc:task-index").Result(); err != nil || n == 0 {
		t.Fatal("task index was not rebuilt from mysql")
	}
	scanLogs(t, []string{logPath}, secrets)
}

type watchResult struct {
	SessionID     string `json:"session_id"`
	Generation    int64  `json:"generation"`
	SessionStatus string `json:"session_status"`
	ViewerID      string `json:"viewer_id"`
	Replayed      bool   `json:"-"`
	Credential    struct {
		Provider    string `json:"provider"`
		Connectable bool   `json:"connectable"`
		Token       string `json:"token"`
		ResourceID  string `json:"resource_id"`
		WHEPURL     string `json:"whep_url"`
	} `json:"credential"`
}

type callResult struct {
	CallID     string `json:"call_id"`
	SessionID  string `json:"session_id"`
	Generation int64  `json:"generation"`
	Status     string `json:"status"`
}

type sessionView struct {
	Status      string `json:"status"`
	Generation  int64  `json:"generation"`
	DeviceReady bool   `json:"device_ready"`
	MediaReady  bool   `json:"media_ready"`
	Occupants   int    `json:"occupants"`
}

func issueTicket(t *testing.T, base, token, deviceID string) string {
	t.Helper()
	return ticketFrom(t, postTicket(t, base+"/api/v1/devices/"+deviceID+"/tickets", token))
}

func issueTicketRotate(t *testing.T, base, token, deviceID string) string {
	t.Helper()
	return ticketFrom(t, postTicket(t, base+"/api/v1/devices/"+deviceID+"/tickets/rotate", token))
}

func postTicket(t *testing.T, raw, token string) map[string]any {
	t.Helper()
	body := map[string]any{}
	if status := postJSON(t, raw, token, map[string]any{}, &body); status != http.StatusCreated && status != http.StatusOK {
		t.Fatalf("ticket status %d", status)
	}
	return body
}

func ticketFrom(t *testing.T, body map[string]any) string {
	t.Helper()
	ticket, _ := body["ticket"].(string)
	if len(ticket) < 16 {
		t.Fatal("ticket missing")
	}
	return ticket
}

func postSDP(t *testing.T, raw, token, sdp string) int {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, raw, strings.NewReader(sdp))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/sdp")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	return resp.StatusCode
}

func watchBody(ticket, clientID, key string) map[string]any {
	return map[string]any{"ticket": ticket, "client_id": clientID, "idempotency_key": key, "channel_id": "1", "stream_type": "live", "resolution": "sd"}
}

func callBody(ticket string) map[string]any {
	return map[string]any{"ticket": ticket, "callee": "operator", "channel_id": "1", "stream_type": "av", "resolution": ""}
}

func doWatch(t *testing.T, base, token, deviceID, ticket, clientID, key string) watchResult {
	t.Helper()
	var result watchResult
	status := postJSON(t, base+"/api/v1/devices/"+deviceID+"/watch", token, watchBody(ticket, clientID, key), &result)
	result.Replayed = status == http.StatusOK
	if status != http.StatusCreated && status != http.StatusOK {
		t.Fatalf("watch status %d", status)
	}
	if len(result.Credential.Token) >= 12 {
		// 凭证只存在响应里，测试不把它写进日志。
	}
	return result
}

func doCall(t *testing.T, base, token, deviceID, ticket string) callResult {
	t.Helper()
	var result callResult
	if status := postJSON(t, base+"/api/v1/devices/"+deviceID+"/calls", token, callBody(ticket), &result); status != http.StatusCreated {
		t.Fatalf("call status %d", status)
	}
	return result
}

func leave(t *testing.T, base, token, viewerID string) {
	t.Helper()
	if postJSON(t, base+"/api/v1/viewers/"+viewerID+"/leave", token, map[string]any{}, nil) != http.StatusOK {
		t.Fatal("leave failed")
	}
}

func getSession(t *testing.T, base, token, sessionID string) sessionView {
	t.Helper()
	var view sessionView
	getJSON(t, base+"/api/v1/sessions/"+sessionID, token, &view)
	return view
}

func waitSession(t *testing.T, base, token, sessionID string, ready func(sessionView) bool) {
	t.Helper()
	deadline := time.Now().Add(8 * time.Second)
	var last sessionView
	for time.Now().Before(deadline) {
		last = getSession(t, base, token, sessionID)
		if ready(last) {
			return
		}
		time.Sleep(150 * time.Millisecond)
	}
	t.Fatalf("session %s stayed %+v", sessionID, last)
}

func waitCall(t *testing.T, base, token, callID, status string) {
	t.Helper()
	deadline := time.Now().Add(8 * time.Second)
	var body map[string]any
	for time.Now().Before(deadline) {
		getJSON(t, base+"/api/v1/calls/"+callID, token, &body)
		if body["status"] == status {
			return
		}
		time.Sleep(150 * time.Millisecond)
	}
	t.Fatalf("call stayed %v", body["status"])
}

func postEvent(t *testing.T, base string, secrets []string, sessionID string, generation int64, eventType, identity string) {
	t.Helper()
	payload, _ := json.Marshal(map[string]any{
		"event_id":   fmt.Sprintf("%s-%d-%s", eventType, generation, sessionID),
		"event_type": eventType,
		"session_id": sessionID,
		"generation": generation,
		"identity":   identity,
	})
	req, err := http.NewRequest(http.MethodPost, base+"/internal/media/events", bytes.NewReader(payload))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Iot-Hook-Token", os.Getenv("IOT_MQTT_HOOK_TOKEN"))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("event %s status %d", eventType, resp.StatusCode)
	}
	_ = secrets
}

func putPlan(t *testing.T, base string, secrets []string, deviceID, op, result string) {
	t.Helper()
	payload, _ := json.Marshal(map[string]string{"device_id": deviceID, "op": op, "result": result})
	req, err := http.NewRequest(http.MethodPut, base+"/internal/rtc/plan", bytes.NewReader(payload))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Iot-Hook-Token", os.Getenv("IOT_MQTT_HOOK_TOKEN"))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("plan status %d", resp.StatusCode)
	}
	_ = secrets
}
