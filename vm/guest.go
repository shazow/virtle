package vm

import (
	"archive/tar"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
)

// Guest performs operations inside a running VM. Implemented by the
// host-side client in ./guest (virtle-native daemon) and by backend/qemu's
// QGA adapter. Shapes are os/exec- and io/fs-flavored, never protocol-
// flavored. Implementations must be safe for concurrent use.
//
// Daemon-only features (file-tree copy, streaming exec, file watching, ...)
// are GuestWithX extension interfaces discovered by type assertion; they
// are never added to Guest itself.
type Guest interface {
	// Run executes a command to completion, delivering its output to
	// cmd.Stdout and cmd.Stderr (the exec.Cmd.Run contract). A non-zero
	// exit status is returned as an error satisfying
	// errors.As(err, new(*ExitError)), so `if err != nil` is sufficient.
	// Whether output streams as it is produced or arrives at completion
	// is up to the implementation.
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
	Path  string
	Args  []string
	Env   []string
	Dir   string
	Stdin io.Reader

	// Stdout and Stderr receive the command's output; nil discards it.
	// Implementations that cannot stream deliver the buffered output at
	// completion.
	Stdout, Stderr io.Writer
}

// ExitError reports a guest command that ran to completion with a
// non-zero exit status, the exec.ExitError analog.
type ExitError struct {
	Code int
	// Stderr holds a tail of the command's standard error when
	// GuestCmd.Stderr was nil and the implementation captured it; it is
	// nil when the caller collected stderr itself.
	Stderr []byte
}

func (e *ExitError) Error() string {
	if len(e.Stderr) > 0 {
		return fmt.Sprintf("exit status %d: stderr=%q", e.Code, e.Stderr)
	}
	return fmt.Sprintf("exit status %d", e.Code)
}

// Output runs cmd and returns its standard output, the exec.Cmd.Output
// analog: cmd.Stdout must be nil, and when cmd.Stderr is nil an
// *ExitError carries a tail of the command's stderr.
func Output(ctx context.Context, g Guest, cmd *GuestCmd) ([]byte, error) {
	if cmd == nil {
		return nil, errors.New("vm: Output: nil command")
	}
	if cmd.Stdout != nil {
		return nil, errors.New("vm: Output: Stdout already set")
	}
	var stdout bytes.Buffer
	run := *cmd
	run.Stdout = &stdout
	err := g.Run(ctx, &run)
	return stdout.Bytes(), err
}

// GuestWithCopy streams file trees between host and guest as tar archives.
// Both directions speak the same shape (the Docker CopyToContainer /
// CopyFromContainer model), so a guest->guest copy is a direct pipe with no
// host filesystem and no buffering, and transforms compose as ordinary
// io.Reader middleware. Implemented by the ./guest daemon client.
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

	// Chown sets the ownership of created entries to UID and GID, the
	// os.Chown shape; false keeps whatever the archive recorded (ArchiveFS
	// records none, since host uids are meaningless in-guest).
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
