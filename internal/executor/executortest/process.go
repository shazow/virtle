package executortest

import (
	"os"
	"sync"
	"sync/atomic"

	"github.com/shazow/virtle/internal/executor"
)

// EventKind identifies a lifecycle event recorded by Process.
type EventKind string

const (
	EventSignal EventKind = "signal"
	EventKill   EventKind = "kill"
)

var eventSequence atomic.Int64

// Event records a signal or kill operation on a Process.
type Event struct {
	Kind     EventKind
	Signal   os.Signal
	Sequence int64
}

// Process is a test implementation of RunningProcess.
//
// The zero value is a running process named "fake" with PID 1. Set Exited to
// true to make Wait return immediately with WaitErr.
type Process struct {
	OverrideName string
	OverridePID  int

	Exited  bool
	WaitErr error

	SignalErr     error
	KillErr       error
	IgnoreSignals bool

	OnSignal func(os.Signal)
	OnKill   func()

	mu        sync.Mutex
	done      chan struct{}
	completed bool
	waitErr   error
	signals   []os.Signal
	events    []Event
}

var _ executor.RunningProcess = (*Process)(nil)

// Process wraps p as an executor Process.
func (p *Process) Process() *executor.Process {
	return executor.Wrap(p)
}

// Complete marks p as exited and unblocks Wait.
func (p *Process) Complete(err error) {
	if p == nil {
		return
	}
	p.complete(err)
}

// Done returns a channel closed when p exits.
func (p *Process) Done() <-chan struct{} {
	if p == nil {
		return closedDone()
	}
	p.mu.Lock()
	done := p.doneLocked()
	p.mu.Unlock()
	return done
}

func closedDone() <-chan struct{} {
	done := make(chan struct{})
	close(done)
	return done
}

// Wait blocks until p exits and returns its completion error.
func (p *Process) Wait() error {
	if p == nil {
		return nil
	}
	p.mu.Lock()
	done := p.doneLocked()
	p.mu.Unlock()
	<-done

	p.mu.Lock()
	defer p.mu.Unlock()
	return p.waitErr
}

// Signal records sig, invokes OnSignal, and completes p unless IgnoreSignals is set.
func (p *Process) Signal(sig os.Signal) error {
	if p == nil {
		return nil
	}
	p.recordEvent(EventSignal, sig)
	if p.OnSignal != nil {
		p.OnSignal(sig)
	}
	if p.SignalErr != nil {
		return p.SignalErr
	}
	if !p.IgnoreSignals {
		p.complete(p.WaitErr)
	}
	return nil
}

// Kill records a kill event, invokes OnKill, and completes p.
func (p *Process) Kill() error {
	if p == nil {
		return nil
	}
	p.recordEvent(EventKill, nil)
	if p.OnKill != nil {
		p.OnKill()
	}
	if p.KillErr != nil {
		return p.KillErr
	}
	p.complete(p.WaitErr)
	return nil
}

// PID returns OverridePID, or 1 when OverridePID is zero.
func (p *Process) PID() int {
	if p == nil {
		return 0
	}
	if p.OverridePID != 0 {
		return p.OverridePID
	}
	return 1
}

// Name returns OverrideName, or "fake" when OverrideName is empty.
func (p *Process) Name() string {
	if p == nil {
		return ""
	}
	if p.OverrideName != "" {
		return p.OverrideName
	}
	return "fake"
}

// Signals returns the signals recorded by Signal.
func (p *Process) Signals() []os.Signal {
	if p == nil {
		return nil
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]os.Signal(nil), p.signals...)
}

// Events returns signal and kill events in the order p observed them.
func (p *Process) Events() []Event {
	if p == nil {
		return nil
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]Event(nil), p.events...)
}

// EventKinds returns the kinds of events recorded by p.
func (p *Process) EventKinds() []EventKind {
	if p == nil {
		return nil
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	kinds := make([]EventKind, 0, len(p.events))
	for _, event := range p.events {
		kinds = append(kinds, event.Kind)
	}
	return kinds
}

func (p *Process) complete(err error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.completed {
		return
	}
	done := p.doneLocked()
	p.waitErr = err
	p.completed = true
	close(done)
}

func (p *Process) doneLocked() chan struct{} {
	if p.done == nil {
		p.done = make(chan struct{})
		if p.Exited {
			p.waitErr = p.WaitErr
			p.completed = true
			close(p.done)
		}
	}
	return p.done
}

func (p *Process) recordEvent(kind EventKind, sig os.Signal) {
	event := Event{
		Kind:     kind,
		Signal:   sig,
		Sequence: eventSequence.Add(1),
	}
	p.mu.Lock()
	if kind == EventSignal {
		p.signals = append(p.signals, sig)
	}
	p.events = append(p.events, event)
	p.mu.Unlock()
}
