package vm

import (
	"context"
	"io"
)

// Term is an interactive terminal session inside a guest: interleaved
// stdio as a stream, window resizing, and an exit status. It is the one
// session shape every interactive source returns — the backend serial
// console (backend.ConsoleProvider), an SSH session to the guest daemon's
// embedded sshd, or future transports — mirroring x/crypto/ssh.Session and
// creack/pty.
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

// TermOptions configures a new Term session. The zero value requests the
// source's default shell with its default size.
type TermOptions struct {
	Argv     []string // command to run; default: a login shell
	Env      []string
	TermType string // terminal type advertised to the guest, e.g. "xterm-256color"
	Cols     int
	Rows     int
}
