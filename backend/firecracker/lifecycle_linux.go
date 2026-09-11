package firecracker

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os/exec"
	"runtime"

	"github.com/shazow/virtle/backend"
	"github.com/shazow/virtle/internal/diskimage"
	imanifest "github.com/shazow/virtle/internal/manifest"
	"github.com/shazow/virtle/internal/vmmhost"
)

// ctrlAltDelSupported reports whether Firecracker's SendCtrlAltDel action,
// its only guest shutdown request, exists on this host; it emulates an i8042
// controller on x86 alone. Tests running a fake VMM override it.
var ctrlAltDelSupported = runtime.GOARCH == "amd64"

// Machine is a Firecracker microVM started by Backend: one VMM process, the
// private directory holding its API socket, the control socket, and the
// VM-name lock shared with QEMU. Done, Err, Wait, Kill, Status, Console, and
// RemoteControl are the microVM lifecycle every API-driven VMM shares;
// Shutdown is Firecracker's.
type Machine struct{ *vmmhost.Machine }

func (b *Backend) start(ctx context.Context, mf *imanifest.Manifest, ephemeralState string) (backend.Machine, error) {
	cfg := mf.Firecracker
	logger := b.logger()
	m, err := vmmhost.Start(ctx, vmmhost.Launch{
		Name:             "firecracker",
		RuntimeDirPrefix: "virtle-fc-",
		Manifest:         mf,
		EphemeralState:   ephemeralState,
		StartupTimeout:   cfg.StartupTimeout,
		ShutdownTimeout:  cfg.ShutdownTimeout,
		Console:          cfg.Console == imanifest.KernelSerialPrint,
		ConsoleOutput:    b.consoleOutput(),
		Logger:           logger,
		Networks:         networkStatuses(cfg),
		Prepare: func(context.Context) (func() error, error) {
			return nil, createDisks(cfg.Disks, logger)
		},
		Command: func(socket string) *exec.Cmd {
			return exec.Command(cfg.Binary, "--api-sock", socket)
		},
		Configure: func(ctx context.Context, socket string) error {
			return newAPIClient(socket).configure(ctx, cfg)
		},
		Graceful: func(ctx context.Context, socket string) error {
			if !ctrlAltDelSupported {
				return fmt.Errorf("firecracker graceful shutdown on %s: %w", runtime.GOARCH, errors.ErrUnsupported)
			}
			return newAPIClient(socket).put(ctx, "/actions", action{Type: "SendCtrlAltDel"})
		},
	})
	if err != nil {
		return nil, err
	}
	return &Machine{m}, nil
}

// createDisks formats the images vm.Disk.Size asked for before launch, as
// QEMU does; existing images are kept.
func createDisks(disks []imanifest.FirecrackerDisk, logger *slog.Logger) error {
	for _, disk := range disks {
		if !disk.Create {
			continue
		}
		created, err := diskimage.Ensure(diskimage.Image{Path: disk.Path, Size: disk.SizeMiB.Bytes().Int64(), Label: disk.Label})
		if err != nil {
			return err
		}
		if created {
			logger.Info("created disk image", "path", disk.Path, "size_mib", disk.SizeMiB)
		}
	}
	return nil
}

// Shutdown asks the guest to stop and waits for the VMM to exit, killing it
// when Backend.ShutdownTimeout or ctx expires. Firecracker's only
// guest shutdown request is the i8042 Ctrl-Alt-Del, which exists on amd64
// alone: the guest needs the i8042 driver and an init that handles
// Ctrl-Alt-Del with a reboot (virtle's reboot=k turns that into a VMM exit).
// On other architectures Shutdown kills the VMM and returns an error
// wrapping errors.ErrUnsupported. Repeated and concurrent calls share one
// attempt.
func (m *Machine) Shutdown(ctx context.Context) error { return m.Machine.Shutdown(ctx) }

var (
	_ backend.Machine         = (*Machine)(nil)
	_ backend.StatusReporter  = (*Machine)(nil)
	_ backend.ConsoleProvider = (*Machine)(nil)
)
