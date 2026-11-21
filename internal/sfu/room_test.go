package sfu

import (
	"bytes"
	"testing"
	"time"

	"github.com/pion/webrtc/v4"
	"github.com/pion/webrtc/v4/pkg/media"
)

func TestRoomForwardsOneTrack(t *testing.T) {
	api := NewAPI()
	marker := []byte("sfu-forward-marker")
	received := make(chan []byte, 1)
	clients := map[string]*webrtc.PeerConnection{}

	var room *Room
	room, err := NewRoom("room-1", api, func(peerID string, offer webrtc.SessionDescription) {
		client := clients[peerID]
		if client == nil {
			t.Errorf("offer for unknown peer %s", peerID)
			return
		}
		if err := client.SetRemoteDescription(offer); err != nil {
			t.Errorf("set subscriber offer: %v", err)
			return
		}
		answer, err := client.CreateAnswer(nil)
		if err != nil {
			t.Errorf("create subscriber answer: %v", err)
			return
		}
		if err := client.SetLocalDescription(answer); err != nil {
			t.Errorf("set subscriber answer: %v", err)
			return
		}
		<-webrtc.GatheringCompletePromise(client)
		local := client.LocalDescription()
		if local == nil {
			t.Error("subscriber answer is empty")
			return
		}
		if err := room.HandleAnswer(peerID, *local); err != nil {
			t.Errorf("accept subscriber answer: %v", err)
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	if room.ID() != "room-1" {
		t.Fatalf("room id = %s", room.ID())
	}
	t.Cleanup(func() { _ = room.Close() })

	publisher := newEndpoint(t, api)
	subscriber := newEndpoint(t, api)
	clients["publisher"] = publisher
	clients["subscriber"] = subscriber
	subscriber.OnTrack(func(track *webrtc.TrackRemote, _ *webrtc.RTPReceiver) {
		buffer := make([]byte, 1500)
		for {
			n, _, err := track.Read(buffer)
			if err != nil {
				return
			}
			if bytes.Contains(buffer[:n], marker) {
				select {
				case received <- append([]byte(nil), buffer[:n]...):
				default:
				}
				return
			}
		}
	})

	if err := room.Join("publisher"); err != nil {
		t.Fatal(err)
	}
	if err := room.Join("subscriber"); err != nil {
		t.Fatal(err)
	}
	if _, err := subscriber.AddTransceiverFromKind(webrtc.RTPCodecTypeVideo, webrtc.RTPTransceiverInit{
		Direction: webrtc.RTPTransceiverDirectionRecvonly,
	}); err != nil {
		t.Fatal(err)
	}

	track, err := webrtc.NewTrackLocalStaticSample(
		webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeVP8},
		"video",
		"stream-1",
	)
	if err != nil {
		t.Fatal(err)
	}
	sender, err := publisher.AddTrack(track)
	if err != nil {
		t.Fatal(err)
	}
	go readRTCP(room.ctx, sender)

	connect(t, publisher, room, "publisher")
	connect(t, subscriber, room, "subscriber")
	waitConnected(t, publisher)
	waitConnected(t, subscriber)

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
				_ = track.WriteSample(media.Sample{Data: payload, Duration: 20 * time.Millisecond})
			}
		}
	}()
	defer close(done)

	select {
	case packet := <-received:
		if !bytes.Contains(packet, marker) {
			t.Fatal("forwarded packet lost the payload marker")
		}
	case <-time.After(12 * time.Second):
		if err := room.Err(); err != nil {
			t.Fatalf("track was not forwarded: %v", err)
		}
		t.Fatal("track was not forwarded")
	}
}

func newEndpoint(t *testing.T, api *webrtc.API) *webrtc.PeerConnection {
	t.Helper()
	pc, err := api.NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = pc.Close() })
	return pc
}

func connect(t *testing.T, client *webrtc.PeerConnection, room *Room, id string) {
	t.Helper()
	offer, err := client.CreateOffer(nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := client.SetLocalDescription(offer); err != nil {
		t.Fatal(err)
	}
	<-webrtc.GatheringCompletePromise(client)
	local := client.LocalDescription()
	if local == nil {
		t.Fatal("endpoint offer is empty")
	}
	answer, err := room.HandleOffer(id, *local)
	if err != nil {
		t.Fatal(err)
	}
	if err := client.SetRemoteDescription(answer); err != nil {
		t.Fatal(err)
	}
}

func TestJoinReplacesSameIdentity(t *testing.T) {
	room, err := NewRoom("room-rejoin", NewAPI(), nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = room.Close() })
	const device = "call:9030d6cb2703"
	if err := room.Join(device); err != nil {
		t.Fatal(err)
	}
	if err := room.Join("call:u7"); err != nil {
		t.Fatal(err)
	}
	if err := room.Join(device); err != nil {
		t.Fatal(err)
	}
	if !room.joined(device) || !room.joined("call:u7") {
		t.Fatal("replacement removed the other peer or the new one")
	}
	room.mu.Lock()
	count := len(room.peers)
	room.mu.Unlock()
	if count != 2 {
		t.Fatalf("peers = %d", count)
	}
}

func waitConnected(t *testing.T, pc *webrtc.PeerConnection) {
	t.Helper()
	if pc.ConnectionState() == webrtc.PeerConnectionStateConnected {
		return
	}
	connected := make(chan struct{})
	pc.OnConnectionStateChange(func(state webrtc.PeerConnectionState) {
		if state == webrtc.PeerConnectionStateConnected {
			select {
			case <-connected:
			default:
				close(connected)
			}
		}
	})
	if pc.ConnectionState() == webrtc.PeerConnectionStateConnected {
		return
	}
	select {
	case <-connected:
	case <-time.After(8 * time.Second):
		t.Fatalf("peer connection stayed %s", pc.ConnectionState())
	}
}
