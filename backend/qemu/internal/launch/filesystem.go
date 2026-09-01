package launch

import (
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strconv"
	"syscall"

	backendfile "github.com/diskfs/go-diskfs/backend/file"
	"github.com/diskfs/go-diskfs/filesystem/ext4"
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
		_, resolvedGID, err := lookupUserIDs(runAsUser)
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
		uid, gid, err := lookupUserIDs(runAsUser)
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

func lookupUserIDs(name string) (int, int, error) {
	account, err := user.Lookup(name)
	if err != nil {
		return 0, 0, fmt.Errorf("look up qemu user %q: %w", name, err)
	}
	uid, err := strconv.Atoi(account.Uid)
	if err != nil {
		return 0, 0, fmt.Errorf("parse uid %q for qemu user %q: %w", account.Uid, name, err)
	}
	gid, err := strconv.Atoi(account.Gid)
	if err != nil {
		return 0, 0, fmt.Errorf("parse gid %q for qemu user %q: %w", account.Gid, name, err)
	}
	return uid, gid, nil
}

// CreateVolumeImage creates a volume image and optionally assigns the new file
// to the host account configured for privilege-dropped QEMU.
func CreateVolumeImage(volume manifest.Volume, runAsUser string) error {
	sizeBytes := volume.Size.Bytes().Int64()
	file, err := createPrivateFile(volume.ImagePath, runAsUser)
	if err != nil {
		return fmt.Errorf("create volume image %q: %w", volume.ImagePath, err)
	}

	created := false
	defer func() {
		if !created {
			_ = os.Remove(volume.ImagePath)
		}
	}()

	if err := file.Close(); err != nil {
		return fmt.Errorf("close volume image %q: %w", volume.ImagePath, err)
	}

	if chattrPath, lookErr := exec.LookPath("chattr"); lookErr == nil {
		cmd := exec.Command(chattrPath, "+C", volume.ImagePath)
		_ = cmd.Run()
	}

	if err := os.Truncate(volume.ImagePath, sizeBytes); err != nil {
		return fmt.Errorf("truncate volume image %q: %w", volume.ImagePath, err)
	}

	image, err := backendfile.OpenFromPath(volume.ImagePath, false)
	if err != nil {
		return fmt.Errorf("open volume image %q: %w", volume.ImagePath, err)
	}
	defer image.Close()

	params := &ext4.Params{}
	if volume.Label != "" {
		params.VolumeName = volume.Label
	}
	params.SectorsPerBlock = 8
	fs, err := ext4.Create(image, sizeBytes, 0, int64(ext4.SectorSize512), params)
	if err != nil {
		return fmt.Errorf("format ext4 volume image %q: %w", volume.ImagePath, err)
	}
	if volume.Label == "" {
		if err := fs.SetLabel(""); err != nil {
			return fmt.Errorf("clear default ext4 volume label for %q: %w", volume.ImagePath, err)
		}
	}

	created = true
	return nil
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
