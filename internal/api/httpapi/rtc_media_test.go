package httpapi

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"iot-server/internal/sfu"
	"iot-server/internal/store"
)

func TestWatchCredentialStaysTestWithoutProvider(t *testing.T) {
	api := New(nil, nil, nil, "dev", "", "", "", time.Second, nil)
	body, err := api.watchCredential(context.Background(), store.WatchResult{Credential: "stored-viewer-token"})
	if err != nil {
		t.Fatal(err)
	}
	if body["provider"] != "test" || body["connectable"] != false || body["token"] != "stored-viewer-token" {
		t.Fatalf("credential = %#v", body)
	}
}

func TestWatchCredentialUsesPion(t *testing.T) {
	node, err := sfu.Listen("127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = node.Close() })
	api := New(nil, nil, nil, "dev", "", "", "", time.Second, nil)
	api.UseMedia(node.Media())
	body, err := api.watchCredential(context.Background(), store.WatchResult{
		SessionID:  "s0011223344556677",
		Generation: 1,
		ViewerID:   "v0011223344556677",
		Credential: "stored-viewer-token",
	})
	if err != nil {
		t.Fatal(err)
	}
	if body["provider"] != "pion" || body["connectable"] != true {
		t.Fatalf("provider = %#v connectable = %#v", body["provider"], body["connectable"])
	}
	token, _ := body["token"].(string)
	if token == "" || token == "stored-viewer-token" {
		t.Fatal("watch reused the stored viewer token")
	}
	resource, _ := body["resource_id"].(string)
	if resource != "pion-s0011223344556677" {
		t.Fatalf("resource = %s", resource)
	}
	whep, _ := body["whep_url"].(string)
	if !strings.HasPrefix(whep, "http://127.0.0.1:") || !strings.HasSuffix(whep, "/whep/"+resource) {
		t.Fatalf("whep url = %s", whep)
	}
	expires, err := time.Parse(time.RFC3339, body["expires_at"].(string))
	if err != nil {
		t.Fatal(err)
	}
	if time.Until(expires) < time.Minute || time.Until(expires) > 3*time.Minute {
		t.Fatalf("expires in %s", time.Until(expires))
	}
	req, err := http.NewRequest(http.MethodPost, strings.Replace(whep, "/whep/", "/whip/", 1), strings.NewReader("v=0\r\n"))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/sdp")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("viewer token on whip = %d", resp.StatusCode)
	}
}
