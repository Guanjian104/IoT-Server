package presence

import "testing"

func TestLateDisconnectDoesNotClearNewerSession(t *testing.T) {
	current, ok := Apply(Snapshot{}, Event{Kind: Connected, ConnectedAt: 100})
	if !ok || !current.Online || current.Generation != 100 {
		t.Fatalf("connect = %+v", current)
	}
	offline, ok := Apply(current, Event{Kind: Disconnected, ConnectedAt: 100, Disconnected: 150})
	if !ok || offline.Online || offline.DisconnectedAt != 150 || offline.Generation != 100 {
		t.Fatalf("disconnect = %+v %v", offline, ok)
	}
	newer, ok := Apply(offline, Event{Kind: Connected, ConnectedAt: 200})
	if !ok || !newer.Online || newer.Generation != 200 {
		t.Fatalf("reconnect = %+v", newer)
	}
	stale, ok := Apply(newer, Event{Kind: Disconnected, ConnectedAt: 100, Disconnected: 180})
	if ok || !stale.Online || stale.Generation != 200 {
		t.Fatalf("stale disconnect applied: %+v %v", stale, ok)
	}
	ignored, ok := Apply(newer, Event{Kind: Disconnected, EventAt: 150})
	if ok || !ignored.Online {
		t.Fatalf("older timestamp applied: %+v", ignored)
	}
}
