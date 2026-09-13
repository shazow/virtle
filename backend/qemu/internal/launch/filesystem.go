package launch

import (
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"syscall"

	"github.com/shazow/virtle/internal/diskimage"
	"github.com/shazow/virtle/internal/manifest"
)

const (
	privateDirectoryMode os.FileMode = 0o700
	// searchableDirectoryMode lets the primary group of an explicitly
	// configured qemu.user traverse managed directories without listing or
	// modifying them.
	searchableDirectoryMode os.FileMode = 0o710
	privateFileMode         os.FileMode = 0o600
)

// EnsurePersistenceDirectory creates a managed persistence directory. For
// privilege-dropped QEMU, only directories created by this call are assigned
// to the QEMU account's primary group.
func EnsurePersistenceDirectory(path string, runAsUser string) error {
	mode := privateDirectoryMode
	gid := -1
	if runAsUser != "" {
		_, resolvedGID, err := diskimage.LookupOwner(runAsUser)
		if err != nil {
			return err
		}
		mode = searchableDirectoryMode
		gid = resolvedGID
	}
	return ensureDirectory(path, mode, gid)
}

// EnsurePrivateDirectory creates path and any missing parents privately.
// Existing directories are deliberately left unchanged for compatibility.
func EnsurePrivateDirectory(path string) error {
	return ensureDirectory(path, privateDirectoryMode, -1)
}

func ensureDirectory(path string, mode os.FileMode, gid int) error {
	path = filepath.Clean(path)
	info, err := os.Stat(path)
	if err == nil {
		if !info.IsDir() {
			return fmt.Errorf("path %q is not a directory", path)
		}
		return nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return err
	}

	parent := filepath.Dir(path)
	if parent != path {
		if err := ensureDirectory(parent, mode, gid); err != nil {
			return err
		}
	}
	if err := os.Mkdir(path, mode); err != nil {
		if errors.Is(err, os.ErrExist) {
			info, statErr := os.Stat(path)
			if statErr == nil && info.IsDir() {
				return nil
			}
		}
		return err
	}
	// Mkdir is filtered through the process umask. Set the requested mode only
	// on directories this call created; existing state is never migrated
	// implicitly.
	if err := os.Chmod(path, mode); err != nil {
		return err
	}
	if gid >= 0 {
		if err := os.Chown(path, -1, gid); err != nil {
			return fmt.Errorf("assign directory %q to qemu group %d: %w", path, gid, err)
		}
	}
	return nil
}

func createPrivateFile(path string, runAsUser string) (*os.File, error) {
	file, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE|os.O_EXCL, privateFileMode)
	if err != nil {
		return nil, err
	}
	keep := false
	defer func() {
		if keep {
			return
		}
		_ = file.Close()
		_ = os.Remove(path)
	}()
	if err := file.Chmod(privateFileMode); err != nil {
		return nil, err
	}
	if runAsUser != "" {
		uid, gid, err := diskimage.LookupOwner(runAsUser)
		if err != nil {
			return nil, err
		}
		if err := file.Chown(uid, gid); err != nil {
			return nil, fmt.Errorf("assign %q to qemu user %q: %w", path, runAsUser, err)
		}
	}
	keep = true
	return file, nil
}

// EnsureVolumeImage creates the volume image unless it already exists and
// reports whether it did.
func EnsureVolumeImage(volume manifest.Volume, runAsUser string) (bool, error) {
	return diskimage.Ensure(volumeImage(volume, runAsUser))
}

func volumeImage(volume manifest.Volume, runAsUser string) diskimage.Image {
	return diskimage.Image{
		Path:  volume.ImagePath,
		Size:  volume.Size.Bytes().Int64(),
		Label: volume.Label,
		Owner: runAsUser,
	}
}

var ErrStaleSocket = errors.New("stale socket")

func RemoveStaleSockets(paths ...string) error {
	for _, path := range paths {
		err := CheckSocketPath(path)
		if err == nil {
			continue
		}
		if !errors.Is(err, ErrStaleSocket) {
			return err
		}
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("remove socket %q: %w", path, err)
		}
	}
	return nil
}

func CheckSocketPath(path string) error {
	info, err := os.Lstat(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("stat socket %q: %w", path, err)
	}
	if info.Mode()&os.ModeSocket == 0 {
		return fmt.Errorf("socket %q: path is not a socket", path)
	}
	conn, err := net.Dial("unix", path)
	if err == nil {
		_ = conn.Close()
		return fmt.Errorf("socket %q is still live", path)
	}
	if !errors.Is(err, syscall.ECONNREFUSED) {
		return fmt.Errorf("check socket %q liveness: %w", path, err)
	}
	return ErrStaleSocket
}
