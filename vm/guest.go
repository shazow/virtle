package vm

import (
	"archive/tar"
	"bytes"
	"context"
	"fmt"
	"io"
	"io/fs"
)

// Guest performs operations inside a running VM. Implemented today by
// backend/qemu's QEMU Guest Agent adapter; a virtle-native guest daemon
// client is planned. Shapes are os/exec- and io/fs-flavored, never
// protocol-flavored. Implementations must be safe for concurrent use.
//
// Daemon-only features (file-tree copy, streaming exec, file watching, ...)
// are GuestWithX extension interfaces discovered by type assertion; they
// are never added to Guest itself.
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

// GuestWithCopy streams file trees between host and guest as tar archives.
// Both directions speak the same shape (the Docker CopyToContainer /
// CopyFromContainer model), so a guest->guest copy is a direct pipe with no
// host filesystem and no buffering, and transforms compose as ordinary
// io.Reader middleware. No in-tree implementation exists yet; it is
// reserved for the planned guest daemon client.
type GuestWithCopy interface {
	CopyToGuest(ctx context.Context, guestPath string, archive io.Reader, opts CopyOptions) error
	CopyFromGuest(ctx context.Context, guestPath string) (io.ReadCloser, error)
}

// CopyOptions carries the options prior art shows are necessary for safe
// usage; nice-to-haves (preserve-times, mode masks, exclusions) wait until
// a consumer needs them. The zero value is the safe default.
//
// One safety rule is an invariant, not an option: extraction rejects
// entries and symlinks that escape the target root (the zip-slip /
// docker cp CVE-2018-15664 class).
type CopyOptions struct {
	// Overwrite replaces existing files instead of failing with an error
	// satisfying errors.Is(err, fs.ErrExist) — the os.CopyFS default.
	Overwrite bool

	// Chown applies UID and GID to created entries. When false, extraction
	// keeps the archive's recorded owners.
	Chown    bool
	UID, GID int
}

// ArchiveFS adapts the common host case — "copy this directory" — to the
// stream API: it returns a reader that lazily produces a tar stream of
// fsys as it is read, so nothing is buffered. Callers pass os.DirFS(path),
// an embed.FS, or a fstest.MapFS. Generation errors surface from Read.
//
//	err := g.CopyToGuest(ctx, "/workspace", vm.ArchiveFS(os.DirFS(src)), opts)
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
