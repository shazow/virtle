package vm

import "io"

// Term is an interactive session with a guest: a terminal's worth of
// interleaved stdio, plus the two things every terminal consumer needs and
// io.ReadWriteCloser cannot express — window resize and an exit status.
//
// It is the shape every interactive source returns, whatever provides it: a
// backend's serial console (the no-agent debug path), an SSH session, or a
// guest daemon's pty channel. Consumers write against Term and stay
// indifferent to which one they got.
type Term interface {
	// Read, Write, and Close carry the session's stdio. Reads see the
	// combined output stream, the way a terminal does.
	io.ReadWriteCloser

	// Resize reports a new window size to the guest.
	Resize(cols, rows int) error

	// Wait blocks until the session ends and returns its exit status.
	// Sources that cannot report one — a raw serial console — return an
	// error wrapping errors.ErrUnsupported.
	Wait() (exitCode int, err error)
}

// TermOptions configures an interactive session. The zero value asks for the
// guest's default shell with no pty.
type TermOptions struct {
	// Argv is the command to run. Empty starts the guest's login shell.
	Argv []string

	// Env holds "key=value" entries added to the session environment.
	Env []string

	// TERM names the terminal type. Empty requests no pty.
	TERM string

	// Cols and Rows are the initial window size, used when TERM is set.
	Cols, Rows int
}
