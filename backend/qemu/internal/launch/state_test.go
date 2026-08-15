package launch

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/shazow/virtle/internal/manifest"
)

func TestSuspendStateRoundTrip(t *testing.T) {
	cfg := testManifest(t)
	state := SuspendState{
		QMPSocketPath: "/run/qmp.sock",
		VMStatePath:   VMStatePath(cfg),
		CID:           7,
		Status:        "saved",
	}

	if err := WriteSuspendStateData(cfg, state); err != nil {
		t.Fatalf("write suspend state: %v", err)
	}
	readState, err := ReadSuspendState(cfg)
	if err != nil {
		t.Fatalf("read suspend state: %v", err)
	}
	if readState.HostName != cfg.Identity.HostName || readState.Status != "saved" || readState.CID != 7 || readState.Timestamp.IsZero() {
		t.Fatalf("unexpected suspend state: %#v", readState)
	}
	saved, err := HasSavedSuspendState(cfg)
	if err != nil {
		t.Fatalf("has saved suspend state: %v", err)
	}
	if !saved {
		t.Fatal("expected saved suspend state")
	}
	if err := RemoveSuspendState(cfg); err != nil {
		t.Fatalf("remove suspend state: %v", err)
	}
	if _, err := os.Stat(SuspendStatePath(cfg)); !os.IsNotExist(err) {
		t.Fatalf("expected suspend state removal, got %v", err)
	}
}

func TestRemoveRestoredSuspendState(t *testing.T) {
	cfg := testManifest(t)
	vmStatePath := VMStatePath(cfg)
	if err := os.MkdirAll(filepath.Dir(vmStatePath), 0o755); err != nil {
		t.Fatalf("create vm state dir: %v", err)
	}
	if err := os.WriteFile(vmStatePath, []byte("state"), 0o644); err != nil {
		t.Fatalf("write vm state: %v", err)
	}
	if err := WriteSuspendStateData(cfg, SuspendState{Status: "saved", VMStatePath: vmStatePath}); err != nil {
		t.Fatalf("write suspend state: %v", err)
	}

	err := RemoveRestoredSuspendState(&Plan{Manifest: cfg, ResumeState: &SuspendState{VMStatePath: vmStatePath}})
	if err != nil {
		t.Fatalf("remove restored state: %v", err)
	}
	if _, err := os.Stat(vmStatePath); !os.IsNotExist(err) {
		t.Fatalf("expected vm state removal, got %v", err)
	}
	if _, err := os.Stat(SuspendStatePath(cfg)); !os.IsNotExist(err) {
		t.Fatalf("expected suspend state removal, got %v", err)
	}
}

func TestLaunchPIDRoundTripAndLockValidation(t *testing.T) {
	cfg := testManifest(t)
	if err := WriteLaunchPID(cfg, 12345); err != nil {
		t.Fatalf("write launch pid: %v", err)
	}
	pid, err := ReadLaunchPID(cfg)
	if err != nil {
		t.Fatalf("read launch pid: %v", err)
	}
	if pid != 12345 {
		t.Fatalf("unexpected launch pid: got %d want 12345", pid)
	}

	lockPath := cfg.ResolvedLockPath()
	if err := os.MkdirAll(filepath.Dir(lockPath), 0o755); err != nil {
		t.Fatalf("create lock dir: %v", err)
	}
	if err := os.WriteFile(lockPath, []byte("12345\n"), 0o644); err != nil {
		t.Fatalf("write lock: %v", err)
	}
	if err := validateLaunchLock(cfg, 12345); err == nil || !strings.Contains(err.Error(), "not held") {
		t.Fatalf("expected stale lock error, got %v", err)
	}

	if err := RemoveLaunchPID(cfg, 12345); err != nil {
		t.Fatalf("remove launch pid: %v", err)
	}
	if _, err := os.Stat(LaunchPIDPath(cfg)); !os.IsNotExist(err) {
		t.Fatalf("expected launch pid removal, got %v", err)
	}
}

func TestResolveLaunchPIDValidatesProcessAndLock(t *testing.T) {
	cfg := testManifest(t)
	lockPath := cfg.ResolvedLockPath()
	if err := os.MkdirAll(filepath.Dir(lockPath), 0o755); err != nil {
		t.Fatalf("create lock dir: %v", err)
	}
	lockFile, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		t.Fatalf("open lock: %v", err)
	}
	defer lockFile.Close()
	if err := syscall.Flock(int(lockFile.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		t.Fatalf("lock file: %v", err)
	}
	defer syscall.Flock(int(lockFile.Fd()), syscall.LOCK_UN)
	if _, err := lockFile.WriteString("12345\n"); err != nil {
		t.Fatalf("write lock: %v", err)
	}
	if err := WriteLaunchPID(cfg, 12345); err != nil {
		t.Fatalf("write launch pid: %v", err)
	}

	pid, err := ResolveLaunchPID(cfg, fakePIDSignaler{})
	if err != nil {
		t.Fatalf("resolve launch pid: %v", err)
	}
	if pid != 12345 {
		t.Fatalf("pid: got %d want 12345", pid)
	}
}

func TestResolveLaunchPIDWrapsStaleProcess(t *testing.T) {
	cfg := testManifest(t)
	if err := WriteLaunchPID(cfg, 12345); err != nil {
		t.Fatalf("write launch pid: %v", err)
	}
	_, err := ResolveLaunchPID(cfg, fakePIDSignaler{existsErr: syscall.ESRCH})
	var stageErr *StageError
	if !errors.As(err, &stageErr) || stageErr.Stage != "launch pid" || !strings.Contains(err.Error(), "process does not exist") {
		t.Fatalf("stale pid err: got %v", err)
	}
}

func TestResolveLaunchPIDWrapsProcessCheckError(t *testing.T) {
	cfg := testManifest(t)
	if err := WriteLaunchPID(cfg, 12345); err != nil {
		t.Fatalf("write launch pid: %v", err)
	}
	checkErr := errors.New("permission check failed")
	_, err := ResolveLaunchPID(cfg, fakePIDSignaler{existsErr: checkErr})
	var stageErr *StageError
	if !errors.As(err, &stageErr) || stageErr.Stage != "launch pid" || !errors.Is(err, checkErr) {
		t.Fatalf("check err: got %v", err)
	}
}

func TestWaitForLaunchExited(t *testing.T) {
	cfg := testManifest(t)
	if err := WriteLaunchPID(cfg, 12345); err != nil {
		t.Fatalf("write launch pid: %v", err)
	}
	if err := RemoveLaunchPID(cfg, 12345); err != nil {
		t.Fatalf("remove launch pid: %v", err)
	}
	if err := WaitForLaunchExited(context.Background(), cfg, time.Millisecond); err != nil {
		t.Fatalf("wait for launch exited: %v", err)
	}
}

func TestWaitForLaunchExitedWrapsTimeout(t *testing.T) {
	cfg := testManifest(t)
	if err := WriteLaunchPID(cfg, 12345); err != nil {
		t.Fatalf("write launch pid: %v", err)
	}
	err := WaitForLaunchExited(context.Background(), cfg, time.Millisecond)
	var stageErr *StageError
	if !errors.As(err, &stageErr) || stageErr.Stage != "launch signal" || !strings.Contains(err.Error(), "was not removed") {
		t.Fatalf("wait err: got %v", err)
	}
}

func TestWaitForSavedSuspendState(t *testing.T) {
	cfg := testManifest(t)
	if err := WriteSuspendStateData(cfg, SuspendState{Status: "saved"}); err != nil {
		t.Fatalf("write suspend state: %v", err)
	}
	if err := WaitForSavedSuspendState(context.Background(), cfg, time.Millisecond); err != nil {
		t.Fatalf("wait for saved suspend state: %v", err)
	}
}

func TestWaitForSavedSuspendStateWrapsTimeout(t *testing.T) {
	cfg := testManifest(t)
	err := WaitForSavedSuspendState(context.Background(), cfg, time.Millisecond)
	var stageErr *StageError
	if !errors.As(err, &stageErr) || stageErr.Stage != "qmp suspend" || !strings.Contains(err.Error(), "was not written") {
		t.Fatalf("wait err: got %v", err)
	}
}

type fakePIDSignaler struct {
	existsErr error
}

func (s fakePIDSignaler) Exists(int) error {
	return s.existsErr
}

func (s fakePIDSignaler) Signal(int, os.Signal) error {
	return nil
}

func testManifest(t *testing.T) *manifest.Manifest {
	t.Helper()

	tmpDir := t.TempDir()
	return &manifest.Manifest{
		Identity: manifest.Identity{HostName: "agent"},
		Paths: manifest.Paths{
			WorkingDir: tmpDir,
			LockPath:   filepath.Join(tmpDir, ".agentspace", "agent.lock"),
		},
		Persistence: manifest.Persistence{
			StateDir: filepath.Join(tmpDir, ".agentspace"),
		},
		QEMU: manifest.QEMU{
			BinaryPath: "/bin/qemu",
			QMP:        manifest.QEMUQMP{SocketPath: "qmp.sock"},
			Devices: manifest.QEMUDevices{
				RNG:   manifest.QEMURNGDevice{ID: "rng0", Transport: "pci"},
				VSOCK: manifest.QEMUVSOCKDevice{ID: "vsock0", Transport: "pci"},
			},
		},
		SSH: manifest.SSH{User: "agent", RetryDelay: 500 * time.Millisecond},
	}
}
