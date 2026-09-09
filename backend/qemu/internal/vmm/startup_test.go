package vmm

import (
	"context"
	"errors"
	"log/slog"
	"path/filepath"
	"testing"
	"time"

	"github.com/shazow/virtle/backend/qemu/internal/launch"
	"github.com/shazow/virtle/internal/control"
)

func TestStartWithPlanClosesControlAfterStartupError(t *testing.T) {
	// A real control socket distinguishes closing the listener from merely
	// unlinking its path. QEMU and QMP use the existing in-memory fakes.
	dir := t.TempDir()
	cfg := validManifest(dir)
	cfg.Paths.LockPath = filepath.Join(dir, "virtle.lock")
	cfg.QEMU.Devices.VSOCK.ID = ""
	cfg.QEMU.Devices.VirtioFS = nil
	cfg.QEMU.Devices.Block = nil
	cfg.Volumes = nil
	cfg.Run = nil
	// Without remote control, this fails after StartControl has succeeded.
	cfg.Workspace.MountCWD = true

	runner := &launchRunner{}
	qmp := &fakeQMPClient{}
	m := newManagerFromConfig(Config{
		Locker:       &fileLocker{},
		Runner:       runner,
		SocketWaiter: &fakeSocketWaiter{},
		QMPDialer:    &fakeQMPDialer{client: qmp},
		Logger:       slog.New(slog.DiscardHandler),
	})
	plan, err := m.planLaunch(launch.Spec{Manifest: cfg})
	if err != nil {
		t.Fatal(err)
	}
	checkedControl := false
	qmp.onQuit = func() {
		defer runner.exitQEMU(nil)
		// Runtime shutdown closes control before stopping QEMU. The fallback
		// cleanup only unlinks sockets later, leaving a live listener here.
		ctx, cancel := context.WithTimeout(t.Context(), time.Second)
		defer cancel()
		checkedControl = true
		if status, err := control.Raw(ctx, plan.Paths.ControlSocket, "status", nil); err == nil {
			t.Errorf("control still serves status during startup-error cleanup: %s", status)
		}
	}

	started, err := m.startWithPlan(t.Context(), plan)
	if started != nil {
		defer started.Close()
		t.Error("failed startup returned a nonnil launch")
	}
	var stage *launch.StageError
	if !errors.As(err, &stage) || stage.Stage != "guest agent" {
		t.Fatalf("startup error = %v, want guest agent stage after control startup", err)
	}
	if !checkedControl || qmp.quitCalls != 1 || qmp.disconnectCalls != 1 {
		t.Errorf("runtime cleanup: control checked=%v, quits=%d, disconnects=%d",
			checkedControl, qmp.quitCalls, qmp.disconnectCalls)
	}
}
