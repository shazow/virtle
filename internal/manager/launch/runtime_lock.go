package launch

import (
	"context"
	"errors"
	"fmt"
	"os"

	"github.com/shazow/virtle/internal/manifest"
)

type RuntimeLockSpec struct {
	Manifest    *manifest.Manifest
	ResumeState *SuspendState
	Locker      Locker
	Lifecycle   *Lifecycle
	Cancel      context.CancelFunc
	PID         int
}

type RuntimeLock struct {
	manifest  *manifest.Manifest
	lock      Lock
	lifecycle *Lifecycle
	cancel    context.CancelFunc
	pid       int
}

func AcquireRuntimeLock(spec RuntimeLockSpec) (*RuntimeLock, error) {
	lock, err := spec.Locker.Acquire(spec.Manifest.ResolvedLockPath())
	if err != nil {
		stopRuntimeLockLifecycle(spec.Lifecycle, spec.Cancel)
		return nil, err
	}
	runtimeLock := &RuntimeLock{
		manifest:  spec.Manifest,
		lock:      lock,
		lifecycle: spec.Lifecycle,
		cancel:    spec.Cancel,
		pid:       spec.PID,
	}

	if spec.ResumeState == nil {
		if err := RemoveSuspendState(spec.Manifest); err != nil {
			_ = runtimeLock.Cleanup()
			return nil, err
		}
	} else if _, err := os.Stat(spec.ResumeState.VMStatePath); err != nil {
		_ = runtimeLock.Cleanup()
		return nil, fmt.Errorf("saved vm state %q is not available: %w", spec.ResumeState.VMStatePath, err)
	}
	if err := WriteLaunchPID(spec.Manifest, spec.PID); err != nil {
		_ = runtimeLock.Cleanup()
		return nil, err
	}
	return runtimeLock, nil
}

func (l *RuntimeLock) Cleanup() error {
	if l == nil {
		return nil
	}
	var cleanupErr error
	cleanupErr = errors.Join(cleanupErr, RemoveLaunchPID(l.manifest, l.pid))
	if l.lock != nil {
		cleanupErr = errors.Join(cleanupErr, l.lock.Release())
	}
	stopRuntimeLockLifecycle(l.lifecycle, l.cancel)
	return cleanupErr
}

func stopRuntimeLockLifecycle(lifecycle *Lifecycle, cancel context.CancelFunc) {
	if lifecycle != nil {
		lifecycle.Stop()
	}
	if cancel != nil {
		cancel()
	}
}
