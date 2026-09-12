package vm

import (
	"archive/tar"
	"bytes"
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
	// Close releases the host side of the guest connection.
	Close() error
}

// GuestCmd describes a command to run inside the guest, mirroring exec.Cmd.
type GuestCmd struct {
	Path           string
	Args, Env      []string
	Dir            string
	Stdin          io.Reader
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

// Output runs cmd and returns its standard output, like exec.Cmd.Output.
// It returns an error when cmd already has a Stdout writer.
func Output(ctx context.Context, g Guest, cmd *GuestCmd) ([]byte, error) {
	if cmd == nil {
		return nil, fmt.Errorf("guest command is required")
	}
	if cmd.Stdout != nil {
		return nil, fmt.Errorf("guest command Stdout is already set")
	}
	var stdout bytes.Buffer
	cmd.Stdout = &stdout
	defer func() { cmd.Stdout = nil }()
	err := g.Run(ctx, cmd)
	return stdout.Bytes(), err
}

// ArchiveFS returns a reader that produces a tar archive of fsys as it is
// read. Callers can pass os.DirFS(path), an embed.FS, or a fstest.MapFS.
// Generation errors surface from Read. Close the reader to release resources
// when the archive is not read to completion.
func ArchiveFS(fsys fs.FS) io.ReadCloser {
	pr, pw := io.Pipe()
	go func() {
		tw := tar.NewWriter(pw)
		err := tw.AddFS(fsys)
		if err == nil {
			err = tw.Close()
		}
		pw.CloseWithError(err)
	}()
	return pr
}
