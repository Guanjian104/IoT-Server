package bridge

import (
	"context"
	"testing"
	"time"

	"iot-server/internal/config"
	"iot-server/internal/mqttmsg"
)

func TestPersistContextOutlivesPublishCancel(t *testing.T) {
	parent, cancel := context.WithCancel(context.Background())
	cancel()
	ctx, stop := persistContext(parent)
	defer stop()
	if err := ctx.Err(); err != nil {
		t.Fatalf("result write was cancelled with the publish context: %v", err)
	}
}

func TestAckWaitStopsAtDeadline(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	if _, ok := ackWait(now.Add(100*time.Millisecond), now); ok {
		t.Fatal("short window still published")
	}
	wait, ok := ackWait(now.Add(time.Second), now)
	if !ok || wait != time.Second {
		t.Fatalf("wait = %s ok=%v", wait, ok)
	}
	wait, ok = ackWait(now.Add(10*time.Second), now)
	if !ok || wait != publishAckWait {
		t.Fatalf("full wait = %s ok=%v", wait, ok)
	}
}

func TestServiceSessionPersists(t *testing.T) {
	opts := serviceOptions(config.Config{InstanceID: "iot-dev-1", MQTTBrokerURL: "tcp://127.0.0.1:11883"})
	if opts.CleanSession || !opts.ResumeSubs || !opts.AutoAckDisabled {
		t.Fatal("service session is not durable")
	}
	if opts.ClientID != mqttmsg.ServiceClientID("iot-dev-1") {
		t.Fatalf("client id = %s", opts.ClientID)
	}
}
