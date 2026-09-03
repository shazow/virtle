package launch

import (
	"context"
	"sync"
)

// SuspendCoordinator queues suspend requests for a running launch and
// reports the outcome to every waiter once the request has been serviced.
// Requests arrive from the control socket and from foreground job control;
// the launch that owns the VM services them.
type SuspendCoordinator struct {
	mu        sync.Mutex
	notify    chan struct{}
	waiters   []chan error
	requested bool
	inFlight  bool
	completed bool
	result    error
}

func NewSuspendCoordinator() *SuspendCoordinator {
	return &SuspendCoordinator{notify: make(chan struct{}, 1)}
}

// HandleQueuedSuspend services a suspend request that was queued before the
// launch started servicing requests. It returns nil when none is pending.
func HandleQueuedSuspend(ctx context.Context, coordinator *SuspendCoordinator, handle func(context.Context, *SuspendCoordinator) error) error {
	select {
	case <-coordinator.Notify():
		return handle(ctx, coordinator)
	default:
		return nil
	}
}

// Notify reports pending requests; it is signaled at most once per request.
func (c *SuspendCoordinator) Notify() <-chan struct{} {
	return c.notify
}

// Request queues a suspend without waiting for its outcome.
func (c *SuspendCoordinator) Request() {
	c.request(nil)
}

// RequestAndWait queues a suspend and blocks until it completes or ctx ends.
func (c *SuspendCoordinator) RequestAndWait(ctx context.Context) error {
	done := make(chan error, 1)
	c.request(done)
	select {
	case err := <-done:
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (c *SuspendCoordinator) request(done chan error) {
	c.mu.Lock()
	if c.completed {
		result := c.result
		c.mu.Unlock()
		if done != nil {
			done <- result
		}
		return
	}
	if done != nil {
		c.waiters = append(c.waiters, done)
	}
	notify := false
	if !c.requested && !c.inFlight {
		c.requested = true
		notify = true
	}
	c.mu.Unlock()

	if notify {
		select {
		case c.notify <- struct{}{}:
		default:
		}
	}
}

// Begin marks the queued request as in flight.
func (c *SuspendCoordinator) Begin() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.requested = false
	c.inFlight = true
}

// Complete records the outcome and releases every waiter.
func (c *SuspendCoordinator) Complete(err error) {
	c.mu.Lock()
	c.inFlight = false
	c.completed = true
	c.result = err
	waiters := c.waiters
	c.waiters = nil
	c.mu.Unlock()

	for _, waiter := range waiters {
		waiter <- err
	}
}
