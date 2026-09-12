package cloudhypervisor

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os/exec"

	"github.com/shazow/virtle/backend"
	imanifest "github.com/shazow/virtle/internal/manifest"
	"github.com/shazow/virtle/internal/vmmhost"
)

// Machine is a Cloud Hypervisor microVM started by Backend: one VMM process,
// the manifest's helpers (a virtiofsd per share among them), the private
// directory holding its API socket, the control socket, and the VM-name lock
// shared with the other backends. Done, Err, Wait, Kill, Status, Console,
// and RemoteControl are the microVM lifecycle every API-driven VMM shares;
// Shutdown is Cloud Hypervisor's.
type Machine struct{ *vmmhost.Machine }

func (b *Backend) start(ctx context.Context, mf *imanifest.Manifest, ephemeralState string) (backend.Machine, error) {
	cfg := mf.CloudHypervisor
	logger := b.logger()
	sockets := make([]string, 0, len(cfg.Shares)) // every share's socket, virtle's daemons' and external ones alike
	for _, share := range cfg.Shares {
		sockets = append(sockets, share.Socket)
	}
	var helpers *vmmhost.Helpers
	m, err := vmmhost.Start(ctx, vmmhost.Launch{
		Name:               "cloud-hypervisor",
		RuntimeDirPrefix:   "virtle-ch-",
		Manifest:           mf,
		EphemeralState:     ephemeralState,
		StartupTimeout:     cfg.StartupTimeout,
		ShutdownTimeout:    cfg.ShutdownTimeout,
		Console:            cfg.Console == imanifest.KernelSerialPrint,
		ConsoleTerminal:    true, // Cloud Hypervisor reads serial input only from a terminal
		ConsoleInteractive: cfg.Console == imanifest.KernelSerialConsole,
		ConsoleOutput:      b.consoleOutput(),
		Logger:             logger,
		Networks:           vmmhost.NetworkStatuses(cfg.Networks),
		Prepare: func(context.Context) (func() error, error) {
			if err := vmmhost.CreateDisks(cfg.Disks, logger); err != nil {
				return nil, err
			}
			for _, share := range cfg.Shares {
				if len(share.Socket) >= vmmhost.UnixPathMax {
					return nil, fmt.Errorf("cloud-hypervisor: share %q: socket path %q is too long; use a shorter state directory or virtiofs.socket", share.Tag, share.Socket)
				}
			}
			var err error
			if helpers, err = vmmhost.StartHelpers(mf, logger); err != nil {
				return nil, err
			}
			return helpers.Stop, nil
		},
		Command: func(socket string) *exec.Cmd {
			return exec.Command(cfg.Binary, append([]string{"--api-socket", "path=" + socket}, cfg.Args...)...)
		},
		Configure: func(ctx context.Context, socket string) error {
			// vm.create fails outright when a share's socket is not there
			// yet, so wait for every daemon to bind first. A daemon that
			// dies afterwards shows as a connection failure on the VMM's
			// side; its own last words explain it.
			if err := helpers.WaitSockets(ctx, sockets); err != nil {
				return err
			}
			if err := newAPIClient(socket).configure(ctx, cfg); err != nil {
				return errors.Join(err, helpers.Exited())
			}
			return nil
		},
		Graceful: func(ctx context.Context, socket string) error {
			return newAPIClient(socket).call(ctx, http.MethodPut, "vm.power-button", nil, nil)
		},
	})
	if err != nil {
		return nil, err
	}
	return &Machine{m}, nil
}

// Shutdown presses the guest's ACPI power button and waits for the VMM to
// exit, which Cloud Hypervisor does once the guest powers off; it kills the
// VMM when Backend.ShutdownTimeout or ctx expires first. The guest needs ACPI
// button support and something acting on the event (acpid, systemd-logind)
// with a power-off, not a reboot: Cloud Hypervisor restarts a guest that
// resets. Repeated and concurrent calls share one attempt.
func (m *Machine) Shutdown(ctx context.Context) error { return m.Machine.Shutdown(ctx) }

var (
	_ backend.Machine         = (*Machine)(nil)
	_ backend.StatusReporter  = (*Machine)(nil)
	_ backend.ConsoleProvider = (*Machine)(nil)
)
