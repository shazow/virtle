package launch

import (
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"syscall"

	backendfile "github.com/diskfs/go-diskfs/backend/file"
	"github.com/diskfs/go-diskfs/filesystem/ext4"
	"github.com/shazow/virtle/internal/manifest"
)

func CreateVolumeImage(volume manifest.Volume) error {
	sizeBytes := volume.Size.Bytes().Int64()
	file, err := os.OpenFile(volume.ImagePath, os.O_RDWR|os.O_CREATE|os.O_EXCL, 0o644)
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
