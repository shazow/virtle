package launch

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/shazow/virtle/internal/manifest"
)

func TestAcquireRuntimeLockWritesPIDAndCleanupRemovesIt(t *testing.T) {
	cfg := runtimeLockManifest(t.TempDir())
	locker := &recordingLocker{}
	stopped := false
	ctx, cancel := context.WithCancel(context.Background())
	lifecycle := NewLifecycle(nil, func() { stopped = true }, cancel)

	runtimeLock, err := AcquireRuntimeLock(RuntimeLockSpec{
		Manifest:  cfg,
		Locker:    locker,
		Lifecycle: lifecycle,
		Cancel:    cancel,
		PID:       123,
	})
	if err != nil {
		t.Fatalf("acquire runtime lock: %v", err)
	}
	if got, err := ReadLaunchPID(cfg); err != nil || got != 123 {
		t.Fatalf("launch pid: got %d err=%v want 123", got, err)
	}

	if err := runtimeLock.Cleanup(); err != nil {
		t.Fatalf("cleanup runtime lock: %v", err)
	}
	if _, err := os.Stat(LaunchPIDPath(cfg)); !os.IsNotExist(err) {
		t.Fatalf("expected pid file removal, stat err=%v", err)
	}
	if !locker.lock.released {
		t.Fatal("expected lock release")
	}
	if !stopped {
		t.Fatal("expected lifecycle stop")
	}
	if ctx.Err() == nil {
		t.Fatal("expected context cancellation")
	}
}

func TestAcquireRuntimeLockRemovesStaleSuspendStateWithoutResume(t *testing.T) {
	cfg := runtimeLockManifest(t.TempDir())
	if err := WriteSuspendStateData(cfg, SuspendState{Status: "saved"}); err != nil {
		t.Fatalf("write suspend state: %v", err)
	}
	runtimeLock, err := AcquireRuntimeLock(RuntimeLockSpec{
		Manifest: cfg,
		Locker:   &recordingLocker{},
		Cancel:   func() {},
		PID:      123,
	})
	if err != nil {
		t.Fatalf("acquire runtime lock: %v", err)
	}
	defer runtimeLock.Cleanup()
	if _, err := os.Stat(SuspendStatePath(cfg)); !os.IsNotExist(err) {
		t.Fatalf("expected suspend state removal, stat err=%v", err)
	}
}

func TestAcquireRuntimeLockValidatesResumeStateAfterLock(t *testing.T) {
	cfg := runtimeLockManifest(t.TempDir())
	locker := &recordingLocker{}
	ctx, cancel := context.WithCancel(context.Background())

	_, err := AcquireRuntimeLock(RuntimeLockSpec{
		Manifest:    cfg,
		ResumeState: &SuspendState{VMStatePath: filepath.Join(t.TempDir(), "missing.vmstate")},
		Locker:      locker,
		Cancel:      cancel,
		PID:         123,
	})
	if err == nil || !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("expected missing vm state error, got %v", err)
	}
	if !locker.lock.released {
		t.Fatal("expected lock release after validation failure")
	}
	if ctx.Err() == nil {
		t.Fatal("expected context cancellation after validation failure")
	}
}

func runtimeLockManifest(tmpDir string) *manifest.Manifest {
	return &manifest.Manifest{
		Identity: manifest.Identity{HostName: "agent"},
		Paths: manifest.Paths{
			WorkingDir: tmpDir,
			LockPath:   filepath.Join(tmpDir, "virtle.lock"),
		},
		Persistence: manifest.Persistence{StateDir: filepath.Join(tmpDir, ".state")},
	}
}

type recordingLocker struct {
	lock recordingLock
	err  error
}

func (l *recordingLocker) Acquire(string) (Lock, error) {
	if l.err != nil {
		return nil, l.err
	}
	return &l.lock, nil
}

type recordingLock struct {
	released bool
}

func (l *recordingLock) Release() error {
	l.released = true
	return nil
}
