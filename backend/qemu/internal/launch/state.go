package launch

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/shazow/virtle/internal/manifest"
)

func SuspendStatePath(manifest *manifest.Manifest) string {
	return filepath.Join(manifest.ResolvedPersistenceStateDir(), manifest.Identity.HostName+".suspend.json")
}

func VMStatePath(manifest *manifest.Manifest) string {
	return filepath.Join(manifest.ResolvedPersistenceStateDir(), manifest.Identity.HostName+".vmstate")
}

// PrepareVMStateFile replaces the saved-state stream with a private empty file
// that privilege-dropped QEMU can reopen for migration.
func PrepareVMStateFile(manifest *manifest.Manifest) (string, error) {
	path := VMStatePath(manifest)
	if err := EnsurePersistenceDirectory(filepath.Dir(path), manifest.QEMU.RunAsUser); err != nil {
		return "", fmt.Errorf("create vm state directory: %w", err)
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return "", fmt.Errorf("remove stale vm state %q: %w", path, err)
	}
	file, err := createPrivateFile(path, manifest.QEMU.RunAsUser)
	if err != nil {
		return "", fmt.Errorf("prepare vm state %q: %w", path, err)
	}
	if err := file.Close(); err != nil {
		return "", fmt.Errorf("close vm state %q: %w", path, err)
	}
	return path, nil
}

func LaunchPIDPath(manifest *manifest.Manifest) string {
	return filepath.Join(manifest.ResolvedPersistenceStateDir(), manifest.Identity.HostName+".pid")
}

func WriteSuspendStateData(manifest *manifest.Manifest, state SuspendState) error {
	if state.HostName == "" {
		state.HostName = manifest.Identity.HostName
	}
	if state.Timestamp.IsZero() {
		state.Timestamp = time.Now().UTC()
	}
	path := SuspendStatePath(manifest)
	if err := EnsurePersistenceDirectory(filepath.Dir(path), manifest.QEMU.RunAsUser); err != nil {
		return fmt.Errorf("create suspend state directory: %w", err)
	}

	data, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return fmt.Errorf("encode suspend state: %w", err)
	}
	data = append(data, '\n')

	if err := os.WriteFile(path, data, privateFileMode); err != nil {
		return fmt.Errorf("write suspend state %q: %w", path, err)
	}
	return nil
}

func ReadSuspendState(manifest *manifest.Manifest) (SuspendState, error) {
	path := SuspendStatePath(manifest)
	data, err := os.ReadFile(path)
	if err != nil {
		return SuspendState{}, err
	}

	var state SuspendState
	if err := json.Unmarshal(data, &state); err != nil {
		return SuspendState{}, fmt.Errorf("decode suspend state %q: %w", path, err)
	}
	return state, nil
}

func HasSavedSuspendState(manifest *manifest.Manifest) (bool, error) {
	state, err := ReadSuspendState(manifest)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return false, nil
		}
		return false, err
	}
	return state.Status == SuspendStatusSaved, nil
}

func RemoveSuspendState(manifest *manifest.Manifest) error {
	path := SuspendStatePath(manifest)
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("remove suspend state %q: %w", path, err)
	}
	return nil
}

func RemoveRestoredSuspendState(plan *Plan) error {
	if err := os.Remove(plan.ResumeState.VMStatePath); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("remove saved vm state %q: %w", plan.ResumeState.VMStatePath, err)
	}
	return RemoveSuspendState(plan.Manifest)
}

func WriteLaunchPID(manifest *manifest.Manifest, pid int) error {
	path := LaunchPIDPath(manifest)
	if err := EnsurePersistenceDirectory(filepath.Dir(path), manifest.QEMU.RunAsUser); err != nil {
		return fmt.Errorf("create launch pid directory: %w", err)
	}
	if err := os.WriteFile(path, []byte(strconv.Itoa(pid)+"\n"), privateFileMode); err != nil {
		return fmt.Errorf("write launch pid %q: %w", path, err)
	}
	return nil
}

func RemoveLaunchPID(manifest *manifest.Manifest, pid int) error {
	path := LaunchPIDPath(manifest)
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("read launch pid %q: %w", path, err)
	}
	current, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil {
		return fmt.Errorf("invalid launch pid file %q: %w", path, err)
	}
	if current != pid {
		return nil
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("remove launch pid %q: %w", path, err)
	}
	return nil
}
