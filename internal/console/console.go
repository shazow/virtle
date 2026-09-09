// Package console fans a VMM's serial console out to a backend's
// ConsoleOutput writer and to attached vm.Term sessions. Both QEMU (with a
// stdio chardev) and Firecracker carry the guest's serial port on the VMM
// process's standard streams, so one Hub serves both: the process writes
// its stdout into the hub and reads its stdin from the hub's input pipe.
package console

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"sync"

	"github.com/shazow/virtle/vm"
)

const (
	// historyLimit bounds the recent output a newly attached Term receives
	// first, so a session attached after the guest booted still sees what
	// it printed.
	historyLimit = 256 << 10
	// pendingLimit bounds unread output per Term; a reader that falls
	// further behind is dropped (vm.ErrTermFellBehind) rather than
	// stalling the VMM's console.
	pendingLimit = 1 << 20
)

// Hub is one machine's serial console. It is an io.Writer for the VMM's
// standard output and owns the pipe behind its standard input.
type Hub struct {
	output io.Writer
	logger *slog.Logger
	input  *os.File // write end of the VMM's stdin pipe
	stdin  *os.File // read end, handed to the VMM process

	mu      sync.Mutex
	history []byte
	terms   map[*Term]struct{}
	closed  bool
}

// New returns a hub that copies console output to output (nil discards it)
// and to attached terminals. Dropped sessions are reported as warnings on
// logger (nil discards them); pass a logger that names the machine.
func New(output io.Writer, logger *slog.Logger) (*Hub, error) {
	stdin, input, err := os.Pipe()
	if err != nil {
		return nil, fmt.Errorf("console input pipe: %w", err)
	}
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	return &Hub{output: output, logger: logger, input: input, stdin: stdin, terms: map[*Term]struct{}{}}, nil
}

// Stdin is the file to hand the VMM process as its standard input.
func (h *Hub) Stdin() *os.File { return h.stdin }

// Started releases the hub's copy of the process's stdin end once the
// process holds its own; call it after the process started.
func (h *Hub) Started() { _ = h.stdin.Close() }

// Write implements io.Writer for the VMM's standard output: the bytes go to
// the output writer, into the bounded history, and to every attached Term.
// A Term whose reader fell behind is dropped and reported once.
func (h *Hub) Write(p []byte) (int, error) {
	if h.output != nil {
		_, _ = h.output.Write(p)
	}
	var dropped []*Term
	h.mu.Lock()
	h.history = append(h.history, p...)
	if excess := len(h.history) - historyLimit; excess > 0 {
		h.history = append([]byte(nil), h.history[excess:]...)
	}
	for t := range h.terms {
		if !t.push(p) {
			delete(h.terms, t)
			dropped = append(dropped, t)
		}
	}
	h.mu.Unlock()
	for _, t := range dropped {
		h.logger.Warn("console session dropped: its reader fell behind",
			"unread_bytes", t.unread(), "limit_bytes", pendingLimit)
	}
	return len(p), nil
}

// Attach returns a Term that reads the retained history followed by live
// output, and whose writes reach the guest's serial input. Attach after
// Close returns a Term that reads the history and then EOF.
func (h *Hub) Attach() vm.Term {
	h.mu.Lock()
	defer h.mu.Unlock()
	t := &Term{hub: h, pending: append([]byte(nil), h.history...)}
	t.cond = sync.NewCond(&t.mu)
	if h.closed {
		t.err = io.EOF
	} else {
		h.terms[t] = struct{}{}
	}
	return t
}

// Close ends the console once the VMM has exited (or never started):
// attached Terms read EOF after the output they have not consumed yet, and
// both ends of the input pipe close.
func (h *Hub) Close() error {
	h.mu.Lock()
	h.closed = true
	for t := range h.terms {
		t.end(io.EOF)
		delete(h.terms, t)
	}
	h.mu.Unlock()
	_ = h.stdin.Close() // already closed by Started when the VMM ran
	if err := h.input.Close(); err != nil && !errors.Is(err, os.ErrClosed) {
		return err
	}
	return nil
}

func (h *Hub) detach(t *Term) {
	h.mu.Lock()
	delete(h.terms, t)
	h.mu.Unlock()
}

// Term is a serial console session: raw bytes both ways, no window size and
// no exit status of its own.
type Term struct {
	hub *Hub

	mu      sync.Mutex
	cond    *sync.Cond
	pending []byte
	err     error // set once no more output will arrive
}

// push queues output for the reader; it reports false when the reader fell
// behind and the Term was ended. The hub holds its own lock while calling.
func (t *Term) push(p []byte) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.err != nil {
		return false
	}
	if len(t.pending)+len(p) > pendingLimit {
		t.err = fmt.Errorf("console session: %w (%d bytes unread)", vm.ErrTermFellBehind, len(t.pending))
		t.cond.Broadcast()
		return false
	}
	t.pending = append(t.pending, p...)
	t.cond.Broadcast()
	return true
}

// unread reports the output queued for the reader.
func (t *Term) unread() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return len(t.pending)
}

func (t *Term) end(err error) {
	t.mu.Lock()
	if t.err == nil {
		t.err = err
	}
	t.cond.Broadcast()
	t.mu.Unlock()
}

// Read returns console output, blocking until some arrives. It returns
// io.EOF once the machine has exited and the queued output is consumed, or
// an error wrapping vm.ErrTermFellBehind when the session was dropped.
func (t *Term) Read(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	for len(t.pending) == 0 && t.err == nil {
		t.cond.Wait()
	}
	if len(t.pending) == 0 {
		return 0, t.err
	}
	n := copy(p, t.pending)
	t.pending = t.pending[n:]
	return n, nil
}

// Write sends bytes to the guest's serial input.
func (t *Term) Write(p []byte) (int, error) {
	t.mu.Lock()
	err := t.err
	t.mu.Unlock()
	if err != nil && err != io.EOF {
		return 0, err
	}
	n, err := t.hub.input.Write(p)
	if err != nil {
		return n, fmt.Errorf("console input: %w", err)
	}
	return n, nil
}

// Close detaches the session; the machine keeps running.
func (t *Term) Close() error {
	t.hub.detach(t)
	t.end(io.EOF)
	return nil
}

// Resize reports errors.ErrUnsupported: a serial console has no window.
func (t *Term) Resize(cols, rows int) error {
	return fmt.Errorf("serial console has no window size: %w", errors.ErrUnsupported)
}

// Wait reports errors.ErrUnsupported: a serial console has no exit status
// of its own; wait for the machine instead.
func (t *Term) Wait(ctx context.Context) (int, error) {
	return 0, fmt.Errorf("serial console has no exit status: %w", errors.ErrUnsupported)
}

var _ vm.Term = (*Term)(nil)
