package vmm

import (
	"context"
	"log/slog"
	"path/filepath"
	"testing"

	"github.com/shazow/virtle/backend/qemu/internal/launch"
	"github.com/shazow/virtle/internal/control"
	"github.com/shazow/virtle/internal/manifest"
)

func TestStartVMWithoutVSockWithExhaustedCIDRange(t *testing.T) {
	// Startup owns real runtime sockets and persistence paths; QEMU and QMP
	// are fakes, so this exercises launch without accessing KVM or host vsock.
	dir := t.TempDir()
	cfg := validManifest(dir)
	cfg.Paths.LockPath = filepath.Join(dir, "virtle.lock")
	cfg.QEMU.Devices.VSOCK.ID = ""
	cfg.VSock.CIDRange = manifest.VSockCIDRange{Start: 3, End: 5}
	cfg.QEMU.Devices.VirtioFS = nil
	cfg.QEMU.Devices.Block = nil
	cfg.Volumes = nil
	cfg.Run = nil

	checker := &exhaustedCIDChecker{}
	runner := &launchRunner{}
	manager := newManagerFromConfig(Config{
		Locker:          &fileLocker{},
		Runner:          runner,
		SocketWaiter:    &fakeSocketWaiter{},
		QMPDialer:       &fakeQMPDialer{client: &fakeQMPClient{onQuit: func() { runner.exitQEMU(nil) }}},
		VSockCIDChecker: checker,
		Logger:          slog.New(slog.DiscardHandler),
	})
	v, err := manager.startVM(t.Context(), launch.Spec{Manifest: cfg})
	if err != nil {
		t.Fatalf("start VM with vsock disabled and occupied CIDs: %v", err)
	}
	defer func() {
		if err := v.Shutdown(context.Background()); err != nil {
			t.Errorf("shutdown VM: %v", err)
		}
	}()
	status, err := v.Status(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if status.State != control.RuntimeReady || status.CID != 0 {
		t.Fatalf("launched status: %+v, want ready with CID 0", status)
	}
	if checker.calls != 0 {
		t.Fatalf("disabled launch checked %d host CIDs", checker.calls)
	}
}

type exhaustedCIDChecker struct{ calls int }

func (c *exhaustedCIDChecker) Available(int) (bool, error) {
	c.calls++
	return false, nil
}
