package store

import "testing"

func TestCommandDispositionExitsEveryOpenState(t *testing.T) {
	cases := []struct {
		name                            string
		state                           string
		attempts                        int
		deadlinePassed, stale, eligible bool
		want                            string
	}{
		{name: "sending expired before third try", state: "sending", attempts: 1, deadlinePassed: true, stale: false, eligible: true, want: "timeout"},
		{name: "sending expired and stale", state: "sending", attempts: 2, deadlinePassed: true, stale: true, eligible: true, want: "timeout"},
		{name: "sending can retry", state: "sending", attempts: 1, deadlinePassed: false, stale: true, eligible: true, want: "pending"},
		{name: "sending exhausted while deadline remains", state: "sending", attempts: 3, deadlinePassed: false, stale: true, eligible: true, want: "needs_attention"},
		{name: "fresh sending stays", state: "sending", attempts: 1, deadlinePassed: false, stale: false, eligible: true, want: ""},
		{name: "stale sending on disabled device", state: "sending", attempts: 1, deadlinePassed: false, stale: true, eligible: false, want: "cancelled"},
		{name: "fresh sending on disabled device waits for send check", state: "sending", attempts: 1, deadlinePassed: false, stale: false, eligible: false, want: ""},
		{name: "expired sending on disabled device times out", state: "sending", attempts: 1, deadlinePassed: true, stale: true, eligible: false, want: "timeout"},
		{name: "pending on disabled device", state: "pending", attempts: 0, deadlinePassed: false, stale: false, eligible: false, want: "cancelled"},
		{name: "pending expired", state: "pending", attempts: 0, deadlinePassed: true, stale: false, eligible: true, want: "timeout"},
		{name: "sent expired", state: "sent", attempts: 1, deadlinePassed: true, stale: false, eligible: true, want: "timeout"},
		{name: "uncertain expired goes to a person", state: "uncertain", attempts: 1, deadlinePassed: true, stale: true, eligible: true, want: "needs_attention"},
		{name: "uncertain waits for reply", state: "uncertain", attempts: 1, deadlinePassed: false, stale: true, eligible: true, want: ""},
		{name: "accepted stays", state: "accepted", attempts: 1, deadlinePassed: true, stale: true, eligible: false, want: ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := commandDisposition(tc.state, tc.attempts, tc.deadlinePassed, tc.stale, tc.eligible)
			if got != tc.want {
				t.Fatalf("next = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestScreenSendRechecksDevice(t *testing.T) {
	if got := screenSend(false, true, true, false); got != "send" {
		t.Fatalf("open device = %s", got)
	}
	if got := screenSend(true, true, true, false); got != "timeout" {
		t.Fatalf("deadline = %s", got)
	}
	if got := screenSend(false, true, false, false); got != "cancelled" {
		t.Fatalf("disabled = %s", got)
	}
	if got := screenSend(false, true, true, true); got != "cancelled" {
		t.Fatalf("revoked = %s", got)
	}
	if got := screenSend(false, false, false, false); got != "cancelled" {
		t.Fatalf("missing = %s", got)
	}
}
