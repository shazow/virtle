package vmm

import (
	"context"
	"errors"
	"log/slog"
	"path/filepath"
	"testing"
	"time"

	"github.com/shazow/virtle/backend/qemu/internal/launch"
	runtimepkg "github.com/shazow/virtle/backend/qemu/internal/runtime"
	control "github.com/shazow/virtle/internal/control"
	"github.com/shazow/virtle/internal/executor/executortest"
	imanifest "github.com/shazow/virtle/internal/manifest"
)

func TestRuntimeStatusAndBalloonUseOwnedQMP(t *testing.T) {
	tmpDir := t.TempDir()
	cfg := validManifest(tmpDir)
	cfg.QEMU.Devices.Balloon = &imanifest.BalloonDevice{ID: "balloon0", Transport: "pci"}
	stats := launch.NewStats(time.Now())
	stats.Timer(launch.TimerBootStarted, time.Now())
	stats.Timer(launch.TimerQMPReady, time.Now())
	qmp := (&fakeQMPClient{queryBalloonActualBytes: 640 * testMiB}).withDefaultBalloonPath("/machine/peripheral/balloon0")
	runtime := runtimepkg.New(runtimepkg.RuntimeConfig{
		Manifest: cfg,
		Paths: launch.RuntimePaths{
			ControlSocket: filepath.Join(tmpDir, "virtle.sock"),
			QMPSocket:     filepath.Join(tmpDir, "qmp.sock"),
		},
		CID:              9,
		Stats:            stats,
		QMP:              qmp,
		WriteBackTimeout: time.Second,
		Logger:           slog.New(slog.DiscardHandler),
	})
	runtime.SetReady()

	status, err := runtime.Status(context.Background(), control.StatusRequest{})
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	if status.State != control.RuntimeReady || status.CID != 9 || status.Paths.ControlSocket == "" || status.Stats.BootToQMP == "" {
		t.Fatalf("unexpected status: %#v", status)
	}

	resp, err := runtime.Balloon(context.Background(), control.BalloonRequest{TargetBytes: 768 * testMiB})
	if err != nil {
		t.Fatalf("balloon: %v", err)
	}
	if resp.ActualBytes != 640*testMiB || resp.TargetBytes != 768*testMiB {
		t.Fatalf("unexpected balloon response: %#v", resp)
	}
	if got := qmp.setBalloonLogicalSizes; len(got) != 1 || got[0] != 768*testMiB {
		t.Fatalf("expected balloon resize through qmp, got %#v", got)
	}
}

func TestRuntimeBalloonMapsMissingDeviceToFailedPrecondition(t *testing.T) {
	tmpDir := t.TempDir()
	cfg := validManifest(tmpDir)
	runtime := runtimepkg.New(runtimepkg.RuntimeConfig{
		Manifest: cfg,
		Paths: launch.RuntimePaths{
			ControlSocket: filepath.Join(tmpDir, "virtle.sock"),
			QMPSocket:     filepath.Join(tmpDir, "qmp.sock"),
		},
		CID:              9,
		Stats:            launch.NewStats(time.Now()),
		QMP:              &fakeQMPClient{},
		WriteBackTimeout: time.Second,
		Logger:           slog.New(slog.DiscardHandler),
	})

	_, err := runtime.Balloon(context.Background(), control.BalloonRequest{})
	var rpcErr *control.RPCError
	if !errors.As(err, &rpcErr) {
		t.Fatalf("error type: got %T", err)
	}
	if rpcErr.Code != control.ErrFailedPrecondition {
		t.Fatalf("code: got %s want %s", rpcErr.Code, control.ErrFailedPrecondition)
	}
}

func TestRuntimeSuspendQueuesAndWaitsForLaunchLoop(t *testing.T) {
	tmpDir := t.TempDir()
	cfg := validManifest(tmpDir)
	coordinator := launch.NewSuspendCoordinator()
	qmp := &fakeQMPClient{status: "running"}
	runtime := runtimepkg.New(runtimepkg.RuntimeConfig{
		Manifest: cfg,
		Paths: launch.RuntimePaths{
			ControlSocket: filepath.Join(tmpDir, "virtle.sock"),
			QMPSocket:     filepath.Join(tmpDir, "qmp.sock"),
		},
		CID:              9,
		Stats:            launch.NewStats(time.Now()),
		QMP:              qmp,
		SuspendRequests:  coordinator,
		WriteBackTimeout: time.Second,
		Logger:           slog.New(slog.DiscardHandler),
		SavedSuspendExit: launch.IsSavedSuspendExit,
	})
	runtime.SetReady()

	done := make(chan error, 1)
	go func() {
		resp, err := runtime.Suspend(context.Background(), control.SuspendRequest{})
		if err == nil && !resp.Saved {
			err = errors.New("suspend response was not saved")
		}
		done <- err
	}()

	select {
	case <-coordinator.Notify():
	case err := <-done:
		t.Fatalf("suspend returned before launch loop handled request: %v", err)
	case <-time.After(time.Second):
		t.Fatal("suspend request was not queued")
	}
	select {
	case err := <-done:
		t.Fatalf("suspend returned before launch loop completion: %v", err)
	case <-time.After(testNoReturnTimeout):
	}
	if qmp.migrateCalls != 0 {
		t.Fatalf("runtime suspend migrated directly over qmp, got %d calls", qmp.migrateCalls)
	}

	coordinator.Begin()
	coordinator.Complete(launch.ErrSavedSuspendExit)
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("suspend: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("suspend did not return after launch loop completion")
	}
}

func TestRuntimeSuspendMapsMissingCoordinatorToFailedPrecondition(t *testing.T) {
	tmpDir := t.TempDir()
	cfg := validManifest(tmpDir)
	runtime := runtimepkg.New(runtimepkg.RuntimeConfig{
		Manifest: cfg,
		Paths: launch.RuntimePaths{
			ControlSocket: filepath.Join(tmpDir, "virtle.sock"),
			QMPSocket:     filepath.Join(tmpDir, "qmp.sock"),
		},
		CID:              9,
		Stats:            launch.NewStats(time.Now()),
		QMP:              &fakeQMPClient{},
		WriteBackTimeout: time.Second,
		Logger:           slog.New(slog.DiscardHandler),
	})
	runtime.SetReady()

	_, err := runtime.Suspend(context.Background(), control.SuspendRequest{})
	var rpcErr *control.RPCError
	if !errors.As(err, &rpcErr) {
		t.Fatalf("error type: got %T", err)
	}
	if rpcErr.Code != control.ErrFailedPrecondition {
		t.Fatalf("code: got %s want %s", rpcErr.Code, control.ErrFailedPrecondition)
	}
}

func TestRuntimeStartControlServesStatus(t *testing.T) {
	tmpDir := t.TempDir()
	cfg := validManifest(tmpDir)
	controlPath := filepath.Join(tmpDir, "virtle.sock")
	runtime := runtimepkg.New(runtimepkg.RuntimeConfig{
		Manifest: cfg,
		Paths: launch.RuntimePaths{
			ControlSocket: controlPath,
			QMPSocket:     filepath.Join(tmpDir, "qmp.sock"),
		},
		CID:              11,
		Stats:            launch.NewStats(time.Now()),
		QMP:              &fakeQMPClient{},
		WriteBackTimeout: time.Second,
		Logger:           slog.New(slog.DiscardHandler),
	})
	runtime.SetReady()
	if _, err := runtime.StartControl(context.Background(), control.Handlers{}); err != nil {
		t.Fatalf("start control: %v", err)
	}
	t.Cleanup(func() {
		if err := runtime.Close(); err != nil {
			t.Errorf("runtime close: %v", err)
		}
	})

	status, err := control.Dial(controlPath).Status(context.Background(), control.StatusRequest{})
	if err != nil {
		t.Fatalf("status over control socket: %v", err)
	}
	if status.State != control.RuntimeReady || status.CID != 11 {
		t.Fatalf("unexpected status: %#v", status)
	}
}

func TestRuntimeMarkSavedSuspendSkipsCloseWriteBack(t *testing.T) {
	tmpDir := t.TempDir()
	cfg := validManifest(tmpDir)
	processes := launch.NewProcessSet()
	qmp := &fakeQMPClient{}
	var writeBackCalls int
	runtime := runtimepkg.New(runtimepkg.RuntimeConfig{
		Manifest: cfg,
		Paths: launch.RuntimePaths{
			ControlSocket: filepath.Join(tmpDir, "virtle.sock"),
			QMPSocket:     filepath.Join(tmpDir, "qmp.sock"),
		},
		CID:       11,
		Stats:     launch.NewStats(time.Now()),
		QMP:       qmp,
		Processes: processes,
		WriteBack: func(context.Context) error {
			writeBackCalls++
			return nil
		},
		WriteBackTimeout: time.Second,
		Logger:           slog.New(slog.DiscardHandler),
		SavedSuspendExit: launch.IsSavedSuspendExit,
	})

	runtime.MarkSavedSuspend()
	if err := runtime.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	if writeBackCalls != 0 {
		t.Fatalf("write-back calls: got %d want 0", writeBackCalls)
	}
}

func TestRuntimeCloseStopsProcessesAndDisconnectsQMPOnce(t *testing.T) {
	tmpDir := t.TempDir()
	cfg := validManifest(tmpDir)
	qmp := &fakeQMPClient{}
	process := (&executortest.Process{OverrideName: "qemu-system-x86_64"}).Process()
	processes := launch.NewProcessSet()
	processes.SetQEMU(process)
	runtime := runtimepkg.New(runtimepkg.RuntimeConfig{
		Manifest: cfg,
		Paths: launch.RuntimePaths{
			ControlSocket: filepath.Join(tmpDir, "virtle.sock"),
			QMPSocket:     filepath.Join(tmpDir, "qmp.sock"),
		},
		CID:              11,
		Stats:            launch.NewStats(time.Now()),
		QMP:              qmp,
		Processes:        processes,
		WriteBackTimeout: time.Second,
		Logger:           slog.New(slog.DiscardHandler),
	})

	if err := runtime.Close(); err != nil {
		t.Fatalf("first close: %v", err)
	}
	if err := runtime.Close(); err != nil {
		t.Fatalf("second close: %v", err)
	}
	if got, want := qmp.disconnectCalls, 1; got != want {
		t.Fatalf("unexpected qmp disconnect calls: got %d want %d", got, want)
	}
	if exited, err := process.PollExit(); err != nil || !exited {
		t.Fatalf("expected process to exit after close, exited=%v err=%v", exited, err)
	}
}
