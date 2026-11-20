package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"iot-server/internal/sfu"
)

func TestVoiceCallPublishesPublicWHIP(t *testing.T) {
	node, err := sfu.ListenEndpoint("127.0.0.1:0", sfu.Endpoint{
		PublicIP:  "203.0.113.5",
		PublicURL: "http://203.0.113.5:18196",
		UDPMin:    40100,
		UDPMax:    40131,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = node.Close() })
	on := true
	api := &Server{media: node.Media()}
	payload, peer, err := api.commandPayload(context.Background(), "9030d6cb176c", 7, "voice_call", &on, []byte(`{"duration_min":15}`))
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Cmd    string         `json:"cmd"`
		Params map[string]any `json:"params"`
	}
	if err := json.Unmarshal(payload, &doc); err != nil {
		t.Fatal(err)
	}
	whip, _ := doc.Params["whip_url"].(string)
	deviceToken, _ := doc.Params["token"].(string)
	peerToken, _ := peer["token"].(string)
	if doc.Cmd != "voice_call" || whip != "http://203.0.113.5:18196/whip/pion-9030d6cb176c" || doc.Params["duration_min"] != float64(15) {
		t.Fatalf("payload = %s", payload)
	}
	if !strings.HasPrefix(deviceToken, "cGlvbi0") || deviceToken == peerToken || strings.Contains(string(payload), peerToken) {
		t.Fatal("device and peer tokens were not separated")
	}
	if _, ok := peer["whep_url"]; ok {
		t.Fatal("voice call returned a subscribe address")
	}
}

func TestVoiceCallWithoutPublicSFUIsUnavailable(t *testing.T) {
	node, err := sfu.Listen("127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = node.Close() })
	on := true
	_, _, err = (&Server{media: node.Media()}).commandPayload(context.Background(), "9030d6cb176c", 7, "voice_call", &on, nil)
	if !errors.Is(err, errVoiceUnavailable) {
		t.Fatalf("error = %v", err)
	}
}
