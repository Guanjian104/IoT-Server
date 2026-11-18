package sfu

import (
	"bytes"
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pion/webrtc/v4"
	pionmedia "github.com/pion/webrtc/v4/pkg/media"
	"iot-server/internal/media"
)

func TestDeliverRules(t *testing.T) {
	if !delivers("viewer:v", "device:d") || !delivers("device:d", "call:c") || !delivers("call:a", "call:b") || !delivers("call:a", "device:d") || !delivers("subscriber", "publisher") {
		t.Fatal("public and call peers were not connected")
	}
	if delivers("viewer:v", "call:c") || delivers("viewer:v", "viewer:other") || delivers("device:d", "viewer:v") || delivers("device:d", "device:d") {
		t.Fatal("private uplink or viewer publish was forwarded")
	}
}

func TestCallUplinkDoesNotReachViewers(t *testing.T) {
	node := newTestNode(t)
	provider := node.Media().(*Provider)
	room, err := provider.CreateRoom(context.Background(), media.RoomRequest{SessionID: "session2", Generation: 1})
	if err != nil {
		t.Fatal(err)
	}
	expires := time.Now().Add(time.Minute).UTC().Truncate(time.Second)
	issue := func(identity string) media.JoinCredential {
		t.Helper()
		cred, err := provider.CreateJoinCredential(context.Background(), media.JoinRequest{
			ResourceID: room.ID, Identity: identity, ExpiresAt: expires,
		})
		if err != nil {
			t.Fatal(err)
		}
		return cred
	}
	deviceCred := issue("device:cam9")
	callCred := issue("call:c9")
	viewCred := issue("viewer:phone9")
	if deviceCred.Token == callCred.Token || !deviceCred.Connectable || !viewCred.Connectable {
		t.Fatal("call and watch credentials were not separated")
	}

	public := []byte("watch-public-marker")
	private := []byte("call-private-uplink")
	viewer := newEndpoint(t, node.api)
	if _, err := viewer.AddTransceiverFromKind(webrtc.RTPCodecTypeVideo, webrtc.RTPTransceiverInit{
		Direction: webrtc.RTPTransceiverDirectionRecvonly,
	}); err != nil {
		t.Fatal(err)
	}
	var viewerTracks atomic.Int32
	publicSeen := make(chan struct{}, 1)
	privateSeen := make(chan struct{}, 1)
	viewer.OnTrack(func(track *webrtc.TrackRemote, _ *webrtc.RTPReceiver) {
		viewerTracks.Add(1)
		buffer := make([]byte, 1500)
		for {
			n, _, err := track.Read(buffer)
			if err != nil {
				return
			}
			packet := append([]byte(nil), buffer[:n]...)
			if bytes.Contains(packet, private) {
				select {
				case privateSeen <- struct{}{}:
				default:
				}
			}
			if bytes.Contains(packet, public) {
				select {
				case publicSeen <- struct{}{}:
				default:
				}
			}
		}
	})
	answer := postSDP(t, provider.SubscribeURL(room.ID), viewCred.Token, localOffer(t, viewer))
	if err := viewer.SetRemoteDescription(webrtc.SessionDescription{Type: webrtc.SDPTypeAnswer, SDP: answer}); err != nil {
		t.Fatal(err)
	}
	waitConnected(t, viewer)

	device := newEndpoint(t, node.api)
	devicePrivate := make(chan struct{}, 1)
	device.OnTrack(func(track *webrtc.TrackRemote, _ *webrtc.RTPReceiver) {
		buffer := make([]byte, 1500)
		for {
			n, _, err := track.Read(buffer)
			if err != nil {
				return
			}
			if bytes.Contains(buffer[:n], private) {
				select {
				case devicePrivate <- struct{}{}:
				default:
				}
				return
			}
		}
	})
	publicTrack, err := webrtc.NewTrackLocalStaticSample(webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeVP8}, "video", "watch")
	if err != nil {
		t.Fatal(err)
	}
	publicSender, err := device.AddTrack(publicTrack)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := device.AddTransceiverFromKind(webrtc.RTPCodecTypeVideo, webrtc.RTPTransceiverInit{
		Direction: webrtc.RTPTransceiverDirectionRecvonly,
	}); err != nil {
		t.Fatal(err)
	}
	sendCtx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go readRTCP(sendCtx, publicSender)
	answer = postSDP(t, provider.PublishURL(room.ID), deviceCred.Token, localOffer(t, device))
	if err := device.SetRemoteDescription(webrtc.SessionDescription{Type: webrtc.SDPTypeAnswer, SDP: answer}); err != nil {
		t.Fatal(err)
	}
	waitConnected(t, device)
	stopPublic := writeSamples(t, publicTrack, append([]byte{0x10}, public...))
	defer close(stopPublic)
	acceptServerOffer(t, viewer, provider.SubscribeURL(room.ID), viewCred.Token)
	select {
	case <-publicSeen:
	case <-time.After(12 * time.Second):
		t.Fatal("viewer did not receive the device stream")
	}

	call := newEndpoint(t, node.api)
	privateTrack, err := webrtc.NewTrackLocalStaticSample(webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeVP8}, "audio", "call")
	if err != nil {
		t.Fatal(err)
	}
	privateSender, err := call.AddTrack(privateTrack)
	if err != nil {
		t.Fatal(err)
	}
	go readRTCP(sendCtx, privateSender)
	answer = postSDP(t, provider.PublishURL(room.ID), callCred.Token, localOffer(t, call))
	if err := call.SetRemoteDescription(webrtc.SessionDescription{Type: webrtc.SDPTypeAnswer, SDP: answer}); err != nil {
		t.Fatal(err)
	}
	waitConnected(t, call)
	stopPrivate := writeSamples(t, privateTrack, append([]byte{0x10}, private...))
	defer close(stopPrivate)
	acceptServerOffer(t, device, provider.PublishURL(room.ID), deviceCred.Token)
	select {
	case <-devicePrivate:
	case <-time.After(12 * time.Second):
		t.Fatal("device did not receive the call uplink")
	}
	select {
	case <-node.offerBox(room.ID, "viewer:phone9"):
		t.Fatal("viewer was offered the call uplink")
	case <-privateSeen:
		t.Fatal("viewer received the call uplink")
	case <-time.After(500 * time.Millisecond):
	}
	if got := viewerTracks.Load(); got != 1 {
		t.Fatalf("viewer tracks = %d", got)
	}
}

func acceptServerOffer(t *testing.T, pc *webrtc.PeerConnection, url, token string) {
	t.Helper()
	offerSDP := getSDP(t, url, token)
	if err := pc.SetRemoteDescription(webrtc.SessionDescription{Type: webrtc.SDPTypeOffer, SDP: offerSDP}); err != nil {
		t.Fatal(err)
	}
	answer, err := pc.CreateAnswer(nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := pc.SetLocalDescription(answer); err != nil {
		t.Fatal(err)
	}
	<-webrtc.GatheringCompletePromise(pc)
	local := pc.LocalDescription()
	if local == nil {
		t.Fatal("renegotiation answer is empty")
	}
	putSDP(t, url, token, local.SDP)
}

func writeSamples(t *testing.T, track *webrtc.TrackLocalStaticSample, payload []byte) chan struct{} {
	t.Helper()
	done := make(chan struct{})
	go func() {
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
	return done
}
