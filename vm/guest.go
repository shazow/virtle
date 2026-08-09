package vm

import (
	"context"
	"io"
	"io/fs"
)

// Guest performs operations inside a running VM.
//
// Implementations are the host-side client of whatever guest agent the VM
// runs — the QEMU guest agent today, virtle's own guest daemon later. The
// shapes here are os/exec- and io/fs-flavored, never protocol-flavored, so
// swapping the agent does not change call sites.
//
// Implementations must be safe for concurrent use. Operations that a
// particular agent cannot perform return an error wrapping
// errors.ErrUnsupported.
type Guest interface {
	// Run executes a command to completion and buffers its output, the
	// exec.Cmd.Output analog. A non-zero exit is reported in the result, not
	// as an error; err is non-nil only when the command could not be run or
	// its outcome could not be determined. Streaming exec arrives later as
	// an extension interface.
	Run(ctx context.Context, cmd *GuestCmd) (*GuestResult, error)

	// Open opens a guest file for reading.
	Open(ctx context.Context, name string) (io.ReadCloser, error)

	// Create creates or truncates a guest file for writing. The contents are
	// not guaranteed to be complete until the returned writer is closed.
	Create(ctx context.Context, name string, mode fs.FileMode) (io.WriteCloser, error)

	// Shutdown asks the guest operating system to power down. It returns
	// once the request is accepted, not once the VM has exited — wait on the
	// instance for that.
	Shutdown(ctx context.Context) error

	// Close releases the connection to the guest agent. It does not stop the
	// VM.
	Close() error
}

// GuestCmd describes a command to run inside the guest, in the shape of
// os/exec.Cmd.
type GuestCmd struct {
	// Path is the command to run, resolved by the guest.
	Path string

	// Args holds the arguments after the command name. Unlike exec.Cmd.Args,
	// it does not repeat Path.
	Args []string

	// Env holds "key=value" entries added to the guest environment.
	Env []string

	// Dir is the guest working directory. Empty uses the agent's own.
	Dir string

	// Stdin is fed to the command's standard input. Nil means no input.
	Stdin io.Reader
}

// GuestResult is the outcome of a completed [Guest.Run].
type GuestResult struct {
	// ExitCode is the command's exit status.
	ExitCode int

	// Stdout and Stderr hold the buffered output streams.
	Stdout, Stderr []byte
}
