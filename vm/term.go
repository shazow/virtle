package vm

import (
	"context"
	"errors"
	"io"
)

// ErrTermFellBehind ends a Term whose reader stopped consuming output.
// Sources with bounded buffering (the serial console) drop such a session
// rather than stall the guest: Read returns an error wrapping it once the
// output queued before the drop is consumed, Write fails with it, and a
// fresh session can be opened in its place.
var ErrTermFellBehind = errors.New("terminal reader fell behind")

// Term is an interactive terminal session inside a guest: interleaved
// stdio as a stream, window resizing, and an exit status. Backend serial
// consoles expose it through backend.ConsoleProvider.
type Term interface {
	io.ReadWriteCloser
	// Resize updates the session's window size. Sources without resize
	// semantics (a raw serial console) return an error wrapping
	// errors.ErrUnsupported.
	Resize(cols, rows int) error
	// Wait blocks until the session ends and returns its exit code.
	// Sources without exit semantics return an error wrapping
	// errors.ErrUnsupported.
	Wait(ctx context.Context) (int, error)
}
