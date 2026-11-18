package rtc

import (
	"testing"
	"time"
)

func TestSessionPath(t *testing.T) {
	state := "CREATED"
	var err error
	for _, event := range []string{"open", "start"} {
		state, err = NextSession(state, event, false, false)
		if err != nil {
			t.Fatal(event, err)
		}
	}
	if state != "STREAM_STARTING" {
		t.Fatal(state)
	}
	stayed, err := NextSession(state, "mark", true, false)
	if err != nil || stayed != "STREAM_STARTING" {
		t.Fatalf("partial ready %s %v", stayed, err)
	}
	state, err = NextSession(state, "mark", true, true)
	if err != nil || state != "ACTIVE" {
		t.Fatal(state, err)
	}
	state, err = NextSession(state, "empty", true, true)
	if err != nil || state != "STOP_PENDING" {
		t.Fatal(state, err)
	}
	state, err = NextSession(state, "resume", true, true)
	if err != nil || state != "ACTIVE" {
		t.Fatal(state, err)
	}
	state, _ = NextSession(state, "empty", true, true)
	state, err = NextSession(state, "stop", true, true)
	if err != nil || state != "STOPPING" {
		t.Fatal(state, err)
	}
	if _, err = NextSession(state, "resume", true, true); err != ErrState {
		t.Fatal("stopping reused")
	}
	state, err = NextSession(state, "closed", true, true)
	if err != nil || state != "CLOSED" {
		t.Fatal(state, err)
	}
	if _, err = NextSession(state, "open", true, true); err != ErrState {
		t.Fatal("closed reopened")
	}
}

func TestMediaInterruptEndsTheRound(t *testing.T) {
	for _, from := range []string{"CREATED", "WAITING_DEVICE", "STREAM_STARTING", "ACTIVE", "STOP_PENDING"} {
		next, err := NextSession(from, "interrupt", true, false)
		if err != nil || next != "FAILED" {
			t.Fatalf("%s -> %s %v", from, next, err)
		}
	}
	next, err := NextSession("STOPPING", "interrupt", true, false)
	if err != nil || next != "CLOSED" {
		t.Fatalf("stopping -> %s %v", next, err)
	}
	if _, err := NextSession("CLOSED", "interrupt", true, true); err != ErrState {
		t.Fatal("closed moved")
	}
	if _, err := NextSession("FAILED", "interrupt", true, true); err != ErrState {
		t.Fatal("failed moved")
	}
}

func TestViewerAndCall(t *testing.T) {
	if next, err := NextViewer("LEFT", "heartbeat"); err != ErrState || next != "LEFT" {
		t.Fatal("heartbeat revived left")
	}
	if next, err := NextViewer("DISCONNECTED", "reconnect"); err != nil || next != "JOINING" {
		t.Fatal(next, err)
	}
	if next, err := NextViewer("ACTIVE", "leave"); err != nil || next != "LEFT" {
		t.Fatal(next, err)
	}
	if _, err := NextViewer("LEFT", "leave"); err != ErrState {
		t.Fatal("second leave changed state")
	}
	state, err := NextCall("CREATED", "ring")
	if err != nil || state != "RINGING" {
		t.Fatal(state, err)
	}
	if _, err = NextCall(state, "accept"); err != nil {
		t.Fatal(err)
	}
	ended, err := NextCall("RINGING", "timeout")
	if err != nil || ended != "TIMEOUT" {
		t.Fatal(ended, err)
	}
	if next, err := NextCall("ACCEPTED", "timeout"); err != nil || next != "TIMEOUT" {
		t.Fatal(next, err)
	}
	if !SameRound("s1", 2, "s1", 2) || SameRound("s1", 2, "s2", 2) || SameRound("s1", 2, "s1", 1) {
		t.Fatal("call round")
	}
	if !CanOperateCall(2, 1, 2, 0, "RINGING", "accept") || CanOperateCall(1, 1, 2, 0, "RINGING", "accept") || CanOperateCall(3, 1, 2, 0, "RINGING", "accept") {
		t.Fatal("accept party")
	}
	if !CanOperateCall(2, 1, 2, 0, "RINGING", "reject") || CanOperateCall(3, 1, 2, 0, "RINGING", "end") {
		t.Fatal("reject party")
	}
	if !CanOperateCall(1, 1, 2, 0, "RINGING", "end") || !CanOperateCall(2, 1, 2, 2, "CONNECTED", "end") || CanOperateCall(3, 1, 2, 2, "CONNECTED", "end") {
		t.Fatal("end party")
	}
	if _, err = NextCall(ended, "end"); err != ErrState {
		t.Fatal("terminal call moved")
	}
}

func TestOccupancyAndGeneration(t *testing.T) {
	if !ViewerOccupies("JOINING", time.Second, 2*time.Second) {
		t.Fatal("joining inside grace")
	}
	if ViewerOccupies("JOINING", 2*time.Second, 2*time.Second) || ViewerOccupies("LEFT", 0, time.Second) || ViewerOccupies("DISCONNECTED", 0, time.Second) {
		t.Fatal("non occupant counted")
	}
	if !ViewerOccupies("ACTIVE", time.Second, 2*time.Second) {
		t.Fatal("fresh active viewer")
	}
	if ViewerOccupies("ACTIVE", 10*time.Hour, time.Second) || !CallOccupies("RINGING") || CallOccupies("ENDED") {
		t.Fatal("occupant rule")
	}
	if AcceptEvent(2, 1) || !AcceptEvent(2, 2) || AcceptEvent(0, 0) {
		t.Fatal("generation")
	}
}
