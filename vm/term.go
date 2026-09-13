package vm

import (
	"errors"
	"io"
)

// ErrTermFellBehind ends a Term whose reader stopped consuming output.
// Sources with bounded buffering (the serial console) drop such a session
// rather than stall the guest: Read returns an error wrapping it once the
// output queued before the drop is consumed, Write fails with it, and a
// fresh session can be opened in its place.
var ErrTermFellBehind = errors.New("terminal reader fell behind")

// Term is an interactive stream connected to a guest's serial console.
// Backend serial consoles expose it through backend.ConsoleProvider.
// Closing the stream detaches the session and leaves the machine running.
type Term interface {
	io.ReadWriteCloser
}
