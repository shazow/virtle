package cloudhypervisor

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"syscall"
	"time"

	"github.com/shazow/virtle/backend"
	"github.com/shazow/virtle/internal/executor"
	imanifest "github.com/shazow/virtle/internal/manifest"
	"github.com/shazow/virtle/internal/vmmhost"
)

const (
	// shareGracePeriod is how long a share daemon gets to leave on SIGTERM
	// before it is killed; virtiofsd exits as soon as the VMM disconnects.
	shareGracePeriod   = 500 * time.Millisecond
	socketPollInterval = 10 * time.Millisecond
)

// Machine is a Cloud Hypervisor microVM started by Backend: one VMM process,
// a virtiofsd per share, the private directory holding its API socket, the
// control socket, and the VM-name lock shared with the other backends. Done,
// Err, Wait, Kill, Status, Console, and RemoteControl are the microVM
// lifecycle every API-driven VMM shares; Shutdown is Cloud Hypervisor's.
type Machine struct{ *vmmhost.Machine }

func (b *Backend) start(ctx context.Context, mf *imanifest.Manifest, ephemeralState string) (backend.Machine, error) {
	cfg := mf.CloudHypervisor
	logger := b.logger()
	var daemons *shareDaemons
	m, err := vmmhost.Start(ctx, vmmhost.Launch{
		Name:             "cloud-hypervisor",
		RuntimeDirPrefix: "virtle-ch-",
		Manifest:         mf,
		EphemeralState:   ephemeralState,
		StartupTimeout:   cfg.StartupTimeout,
		ShutdownTimeout:  cfg.ShutdownTimeout,
		Console:          cfg.Console == imanifest.KernelSerialPrint,
		ConsoleOutput:    b.consoleOutput(),
		Logger:           logger,
		Networks:         vmmhost.NetworkStatuses(cfg.Networks),
		Prepare: func(context.Context) (func() error, error) {
			if err := vmmhost.CreateDisks(cfg.Disks, logger); err != nil {
				return nil, err
			}
			var err error
			if daemons, err = startShareDaemons(mf, logger); err != nil {
				return nil, err
			}
			return daemons.stop, nil
		},
		Command: func(socket string) *exec.Cmd {
			return exec.Command(cfg.Binary, "--api-socket", "path="+socket)
		},
		Configure: func(ctx context.Context, socket string) error {
			// vm.create fails outright when a share's socket is not there
			// yet, so wait for every daemon to bind first.
			if err := daemons.waitSockets(ctx); err != nil {
				return err
			}
			return newAPIClient(socket).configure(ctx, cfg)
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

// shareDaemons are the virtiofsd processes serving the shares, one per
// share virtle manages, started before the VMM and stopped after it exits.
type shareDaemons struct {
	sockets []string // every share's socket, virtle's daemons' and external ones alike
	group   executor.Group
	stderr  []*vmmhost.DiagnosticWriter // parallel to group.Processes()
	cleanup []string                    // socket paths virtle's daemons leave behind
}

// startShareDaemons starts the manifest's runs (its virtiofsd processes)
// in their own process groups, as QEMU does, keeping each one's stderr tail
// for the diagnosis of a daemon that dies before serving.
func startShareDaemons(mf *imanifest.Manifest, logger *slog.Logger) (*shareDaemons, error) {
	d := &shareDaemons{}
	for _, share := range mf.CloudHypervisor.Shares {
		if len(share.Socket) >= vmmhost.UnixPathMax {
			return nil, fmt.Errorf("cloud-hypervisor: share %q: socket path %q is too long; use a shorter state directory or virtiofs.socket", share.Tag, share.Socket)
		}
		d.sockets = append(d.sockets, share.Socket)
	}
	runs, err := mf.ResolvedRuns(0)
	if err != nil {
		return nil, err
	}
	if d.cleanup, err = mf.ResolvedCleanupFiles(); err != nil {
		return nil, err
	}
	for _, run := range runs {
		cmd := executor.Command(run.Exec[0], run.Exec[1:], run.Env)
		cmd.Dir = run.Dir
		cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
		stderr := &vmmhost.DiagnosticWriter{}
		cmd.Stdout, cmd.Stderr = io.Discard, stderr
		logger.Info("starting share daemon", "exec", run.Exec)
		process, err := (&executor.Runner{Logger: logger}).Start(cmd)
		if err != nil {
			_ = d.stop()
			return nil, err
		}
		process.SetGracePeriod(shareGracePeriod)
		d.group.Add(process)
		d.stderr = append(d.stderr, stderr)
	}
	return d, nil
}

// waitSockets returns once every share socket exists as a socket. It never
// connects: virtiofsd serves exactly one vhost-user connection, and a probe
// would take it from the VMM. A daemon that exits meanwhile ends the wait
// with its stderr.
func (d *shareDaemons) waitSockets(ctx context.Context) error {
	if len(d.sockets) == 0 {
		return nil
	}
	ticker := time.NewTicker(socketPollInterval)
	defer ticker.Stop()
	for {
		missing := ""
		for _, path := range d.sockets {
			if info, err := os.Stat(path); err != nil || info.Mode().Type() != os.ModeSocket {
				missing = path
				break
			}
		}
		if missing == "" {
			return nil
		}
		if err := d.exited(); err != nil {
			return err
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("wait for share socket %q: %w", missing, ctx.Err())
		case <-ticker.C:
		}
	}
}

// exited reports the first share daemon that has died, with its stderr.
func (d *shareDaemons) exited() error {
	process, exitErr, ok := d.group.FirstExit()
	if !ok {
		return nil
	}
	err := fmt.Errorf("share daemon %s exited before serving its socket", process.Name())
	if exitErr != nil {
		err = fmt.Errorf("share daemon %s exited before serving its socket: %w", process.Name(), exitErr)
	}
	for i, candidate := range d.group.Processes() {
		if candidate == process {
			if text := d.stderr[i].String(); text != "" {
				err = fmt.Errorf("%w; stderr: %q", err, text)
			}
		}
	}
	return err
}

// stop ends the daemons and removes the sockets they leave behind (a killed
// daemon cannot unlink its own). It runs after the VMM has exited, so a
// daemon that noticed the disconnect is already gone.
func (d *shareDaemons) stop() error {
	ctx, cancel := context.WithTimeout(context.Background(), 2*shareGracePeriod+time.Second)
	defer cancel()
	err := d.group.StopAll(ctx)
	for _, path := range d.cleanup {
		if removeErr := os.Remove(path); removeErr != nil && !errors.Is(removeErr, os.ErrNotExist) {
			err = errors.Join(err, removeErr)
		}
	}
	return err
}

var (
	_ backend.Machine         = (*Machine)(nil)
	_ backend.StatusReporter  = (*Machine)(nil)
	_ backend.ConsoleProvider = (*Machine)(nil)
)
