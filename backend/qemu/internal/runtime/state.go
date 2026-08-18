package runtime

import (
	"sync"

	"github.com/shazow/virtle/internal/control"
)

type state struct {
	mu    sync.Mutex
	value control.RuntimeState
}

func newState(initial control.RuntimeState) *state {
	return &state{value: initial}
}

func (s *state) Set(value control.RuntimeState) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.value = value
}

func (s *state) Current() control.RuntimeState {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.value
}
