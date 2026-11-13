package streamctl

import (
	"context"
	"testing"
)

func TestScriptRepeatsFirstOutcome(t *testing.T) {
	script := &Script{StartOutcome: "timeout", StopOutcome: "unknown", StatusKnown: true}
	req := Request{RequestID: "r1", Generation: 3}
	first, err := script.Start(context.Background(), req)
	if err != nil || first.Outcome != "timeout" {
		t.Fatal(first, err)
	}
	script.StartOutcome = "success"
	second, err := script.Start(context.Background(), req)
	if err != nil || second.Outcome != "timeout" {
		t.Fatal("repeat changed", second, err)
	}
	stopped, err := script.Stop(context.Background(), req)
	if err != nil || stopped.Outcome != "unknown" {
		t.Fatal(stopped, err)
	}
	if _, err = Interpret("later"); err != ErrOutcome {
		t.Fatal("bad outcome accepted")
	}
}
