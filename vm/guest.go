package vm

import (
	"archive/tar"
	"context"
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
	// Run executes a command to completion with buffered output (the
	// exec.Cmd.Output analog); streaming exec arrives as an extension.
	Run(ctx context.Context, cmd *GuestCmd) (*GuestResult, error)
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
}

// GuestResult is the buffered outcome of a completed GuestCmd.
type GuestResult struct {
	ExitCode       int
	Stdout, Stderr []byte
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

	// UID/GID override ownership of created entries; nil keeps whatever
	// the archive recorded (ArchiveFS records none, since host uids are
	// meaningless in-guest). Pointers are load-bearing: 0 (root) is a
	// valid value, so nil must be distinguishable from it.
	UID, GID *int
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
