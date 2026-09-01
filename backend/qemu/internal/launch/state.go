package launch

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/shazow/virtle/internal/manifest"
)

const defaultStateWaitTimeout = 500 * time.Millisecond

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
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, err
	}
	return state.Status == "saved", nil
}

func RemoveSuspendState(manifest *manifest.Manifest) error {
	path := SuspendStatePath(manifest)
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
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

func ReadLaunchPID(manifest *manifest.Manifest) (int, error) {
	path := LaunchPIDPath(manifest)
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, fmt.Errorf("launch pid file %q does not exist; is virtle launch running for host %q?", path, manifest.Identity.HostName)
		}
		return 0, fmt.Errorf("read launch pid %q: %w", path, err)
	}

	raw := strings.TrimSpace(string(data))
	pid, err := strconv.Atoi(raw)
	if err != nil || pid <= 0 {
		if err == nil {
			err = fmt.Errorf("pid must be positive")
		}
		return 0, fmt.Errorf("invalid launch pid file %q: %w", path, err)
	}
	return pid, nil
}

func RemoveLaunchPID(manifest *manifest.Manifest, pid int) error {
	path := LaunchPIDPath(manifest)
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
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
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("remove launch pid %q: %w", path, err)
	}
	return nil
}

func validateLaunchLock(manifest *manifest.Manifest, pid int) error {
	path := manifest.ResolvedLockPath()
	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read launch lock %q: %w", path, err)
	}
	lockPID, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil || lockPID <= 0 {
		if err == nil {
			err = fmt.Errorf("pid must be positive")
		}
		return fmt.Errorf("invalid launch lock %q: %w", path, err)
	}
	if lockPID != pid {
		return fmt.Errorf("launch pid %d does not match lock owner %d in %q", pid, lockPID, path)
	}

	file, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		return fmt.Errorf("open launch lock %q: %w", path, err)
	}
	defer file.Close()

	if err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err == nil {
		_ = syscall.Flock(int(file.Fd()), syscall.LOCK_UN)
		return fmt.Errorf("stale launch pid %d from %q: sandbox lock %q is not held", pid, LaunchPIDPath(manifest), path)
	} else if !errors.Is(err, syscall.EWOULDBLOCK) && !errors.Is(err, syscall.EAGAIN) {
		return fmt.Errorf("check launch lock %q: %w", path, err)
	}

	return nil
}

func ResolveLaunchPID(manifest *manifest.Manifest, signaler PIDSignaler) (int, error) {
	if err := manifest.Validate(); err != nil {
		return 0, err
	}

	pid, err := ReadLaunchPID(manifest)
	if err != nil {
		return 0, wrapStage("launch pid", err)
	}

	if signaler != nil {
		if err := signaler.Exists(pid); err != nil {
			if IsNoProcess(err) {
				return 0, wrapStage("launch pid", fmt.Errorf("stale launch pid %d from %q: process does not exist", pid, LaunchPIDPath(manifest)))
			}
			return 0, wrapStage("launch pid", fmt.Errorf("check launch pid %d from %q: %w", pid, LaunchPIDPath(manifest), err))
		}
	}
	if err := validateLaunchLock(manifest, pid); err != nil {
		return 0, wrapStage("launch pid", err)
	}
	return pid, nil
}

func IsNoProcess(err error) bool {
	return errors.Is(err, os.ErrProcessDone) || errors.Is(err, syscall.ESRCH)
}

func WaitForLaunchExited(ctx context.Context, manifest *manifest.Manifest, timeout time.Duration) error {
	if err := waitForStateCondition(ctx, timeout, func() (bool, error) {
		_, err := os.Stat(LaunchPIDPath(manifest))
		if os.IsNotExist(err) {
			return true, nil
		}
		return false, err
	}, fmt.Sprintf("launch pid %q was not removed", LaunchPIDPath(manifest))); err != nil {
		return wrapStage("launch signal", err)
	}
	return nil
}

func WaitForSavedSuspendState(ctx context.Context, manifest *manifest.Manifest, timeout time.Duration) error {
	if err := waitForStateCondition(ctx, timeout, func() (bool, error) {
		state, err := ReadSuspendState(manifest)
		if os.IsNotExist(err) {
			return false, nil
		}
		if err != nil {
			return false, err
		}
		return state.Status == "saved", nil
	}, fmt.Sprintf("saved suspend state %q was not written", SuspendStatePath(manifest))); err != nil {
		return wrapStage("qmp suspend", err)
	}
	return nil
}

func waitForStateCondition(ctx context.Context, timeout time.Duration, ready func() (bool, error), timeoutMessage string) error {
	if timeout <= 0 {
		timeout = defaultStateWaitTimeout
	}

	deadline := time.NewTimer(timeout)
	defer deadline.Stop()
	ticker := time.NewTicker(20 * time.Millisecond)
	defer ticker.Stop()

	var lastErr error
	for {
		ok, err := ready()
		if ok {
			return nil
		}
		if err != nil {
			lastErr = err
		}

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-deadline.C:
			if lastErr != nil {
				return fmt.Errorf("%s before timeout: %w", timeoutMessage, lastErr)
			}
			return fmt.Errorf("%s before timeout", timeoutMessage)
		case <-ticker.C:
		}
	}
}
