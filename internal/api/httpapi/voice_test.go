package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
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

func TestInternalCallIssuesSeparateTokens(t *testing.T) {
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
	api := &Server{media: node.Media(), hookToken: "hook-token"}
	req := httptest.NewRequest(http.MethodPost, "/internal/calls", strings.NewReader(`{"mac":"9030d6cb2703","user_ref":"USER1","duration_min":15}`))
	req.Header.Set("X-Iot-Hook-Token", "hook-token")
	rec := httptest.NewRecorder()
	api.issueCall(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	var doc struct {
		Whip        string `json:"whip_url"`
		DeviceToken string `json:"device_token"`
		AppToken    string `json:"app_token"`
		Expires     string `json:"expires_at"`
		WHEP        string `json:"whep_url"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &doc); err != nil {
		t.Fatal(err)
	}
	if doc.Whip != "http://203.0.113.5:18196/whip/pion-9030d6cb2703" || doc.DeviceToken == "" || doc.AppToken == "" || doc.DeviceToken == doc.AppToken || doc.Expires == "" || doc.WHEP != "" {
		t.Fatalf("call = %+v", doc)
	}
	if strings.Contains(doc.Whip, "127.0.0.1") || strings.Contains(rec.Body.String(), "/whep/") {
		t.Fatalf("body = %s", rec.Body.String())
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
