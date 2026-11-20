package sfu

import (
	"strings"
	"testing"
	"time"

	"github.com/pion/webrtc/v4"
)

func TestPublicEndpointRewritesHostCandidate(t *testing.T) {
	api, err := buildAPI(Endpoint{PublicIP: "203.0.113.5", UDPMin: 47900, UDPMax: 47907})
	if err != nil {
		t.Fatal(err)
	}
	pc, err := api.NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = pc.Close() })
	if _, err := pc.AddTransceiverFromKind(webrtc.RTPCodecTypeAudio); err != nil {
		t.Fatal(err)
	}
	offer, err := pc.CreateOffer(nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := pc.SetLocalDescription(offer); err != nil {
		t.Fatal(err)
	}
	select {
	case <-webrtc.GatheringCompletePromise(pc):
	case <-time.After(5 * time.Second):
		t.Fatal("gathering timed out")
	}
	local := pc.LocalDescription()
	if local == nil || !strings.Contains(local.SDP, "203.0.113.5") {
		t.Fatalf("sdp = %v", local)
	}
	if strings.Contains(local.SDP, "127.0.0.1") {
		t.Fatal("sdp still contains the loopback address")
	}
}

func TestPublishURLUsesPublicBase(t *testing.T) {
	node, err := ListenEndpoint("127.0.0.1:0", Endpoint{PublicURL: "http://203.0.113.5:18196/"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = node.Close() })
	provider := node.Media().(*Provider)
	if got := provider.PublishURL("pion-9030d6cb176c"); got != "http://203.0.113.5:18196/whip/pion-9030d6cb176c" {
		t.Fatalf("publish url = %s", got)
	}
	if got := provider.SubscribeURL("pion-9030d6cb176c"); strings.Contains(got, "203.0.113.5") {
		t.Fatalf("subscribe url = %s", got)
	}
}
