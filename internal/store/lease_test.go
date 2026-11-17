package store

import "testing"

func TestOwnsLease(t *testing.T) {
	task := rtcTask{Owner: "worker-a", Token: 3}
	if !ownsLease("leased", "worker-a", 3, task) {
		t.Fatal("current lease was rejected")
	}
	if ownsLease("pending", "worker-a", 3, task) || ownsLease("leased", "worker-b", 3, task) || ownsLease("leased", "worker-a", 2, task) || ownsLease("leased", "worker-a", 0, task) {
		t.Fatal("stale lease was accepted")
	}
}
