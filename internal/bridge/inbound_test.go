package bridge

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"iot-server/internal/store"
)

func TestLaneRetriesUnavailableInOrder(t *testing.T) {
	lane := newLane()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	released := make(chan struct{})
	failed := make(chan struct{})
	var once sync.Once
	var mu sync.Mutex
	var got []string
	done := make(chan struct{})
	go func() {
		lane.run(ctx, func(topic string, _ []byte) error {
			if topic == "a" {
				select {
				case <-released:
				default:
					once.Do(func() { close(failed) })
					return store.ErrUnavailable
				}
			}
			mu.Lock()
			got = append(got, topic)
			mu.Unlock()
			return nil
		}, 10*time.Millisecond, nil)
		close(done)
	}()
	acked := map[string]bool{}
	var ackMu sync.Mutex
	lane.add(delivery{topic: "a", body: []byte("1"), ack: func() {
		ackMu.Lock()
		acked["a"] = true
		ackMu.Unlock()
	}})
	lane.add(delivery{topic: "b", body: []byte("2"), ack: func() {
		ackMu.Lock()
		acked["b"] = true
		ackMu.Unlock()
	}})
	select {
	case <-failed:
	case <-time.After(2 * time.Second):
		t.Fatal("unavailable message was not retried")
	}
	mu.Lock()
	early := append([]string(nil), got...)
	mu.Unlock()
	if len(early) != 0 {
		t.Fatalf("later message ran before the failed one: %v", early)
	}
	close(released)
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		n := len(got)
		mu.Unlock()
		ackMu.Lock()
		both := acked["a"] && acked["b"]
		ackMu.Unlock()
		if n == 2 && both {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(got) != 2 || got[0] != "a" || got[1] != "b" {
		t.Fatalf("order = %v", got)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("lane did not stop")
	}
}

func TestLaneDoesNotBlockOnWaitingReply(t *testing.T) {
	lane := newLane()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var mu sync.Mutex
	acked := map[string]bool{}
	go lane.run(ctx, func(topic string, _ []byte) error {
		if topic == "wait" {
			return errors.New("waiting")
		}
		mu.Lock()
		acked[topic] = true
		mu.Unlock()
		return nil
	}, 20*time.Millisecond, nil)
	lane.add(delivery{topic: "wait", body: []byte("r"), ack: func() {
		mu.Lock()
		acked["wait"] = true
		mu.Unlock()
	}})
	lane.add(delivery{topic: "next", body: []byte("n"), ack: func() {}})
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		ok := acked["next"]
		waitAck := acked["wait"]
		mu.Unlock()
		if ok {
			if waitAck {
				t.Fatal("waiting reply was acknowledged")
			}
			cancel()
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("following message stayed behind a waiting reply")
}

func TestLaneCapsAndIsolatesPoison(t *testing.T) {
	lane := newLane()
	for i := 0; i < laneCap; i++ {
		if !lane.add(delivery{topic: "t", body: []byte{byte(i)}, hash: string(rune(i)) + "x"}) {
			t.Fatal("capacity rejected an empty slot")
		}
	}
	if lane.add(delivery{topic: "overflow", body: []byte("z"), hash: "overflow"}) {
		t.Fatal("queue grew past its cap")
	}
	if !lane.add(delivery{topic: "t", body: []byte{0}, hash: string(rune(0)) + "x", ack: func() {}}) {
		t.Fatal("duplicate was counted again")
	}

	isolated := newLane()
	acked := make(chan struct{})
	isolated.isolate = func(delivery) bool { return true }
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() {
		isolated.run(ctx, func(string, []byte) error { return errors.New("bad") }, time.Millisecond, nil)
		close(done)
	}()
	isolated.add(delivery{topic: "bad", body: []byte("p"), ack: func() {
		select {
		case <-acked:
		default:
			close(acked)
		}
	}})
	select {
	case <-acked:
	case <-time.After(2 * time.Second):
		t.Fatal("poison message was not isolated")
	}
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("lane did not stop")
	}
}
