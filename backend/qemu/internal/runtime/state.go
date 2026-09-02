package runtime

import (
	"context"
	"sync"

	"github.com/shazow/virtle/internal/control"
)

type state struct {
	mu    sync.Mutex
	value control.RuntimeState
	done  chan struct{}
	err   error
	once  sync.Once
}

func newState(initial control.RuntimeState) *state {
	return &state{value: initial, done: make(chan struct{})}
}

func (s *state) Finish(err error) {
	s.once.Do(func() {
		s.mu.Lock()
		s.err = err
		s.mu.Unlock()
		close(s.done)
	})
}

func (s *state) Wait(ctx context.Context) error {
	select {
	case <-s.done:
		s.mu.Lock()
		defer s.mu.Unlock()
		return s.err
	case <-ctx.Done():
		return context.Cause(ctx)
	}
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
