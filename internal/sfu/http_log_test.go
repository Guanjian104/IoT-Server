package sfu

import (
	"bytes"
	"net/http"
	"strings"
	"testing"

	"iot-server/internal/logger"
)

func TestRejectedOfferLogsSDP(t *testing.T) {
	node := newTestNode(t)
	var buf bytes.Buffer
	log, err := logger.New(&buf, "info")
	if err != nil {
		t.Fatal(err)
	}
	node.UseLog(log)
	provider := node.Media().(*Provider)
	room, publish, _ := connectableCredentials(t, provider)

	const offer = "v=0\r\no=- 1 2 IN IP4 127.0.0.1\r\ns=-\r\nt=0 0\r\nm=audio 9 UDP/TLS/RTP/SAVPF 8\r\na=rtpmap:8 PCMA/8000"
	status, body := sendSDP(t, http.MethodPost, provider.PublishURL(room.ID), publish.Token, offer)
	if status != http.StatusBadRequest || body != "invalid\n" {
		t.Fatalf("status %d body %q", status, body)
	}
	logged := buf.String()
	if !strings.Contains(logged, "sfu sdp rejected") || !strings.Contains(logged, "a=rtpmap:8 PCMA/8000") || !strings.Contains(logged, room.ID) {
		t.Fatalf("log = %s", logged)
	}
	if strings.Contains(logged, publish.Token) {
		t.Fatal("log contains credential")
	}
}
