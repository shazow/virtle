package runtime

import (
	"sync"
	"testing"

	"github.com/shazow/virtle/internal/control"
)

func TestStateTracksCurrentValue(t *testing.T) {
	state := newState(control.RuntimeStarting)
	if got := state.Current(); got != control.RuntimeStarting {
		t.Fatalf("initial state: got %q want %q", got, control.RuntimeStarting)
	}

	state.Set(control.RuntimeReady)
	if got := state.Current(); got != control.RuntimeReady {
		t.Fatalf("updated state: got %q want %q", got, control.RuntimeReady)
	}
}

func TestStateSupportsConcurrentAccess(t *testing.T) {
	state := newState(control.RuntimeStarting)
	var wg sync.WaitGroup
	for range 100 {
		wg.Add(2)
		go func() {
			defer wg.Done()
			state.Set(control.RuntimeReady)
		}()
		go func() {
			defer wg.Done()
			_ = state.Current()
		}()
	}
	wg.Wait()
}
