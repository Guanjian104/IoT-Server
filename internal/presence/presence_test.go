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

func TestDuplicateSessionDoesNotRewrite(t *testing.T) {
	current, ok := Apply(Snapshot{}, Event{Kind: Connected, ConnectedAt: 100})
	if !ok {
		t.Fatal("first connect was ignored")
	}
	again, ok := Apply(current, Event{Kind: Connected, ConnectedAt: 100})
	if ok || again != current {
		t.Fatalf("duplicate connect = %+v %v", again, ok)
	}
	offline, ok := Apply(current, Event{Kind: Disconnected, ConnectedAt: 100, Disconnected: 150})
	if !ok || offline.Online || offline.DisconnectedAt != 150 {
		t.Fatalf("disconnect = %+v %v", offline, ok)
	}
	replayed, ok := Apply(offline, Event{Kind: Connected, ConnectedAt: 100})
	if ok || replayed.Online || replayed != offline {
		t.Fatalf("replayed connect = %+v %v", replayed, ok)
	}
	repeated, ok := Apply(offline, Event{Kind: Disconnected, ConnectedAt: 100, Disconnected: 180})
	if ok || repeated != offline {
		t.Fatalf("duplicate disconnect = %+v %v", repeated, ok)
	}
}

func TestCalibrateRepairsMissedSession(t *testing.T) {
	offline := Snapshot{Generation: 100, ConnectedAt: 100, DisconnectedAt: 150}
	restored, ok := Calibrate(offline, true, 100)
	if !ok || !restored.Online || restored.Generation != 100 || restored.ConnectedAt != 100 {
		t.Fatalf("restore = %+v %v", restored, ok)
	}
	same, ok := Calibrate(restored, true, 100)
	if ok || same != restored {
		t.Fatalf("repeat sighting = %+v %v", same, ok)
	}
	replaced, ok := Calibrate(restored, true, 250)
	if !ok || !replaced.Online || replaced.Generation != 250 || replaced.ConnectedAt != 250 {
		t.Fatalf("newer sighting = %+v %v", replaced, ok)
	}
	missed, ok := Calibrate(replaced, false, 0)
	if !ok || missed.Online || missed.Generation != 250 {
		t.Fatalf("missed disconnect = %+v %v", missed, ok)
	}
	still, ok := Calibrate(missed, false, 0)
	if ok || still != missed {
		t.Fatalf("repeat absence = %+v %v", still, ok)
	}
	older, ok := Calibrate(missed, true, 80)
	if !ok || !older.Online || older.Generation != 250 {
		t.Fatalf("older sighting = %+v %v", older, ok)
	}
}
