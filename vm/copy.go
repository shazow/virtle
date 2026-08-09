package vm

import (
	"archive/tar"
	"context"
	"io"
	"io/fs"
)

// GuestWithCopy streams file trees between host and guest as tar archives. It
// is an extension interface: assert for it on a [Guest] rather than expecting
// every agent to provide it.
//
// Both directions speak the same shape — the Docker
// CopyToContainer/CopyFromContainer model — so a guest-to-guest copy is a
// direct pipe with no host filesystem and no buffering:
//
//	rc, err := src.CopyFromGuest(ctx, "/data")
//	defer rc.Close()
//	err = dst.CopyToGuest(ctx, "/data", rc, vm.CopyOptions{})
//
// and transforms compose as ordinary io.Reader middleware.
type GuestWithCopy interface {
	// CopyToGuest extracts a tar stream into guestPath.
	//
	// Entries that would escape guestPath — absolute paths, "..", symlinks
	// pointing outside the target — are rejected. That is an invariant of
	// the operation, not an option: extraction runs as root in the guest.
	CopyToGuest(ctx context.Context, guestPath string, archive io.Reader, opts CopyOptions) error

	// CopyFromGuest returns a tar stream of guestPath. The caller must close
	// it.
	CopyFromGuest(ctx context.Context, guestPath string) (io.ReadCloser, error)
}

// CopyOptions carries the options prior art shows are necessary for safe
// usage; nice-to-haves such as preserve-times, mode masks, and exclusions
// wait until a consumer needs them. The zero value is the safe default.
type CopyOptions struct {
	// Overwrite replaces existing files instead of failing with an error
	// satisfying errors.Is(err, fs.ErrExist) — the os.CopyFS default.
	Overwrite bool

	// UID and GID override ownership of created entries; nil keeps whatever
	// the archive recorded. The pointers are load-bearing: 0 (root) is a
	// valid value, so nil has to be distinguishable from it.
	UID, GID *int
}

// ArchiveFS adapts the common host case — "copy this directory" — to the
// stream API. It returns a reader that lazily produces a tar stream of fsys as
// it is read, so nothing is buffered. Callers pass os.DirFS(path), an
// embed.FS, or a fstest.MapFS:
//
//	err := g.CopyToGuest(ctx, "/workspace", vm.ArchiveFS(os.DirFS(src)), opts)
//
// Generation errors surface from Read rather than up front, since with lazy
// generation there is nothing to report yet when ArchiveFS returns. The
// archive records no ownership: host uids are meaningless in-guest, so set
// ownership through [CopyOptions] instead.
func ArchiveFS(fsys fs.FS) io.ReadCloser {
	reader, writer := io.Pipe()
	go func() {
		archive := tar.NewWriter(writer)
		err := archive.AddFS(fsys)
		if err == nil {
			err = archive.Close()
		}
		writer.CloseWithError(err)
	}()
	return reader
}
