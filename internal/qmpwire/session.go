package qmpwire

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"sync"
	"time"
)

// DefaultRPCTimeout bounds a single round trip on a QEMU control socket when
// the dialer does not configure one.
const DefaultRPCTimeout = 15 * time.Second

// WireError marks a connection-level failure — a failed write, read, or
// decode — after which the stream position is unknown and the connection must
// not be reused.
type WireError struct {
	Err error
}

func (e *WireError) Error() string { return e.Err.Error() }

func (e *WireError) Unwrap() error { return e.Err }

// IsWireError reports whether err carries a WireError.
func IsWireError(err error) bool {
	var wireErr *WireError
	return errors.As(err, &wireErr)
}

// ErrBroken reports an operation attempted on a session whose stream is no
// longer trusted.
var ErrBroken = errors.New("connection unusable after earlier failure")

// EnvelopeError is a server-reported command failure. The stream stays usable
// after one: the response was fully read.
type EnvelopeError struct {
	Class string `json:"class"`
	Desc  string `json:"desc"`
}

func (e *EnvelopeError) Error() string {
	if e.Desc != "" {
		return e.Desc
	}
	return "command failed with " + e.Class
}

// Envelope is one message from a QEMU line-delimited JSON socket.
type Envelope struct {
	Raw    json.RawMessage `json:"-"`
	Return json.RawMessage `json:"return"`
	Event  string          `json:"event,omitempty"`
	Data   json.RawMessage `json:"data,omitempty"`
	Error  *EnvelopeError  `json:"error,omitempty"`
}

// DecodeEnvelope reads the next message from decoder. Failures are
// wire-level: a stream that cannot be decoded cannot be trusted.
func DecodeEnvelope(decoder *json.Decoder) (Envelope, error) {
	var raw json.RawMessage
	if err := decoder.Decode(&raw); err != nil {
		return Envelope{}, &WireError{Err: err}
	}
	var envelope Envelope
	if err := json.Unmarshal(raw, &envelope); err != nil {
		return Envelope{}, &WireError{Err: err}
	}
	envelope.Raw = raw
	return envelope, nil
}

// DialUnix connects to a unix socket and classifies failures: the caller's
// ctx ending wins, timeouts are labeled with subject, and other errors pass
// through.
func DialUnix(ctx context.Context, socketPath string, timeout time.Duration, subject string) (net.Conn, error) {
	dialer := net.Dialer{Timeout: timeout}
	conn, err := dialer.DialContext(ctx, "unix", socketPath)
	if err != nil {
		return nil, DialError(ctx, err, subject, timeout)
	}
	if ctx.Err() != nil {
		_ = conn.Close()
		return nil, context.Cause(ctx)
	}
	return conn, nil
}

// DialError classifies a connection-establishment failure the same way
// DialUnix does.
func DialError(ctx context.Context, err error, subject string, timeout time.Duration) error {
	if ctx.Err() != nil {
		return context.Cause(ctx)
	}
	if IsTimeout(err) {
		return fmt.Errorf("%s connect timed out after %s", subject, timeout)
	}
	return err
}

// Session serializes operations on one line-delimited JSON connection. It
// arbitrates the socket deadline between the per-RPC liveness bound and the
// caller's ctx, interrupts a blocked read when ctx ends, and marks the
// session broken once the stream position is unknown — after an interrupted
// operation or a wire-level failure — so later operations fail immediately
// instead of reading a stale reply.
type Session struct {
	Conn net.Conn
	// RPCTimeout bounds each operation run through Do regardless of the
	// caller's ctx deadline, so a wedged peer fails fast even when the
	// operation itself has no time limit. Must be positive.
	RPCTimeout time.Duration

	mu     sync.Mutex
	broken error
}

// Do runs one operation with the connection deadline set to the RPC liveness
// bound. The ctx deadline is enforced by the cancellation interrupt, which
// runs strictly after ctx is marked done, so a failure is always attributable
// to exactly one of the two bounds.
func (s *Session) Do(ctx context.Context, fn func() error) error {
	return s.do(ctx, time.Now().Add(s.RPCTimeout), fn)
}

// DoSlow runs one operation bounded only by ctx, for commands whose reply
// legitimately lags past the RPC liveness bound.
func (s *Session) DoSlow(ctx context.Context, fn func() error) error {
	return s.do(ctx, time.Time{}, fn)
}

func (s *Session) do(ctx context.Context, deadline time.Time, fn func() error) (err error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.broken != nil {
		// %v (not %w) so the original failure cannot be re-matched as a
		// timeout or cancellation by callers mapping this fresh failure.
		return fmt.Errorf("%w: %v", ErrBroken, s.broken)
	}

	// A zero deadline means no time limit; setting it unconditionally also
	// clears anything left behind by an earlier interrupt.
	if err := s.Conn.SetDeadline(deadline); err != nil {
		return err
	}
	defer s.Conn.SetDeadline(time.Time{})

	// A ctx that can never end needs no interrupt plumbing.
	if ctx.Done() == nil {
		defer func() {
			if err != nil && IsWireError(err) {
				s.broken = err
			}
		}()
		return fn()
	}

	interrupted := false
	stopc := make(chan struct{})
	stop := context.AfterFunc(ctx, func() {
		_ = s.Conn.SetDeadline(time.Now())
		close(stopc)
	})
	defer func() {
		// Wait for a fired AfterFunc before releasing the mutex so it cannot
		// clobber the deadline set by the next operation.
		if !stop() {
			<-stopc
			interrupted = true
		}
		if err != nil && (interrupted || IsWireError(err)) {
			// The reply may still arrive later, so the stream position is
			// unknown from here on.
			s.broken = err
		}
	}()

	return fn()
}
