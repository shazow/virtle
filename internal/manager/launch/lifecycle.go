package launch

import (
	"context"
	"os"
	"os/signal"
	"sync"
	"syscall"
)

type Lifecycle struct {
	suspend    *SuspendCoordinator
	info       chan struct{}
	signalDone chan struct{}
	stopSignal func()
	stopOnce   sync.Once
}

type SuspendCoordinator struct {
	mu        sync.Mutex
	notify    chan struct{}
	waiters   []chan error
	requested bool
	inFlight  bool
	completed bool
	result    error
}

func NewLifecycle(signalCh <-chan os.Signal, stopSignals func(), cancel context.CancelFunc) *Lifecycle {
	if stopSignals == nil {
		stopSignals = func() {}
	}
	lifecycle := &Lifecycle{
		suspend:    NewSuspendCoordinator(),
		info:       make(chan struct{}, 1),
		signalDone: make(chan struct{}),
		stopSignal: stopSignals,
	}
	go lifecycle.watchSignals(signalCh, cancel)
	return lifecycle
}

func NewSignalLifecycle(signalCh <-chan os.Signal, cancel context.CancelFunc) *Lifecycle {
	if signalCh != nil {
		return NewLifecycle(signalCh, func() {}, cancel)
	}

	ch := make(chan os.Signal, 8)
	signal.Notify(ch, os.Interrupt, syscall.SIGTERM, syscall.SIGTSTP, syscall.SIGUSR1)
	return NewLifecycle(ch, func() {
		signal.Stop(ch)
		close(ch)
	}, cancel)
}

func NewSuspendCoordinator() *SuspendCoordinator {
	return &SuspendCoordinator{notify: make(chan struct{}, 1)}
}

func (l *Lifecycle) watchSignals(signalCh <-chan os.Signal, cancel context.CancelFunc) {
	for {
		select {
		case <-l.signalDone:
			return
		case sig, ok := <-signalCh:
			if !ok {
				return
			}
			switch sig {
			case os.Interrupt, syscall.SIGTERM:
				if cancel != nil {
					cancel()
				}
			case syscall.SIGTSTP:
				l.Suspend().Request()
			case syscall.SIGUSR1:
				l.RequestInfo()
			}
		}
	}
}

func (l *Lifecycle) Stop() {
	l.stopOnce.Do(func() {
		close(l.signalDone)
		l.stopSignal()
	})
}

func (l *Lifecycle) Suspend() *SuspendCoordinator {
	return l.suspend
}

func (l *Lifecycle) Info() <-chan struct{} {
	return l.info
}

func (l *Lifecycle) RequestInfo() {
	select {
	case l.info <- struct{}{}:
	default:
	}
}

func HandleQueuedSuspend(ctx context.Context, lifecycle *Lifecycle, handle func(context.Context, *SuspendCoordinator) error) error {
	select {
	case <-lifecycle.Suspend().Notify():
		return handle(ctx, lifecycle.Suspend())
	default:
		return nil
	}
}

func (c *SuspendCoordinator) Notify() <-chan struct{} {
	return c.notify
}

func (c *SuspendCoordinator) Request() {
	c.request(nil)
}

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

func (c *SuspendCoordinator) Begin() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.requested = false
	c.inFlight = true
}

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
