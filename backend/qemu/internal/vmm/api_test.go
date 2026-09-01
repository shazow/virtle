package vmm

import (
	"bytes"
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/shazow/virtle/backend/qemu/internal/launch"
)

// startWatchedVM starts a VM through the library path's fixture and
// installs the exit watcher StartVM installs.
func startWatchedVM(t *testing.T) (*VM, *launchRunner) {
	t.Helper()
	tmpDir := t.TempDir()
	cfg := validManifest(tmpDir)
	cfg.Paths.LockPath = filepath.Join(tmpDir, "virtle.lock")
	cfg.QEMU.QMP.SocketPath = "qmp.sock"
	cfg.Volumes[0].AutoCreate = false

	runner := &launchRunner{}
	var logOutput bytes.Buffer
	manager := &manager{
		locker:            &fileLocker{},
		runner:            runner,
		socketWaiter:      &fakeSocketWaiter{callback: func(paths []string) error { return nil }},
		qmpDialer:         &fakeQMPDialer{client: &fakeQMPClient{}},
		logger:            debugTestLogger(&logOutput),
		shutdownDelay:     10 * time.Millisecond,
		qmpRetryDelay:     0,
		qmpConnectTimeout: time.Millisecond,
		qmpQuitTimeout:    time.Millisecond,
	}
	v, err := manager.startVM(context.Background(), launch.Spec{
		Manifest: cfg,
		Options:  LaunchOptions{Resume: ResumeModeNo},
	})
	if err != nil {
		t.Fatalf("startVM: %v", err)
	}
	v.watchExit()
	return v, runner
}

func TestVMDoneClosesOnExit(t *testing.T) {
	v, runner := startWatchedVM(t)

	select {
	case <-v.Done():
		t.Fatal("Done closed before the VM exited")
	default:
	}
	if err := v.Err(); err != nil {
		t.Fatalf("Err before exit = %v, want nil", err)
	}

	runner.exitQEMU(nil)
	<-v.Done()
	if err := v.Err(); err != nil {
		t.Fatalf("Err after clean exit = %v, want nil", err)
	}
	if err := v.Wait(context.Background()); err != nil {
		t.Fatalf("Wait after exit = %v, want nil", err)
	}
}

func TestVMErrReportsExitStatus(t *testing.T) {
	v, runner := startWatchedVM(t)

	exitErr := errors.New("exit status 1")
	runner.exitQEMU(exitErr)
	<-v.Done()
	if err := v.Err(); !errors.Is(err, exitErr) {
		t.Fatalf("Err = %v, want %v", err, exitErr)
	}
}

func TestVMWaitHonorsContext(t *testing.T) {
	v, runner := startWatchedVM(t)
	t.Cleanup(func() { runner.exitQEMU(nil); <-v.Done() })

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := v.Wait(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("Wait with canceled ctx = %v, want context.Canceled", err)
	}
}
