package vm

import (
	"context"
	"fmt"
	"io"
	"io/fs"
)

// Guest performs operations inside a running VM. The QEMU backend implements
// it through the QEMU Guest Agent. Implementations must be safe for concurrent
// use.
type Guest interface {
	// Run executes a command to completion. A non-zero exit status is returned
	// as an error satisfying errors.As(err, *ExitError).
	Run(ctx context.Context, cmd *GuestCmd) error
	// Open opens the named guest file for reading.
	Open(ctx context.Context, name string) (io.ReadCloser, error)
	// Create creates or truncates the named guest file for writing.
	Create(ctx context.Context, name string, mode fs.FileMode) (io.WriteCloser, error)
	// Shutdown requests a graceful guest shutdown.
	Shutdown(ctx context.Context) error
}

// GuestCmd describes a command to run inside the guest, mirroring exec.Cmd.
type GuestCmd struct {
	Path           string
	Args, Env      []string
	Dir            string
	Stdout, Stderr io.Writer // nil discards output
}

// ExitError reports an unsuccessful guest command. Stderr contains captured
// standard error when the command did not provide its own Stderr writer.
type ExitError struct {
	Code   int
	Stderr []byte
}

func (e *ExitError) Error() string {
	if e == nil {
		return ""
	}
	return fmt.Sprintf("guest command exited with status %d", e.Code)
}
