package launch

import (
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
)

// PrepareRuntimeState makes the host side of plan ready for QEMU to start: it
// creates the directories the manifest expects, clears sockets left behind by
// a crashed run, checks that externally managed virtiofs sockets are live, and
// creates any volume images marked auto-create.
//
// It is shared by the CLI manager and the qemu backend, which reach the same
// plan by different routes. logger may be nil.
func PrepareRuntimeState(plan *Plan, logger *slog.Logger) error {
	for _, dir := range plan.Manifest.ResolvedPersistenceDirectories() {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return fmt.Errorf("create directory %q: %w", dir, err)
		}
	}
	for _, path := range plan.RuntimeSocketCleanupFiles() {
		dir := filepath.Dir(path)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return fmt.Errorf("create directory %q: %w", dir, err)
		}
	}
	for _, path := range plan.ExternalVirtioFSSocketPaths {
		info, err := os.Stat(path)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				return fmt.Errorf("external virtiofs socket %q does not exist", path)
			}
			return fmt.Errorf("stat external virtiofs socket %q: %w", path, err)
		}
		if info.Mode()&os.ModeSocket == 0 {
			return fmt.Errorf("external virtiofs socket %q is not a socket", path)
		}
	}
	for _, path := range plan.VolumeImagePaths {
		dir := filepath.Dir(path)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return fmt.Errorf("create directory %q: %w", dir, err)
		}
	}
	if err := RemoveStaleSockets(plan.RuntimeSocketCleanupFiles()...); err != nil {
		return err
	}
	for _, volume := range plan.Volumes {
		if !volume.AutoCreate {
			continue
		}
		info, err := os.Stat(volume.ImagePath)
		if err == nil {
			if info.IsDir() {
				return fmt.Errorf("volume image %q is a directory", volume.ImagePath)
			}
			continue
		}
		if !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("stat volume image %q: %w", volume.ImagePath, err)
		}
		if logger != nil {
			logger.Info("creating volume image", "path", volume.ImagePath, "size_mib", volume.Size, "fs_type", volume.FSType)
		}
		if err := CreateVolumeImage(volume); err != nil {
			return err
		}
	}
	return nil
}
