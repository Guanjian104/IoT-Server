package sfu

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/pion/webrtc/v4"
	pionmedia "github.com/pion/webrtc/v4/pkg/media"
	"iot-server/internal/media"
)

func TestWHIPAndWHEPForwardOneTrack(t *testing.T) {
	node := newTestNode(t)
	provider := node.Media().(*Provider)
	room, publish, subscribe := connectableCredentials(t, provider)

	subscriber := newEndpoint(t, node.api)
	if _, err := subscriber.AddTransceiverFromKind(webrtc.RTPCodecTypeVideo, webrtc.RTPTransceiverInit{
		Direction: webrtc.RTPTransceiverDirectionRecvonly,
	}); err != nil {
		t.Fatal(err)
	}
	marker := []byte("sfu-forward-marker")
	received := make(chan []byte, 1)
	subscriber.OnTrack(func(track *webrtc.TrackRemote, _ *webrtc.RTPReceiver) {
		buffer := make([]byte, 1500)
		for {
			n, _, err := track.Read(buffer)
			if err != nil {
				return
			}
			packet := append([]byte(nil), buffer[:n]...)
			if bytes.Contains(packet, marker) {
				select {
				case received <- packet:
				default:
				}
				return
			}
		}
	})
	answer := postSDP(t, provider.SubscribeURL(room.ID), subscribe.Token, localOffer(t, subscriber))
	if err := subscriber.SetRemoteDescription(webrtc.SessionDescription{Type: webrtc.SDPTypeAnswer, SDP: answer}); err != nil {
		t.Fatal(err)
	}
	waitConnected(t, subscriber)

	publisher := newEndpoint(t, node.api)
	track, err := webrtc.NewTrackLocalStaticSample(webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeVP8}, "video", "stream")
	if err != nil {
		t.Fatal(err)
	}
	sender, err := publisher.AddTrack(track)
	if err != nil {
		t.Fatal(err)
	}
	sendCtx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go readRTCP(sendCtx, sender)
	answer = postSDP(t, provider.PublishURL(room.ID), publish.Token, localOffer(t, publisher))
	if err := publisher.SetRemoteDescription(webrtc.SessionDescription{Type: webrtc.SDPTypeAnswer, SDP: answer}); err != nil {
		t.Fatal(err)
	}
	waitConnected(t, publisher)

	done := make(chan struct{})
	go func() {
		payload := append([]byte{0x10}, marker...)
		ticker := time.NewTicker(20 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-done:
				return
			case <-ticker.C:
				_ = track.WriteSample(pionmedia.Sample{Data: payload, Duration: 20 * time.Millisecond})
			}
		}
	}()
	defer close(done)

	offerSDP := getSDP(t, provider.SubscribeURL(room.ID), subscribe.Token)
	if err := subscriber.SetRemoteDescription(webrtc.SessionDescription{Type: webrtc.SDPTypeOffer, SDP: offerSDP}); err != nil {
		t.Fatal(err)
	}
	renegotiation, err := subscriber.CreateAnswer(nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := subscriber.SetLocalDescription(renegotiation); err != nil {
		t.Fatal(err)
	}
	<-webrtc.GatheringCompletePromise(subscriber)
	local := subscriber.LocalDescription()
	if local == nil {
		t.Fatal("renegotiation answer is empty")
	}
	putSDP(t, provider.SubscribeURL(room.ID), subscribe.Token, local.SDP)

	select {
	case packet := <-received:
		if !bytes.Contains(packet, marker) {
			t.Fatal("forwarded packet lost the payload marker")
		}
	case <-time.After(12 * time.Second):
		if err := node.room(room.ID).Err(); err != nil {
			t.Fatalf("track was not forwarded: %v", err)
		}
		t.Fatal("track was not forwarded")
	}
}

func TestJoinCredentialIsShortAndRoleBound(t *testing.T) {
	node := newTestNode(t)
	provider := node.Media().(*Provider)
	room, publish, subscribe := connectableCredentials(t, provider)
	if publish.Provider != "pion" || !publish.Connectable || subscribe.Provider != "pion" || !subscribe.Connectable {
		t.Fatal("credential is not connectable")
	}
	if publish.Token == "" || publish.Token == subscribe.Token {
		t.Fatal("publish and subscribe credentials were not distinct")
	}
	again, err := provider.CreateRoom(context.Background(), media.RoomRequest{SessionID: "session1", Generation: 2})
	if err != nil || again.ID != room.ID || again.Created || again.Provider != "pion" {
		t.Fatalf("room was not reused: %s created=%t %v", again.ID, again.Created, err)
	}
	if _, err := provider.CreateJoinCredential(context.Background(), media.JoinRequest{
		ResourceID: room.ID,
		Identity:   "viewer:phone1",
		ExpiresAt:  time.Now().Add(-time.Second),
	}); err == nil {
		t.Fatal("expired credential was issued")
	}
	if _, err := provider.CreateJoinCredential(context.Background(), media.JoinRequest{
		ResourceID: room.ID,
		Identity:   "viewer:phone1",
		ExpiresAt:  time.Now().Add(11 * time.Minute),
	}); err == nil {
		t.Fatal("long-lived credential was issued")
	}

	status := postStatus(t, provider.PublishURL(room.ID), subscribe.Token, "v=0\r\n")
	if status != http.StatusUnauthorized {
		t.Fatalf("subscribe token on whip = %d", status)
	}
	expired, err := sign(node.secret, room.ID, "device:cam1", "publish", time.Now().Add(-time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if status := postStatus(t, provider.PublishURL(room.ID), expired, "v=0\r\n"); status != http.StatusUnauthorized {
		t.Fatalf("expired token = %d", status)
	}
	missing, err := sign(node.secret, "pion-missing", "device:cam1", "publish", time.Now().Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if status := postStatus(t, node.base+"/whip/pion-missing", missing, "v=0\r\n"); status != http.StatusNotFound {
		t.Fatalf("missing room = %d", status)
	}
}

func TestEndRemovesParticipantAndUnsharedRoom(t *testing.T) {
	node := newTestNode(t)
	provider := node.Media().(*Provider)
	room, err := provider.CreateRoom(context.Background(), media.RoomRequest{SessionID: "session9", Generation: 1})
	if err != nil {
		t.Fatal(err)
	}
	if err := node.room(room.ID).Join("viewer:phone9"); err != nil {
		t.Fatal(err)
	}
	if err := provider.RemoveParticipant(context.Background(), room.ID, "viewer:phone9"); err != nil {
		t.Fatal(err)
	}
	if err := node.room(room.ID).Join("viewer:phone9"); err != nil {
		t.Fatal(err)
	}
	if err := provider.DeleteRoom(context.Background(), room.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := provider.GetRoom(context.Background(), room.ID); err == nil {
		t.Fatal("deleted room is still visible")
	}
	rebuilt, err := provider.CreateRoom(context.Background(), media.RoomRequest{SessionID: "session9", Generation: 1})
	if err != nil || !rebuilt.Created || rebuilt.ID != room.ID {
		t.Fatalf("room was not recreated: %+v %v", rebuilt, err)
	}
}

func newTestNode(t *testing.T) *Node {
	t.Helper()
	node, err := Listen("127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = node.Close() })
	return node
}

func connectableCredentials(t *testing.T, provider *Provider) (media.Room, media.JoinCredential, media.JoinCredential) {
	t.Helper()
	room, err := provider.CreateRoom(context.Background(), media.RoomRequest{SessionID: "session1", Generation: 1})
	if err != nil {
		t.Fatal(err)
	}
	if room.ID == "session1" || !strings.HasPrefix(room.ID, "pion-") || !room.Created || room.Provider != "pion" {
		t.Fatalf("resource id = %s created=%t provider=%s", room.ID, room.Created, room.Provider)
	}
	expires := time.Now().Add(time.Minute).UTC().Truncate(time.Second)
	publish, err := provider.CreateJoinCredential(context.Background(), media.JoinRequest{
		ResourceID: room.ID, Identity: "device:cam1", ExpiresAt: expires,
	})
	if err != nil {
		t.Fatal(err)
	}
	subscribe, err := provider.CreateJoinCredential(context.Background(), media.JoinRequest{
		ResourceID: room.ID, Identity: "viewer:phone1", ExpiresAt: expires,
	})
	if err != nil {
		t.Fatal(err)
	}
	return room, publish, subscribe
}

func localOffer(t *testing.T, pc *webrtc.PeerConnection) webrtc.SessionDescription {
	t.Helper()
	offer, err := pc.CreateOffer(nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := pc.SetLocalDescription(offer); err != nil {
		t.Fatal(err)
	}
	<-webrtc.GatheringCompletePromise(pc)
	local := pc.LocalDescription()
	if local == nil {
		t.Fatal("endpoint offer is empty")
	}
	return *local
}

func postSDP(t *testing.T, url, token string, offer webrtc.SessionDescription) string {
	t.Helper()
	status, body := sendSDP(t, http.MethodPost, url, token, offer.SDP)
	if status != http.StatusCreated || body == "" {
		t.Fatalf("whip/whep status %d", status)
	}
	return body
}

func getSDP(t *testing.T, url, token string) string {
	t.Helper()
	status, body := sendSDP(t, http.MethodGet, url, token, "")
	if status != http.StatusOK || body == "" {
		t.Fatalf("whep offer status %d", status)
	}
	return body
}

func putSDP(t *testing.T, url, token, sdp string) {
	t.Helper()
	status, _ := sendSDP(t, http.MethodPut, url, token, sdp)
	if status != http.StatusNoContent {
		t.Fatalf("whep answer status %d", status)
	}
}

func postStatus(t *testing.T, url, token, sdp string) int {
	t.Helper()
	status, _ := sendSDP(t, http.MethodPost, url, token, sdp)
	return status
}

func sendSDP(t *testing.T, method, url, token, sdp string) (int, string) {
	t.Helper()
	var body io.Reader
	if sdp != "" {
		body = strings.NewReader(sdp)
	}
	req, err := http.NewRequest(method, url, body)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	if sdp != "" {
		req.Header.Set("Content-Type", "application/sdp")
	}
	client := &http.Client{Timeout: 15 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	buf, err := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
	if err != nil {
		t.Fatal(err)
	}
	return resp.StatusCode, string(buf)
}
