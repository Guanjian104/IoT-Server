package health

import (
	"sync"
	"testing"
)

func TestReadyStartsFalse(t *testing.T) {
	var state State
	if state.Ready() {
		t.Fatal("new state is ready")
	}
	state.SetReady(true)
	if !state.Ready() {
		t.Fatal("state was not set ready")
	}
}

func TestConcurrentAccess(t *testing.T) {
	var state State
	var wg sync.WaitGroup
	for i := 0; i < 32; i++ {
		wg.Add(2)
		go func() {
			defer wg.Done()
			state.SetReady(true)
			_ = state.Ready()
		}()
		go func() {
			defer wg.Done()
			state.SetReady(false)
		}()
	}
	wg.Wait()
}
