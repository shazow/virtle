// Package qemu starts virtual machines with QEMU.
//
// It is one implementation of [github.com/shazow/virtle/backend].Backend, and
// the only one virtle ships today. Consumers name it explicitly:
//
//	b, err := qemu.BackendWithQGA(qemu.Config{})
//	inst, err := b.Start(ctx, spec)
//	defer backend.Shutdown(ctx, inst)
//
// The constructor is where the choice of guest-control protocol is made —
// [BackendWithQGA] speaks the QEMU guest agent — and where QEMU-only settings
// arrive, through [Config]. Everything after Start is protocol-neutral.
//
// The backends returned here also implement backend.Suspender (QMP migration
// to and from a state file) and backend.MemoryResizer (virtio-balloon, when
// Config.Balloon is set). virtiofs daemons, QMP and guest-agent sockets, and
// the QEMU command line are implementation details that no exported symbol
// names.
//
// This package is experimental. The module is untagged v0 and the API may
// change until it settles.
package qemu

import (
	"context"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/shazow/virtle/backend"
	"github.com/shazow/virtle/internal/executor"
	"github.com/shazow/virtle/internal/manager/launch"
	"github.com/shazow/virtle/internal/manifest"
	"github.com/shazow/virtle/internal/qemuargs"
	"github.com/shazow/virtle/internal/qga"
	"github.com/shazow/virtle/internal/qmpclient"
	"github.com/shazow/virtle/vm"
)

const (
	// qmpConnectTimeout bounds waiting for QEMU's monitor to answer, which
	// is how the backend learns the VM is up.
	qmpConnectTimeout = 30 * time.Second
	qmpRetryDelay     = 50 * time.Millisecond

	// defaultGuestAgentTimeout bounds waiting for the guest agent to answer.
	// It is generous because it covers guest boot, not just a round trip.
	defaultGuestAgentTimeout = 2 * time.Minute

	// stopGracePeriod is how long a helper process gets to exit on SIGTERM
	// before it is killed.
	stopGracePeriod = 15 * time.Second
)

// qemuBackend starts QEMU machines. Its unexported dependencies are the seams
// tests replace; consumers configure it through [Config] alone.
type qemuBackend struct {
	config Config

	// guestControl builds the vm.Guest an instance hands out. Nil means the
	// backend was constructed without guest control.
	guestControl func(*instance) (vm.Guest, error)

	runner       launch.Runner
	socketWaiter launch.SocketWaiter
	cidChecker   launch.VSockCIDChecker
	qmpDialer    qmpclient.Dialer
	qgaDialer    qga.Dialer

	// setBalloonSize is the one monitor command issued through the raw QMP
	// monitor rather than a typed helper, so it is a seam of its own.
	setBalloonSize func(ctx context.Context, client qmpclient.Client, sizeBytes int64) error
}

// BackendWithQGA returns a QEMU backend whose instances' RemoteControl speaks
// the QEMU guest agent. Guests must run qemu-guest-agent on the virtio-serial
// channel virtle wires up, which is what virtle has always required.
func BackendWithQGA(config Config) (backend.Backend, error) {
	b := newBackend(config)
	b.guestControl = (*instance).qgaGuestControl
	return b, nil
}

func newBackend(config Config) *qemuBackend {
	return &qemuBackend{
		config:       config,
		runner:       &executor.Runner{},
		socketWaiter: &launch.PollingSocketWaiter{},
		cidChecker:   launch.NewHostVSockCIDChecker(),
		qmpDialer:    &qmpclient.SocketMonitorDialer{},
		qgaDialer:    &qga.SocketDialer{},
		setBalloonSize: func(ctx context.Context, client qmpclient.Client, sizeBytes int64) error {
			return qmpclient.SetBalloonSize(ctx, client, sizeBytes)
		},
	}
}

// Start boots a machine and returns once QEMU's monitor answers. When the spec
// carries Files, it also waits for guest control and writes them, so the
// workload finds them in place.
func (b *qemuBackend) Start(ctx context.Context, spec *vm.Spec) (backend.Instance, error) {
	document, err := b.config.document(spec)
	if err != nil {
		return nil, err
	}
	inst, err := b.start(ctx, document, nil)
	if err != nil {
		return nil, err
	}
	if len(spec.Files) > 0 {
		if err := inst.writeFiles(ctx, spec.Files); err != nil {
			return nil, errors.Join(fmt.Errorf("write spec files: %w", err), inst.Kill())
		}
	}
	return inst, nil
}

// start does the work shared by booting and resuming: resolve the manifest,
// claim a CID, start the virtiofs helpers, start QEMU, and wait for QMP.
//
// resume is nil for a fresh boot. When set, QEMU is started expecting an
// incoming migration stream and reuses the CID the suspended machine had.
func (b *qemuBackend) start(ctx context.Context, document manifest.Document, resume *suspendState) (_ *instance, err error) {
	logger := b.config.logger()
	resolved, err := document.ManifestWithOptions(manifest.ResolveOptions{Logger: logger})
	if err != nil {
		return nil, err
	}
	plan, err := launch.BuildPlan(launch.Spec{Manifest: resolved}, nil, nil)
	if err != nil {
		return nil, err
	}

	var savedState *launch.SuspendState
	if resume != nil {
		savedState = &launch.SuspendState{CID: resume.CID}
	}
	cid, err := launch.AcquireCID(resolved, savedState, b.cidChecker)
	if err != nil {
		return nil, err
	}
	plan.CID = cid

	if err := launch.PrepareRuntimeState(plan, logger); err != nil {
		return nil, err
	}

	command, err := qemuargs.BuildCommand(resolved, cid, resume != nil)
	if err != nil {
		return nil, err
	}

	inst := &instance{
		backend:   b,
		manifest:  resolved,
		plan:      plan,
		cid:       cid,
		processes: launch.NewProcessSet(),
		logger:    logger,
	}
	// From here on the instance owns host resources — helper daemons, then
	// QEMU itself — so any failure has to release them before returning.
	defer func() {
		if err != nil {
			err = errors.Join(err, inst.Kill())
		}
	}()

	if err := inst.startHelpers(ctx); err != nil {
		return nil, err
	}

	logger.Info("starting qemu", "cid", cid, "binary", command.Path)
	process, err := b.runner.Start(command)
	if err != nil {
		return nil, err
	}
	process.SetGracePeriod(stopGracePeriod)
	inst.processes.SetQEMU(process)

	qmp, err := launch.WaitForQMP(ctx, launch.QMPWait{
		Stage:          "vm startup",
		SocketPath:     plan.Paths.QMPSocket,
		SocketWaiter:   b.socketWaiter,
		Dialer:         b.qmpDialer,
		ConnectTimeout: qmpConnectTimeout,
		RetryDelay:     qmpRetryDelay,
		PollDelay:      launch.DefaultSocketPollInterval,
		Watchers:       inst.processes.Watchers(),
	})
	if err != nil {
		return nil, err
	}
	inst.qmp = qmp
	return inst, nil
}

// startHelpers starts the host-side daemons the machine needs before QEMU can
// attach to them — virtiofs daemons, one per share — and waits for their
// sockets. The resolved manifest expresses them as run commands; a
// library-built manifest has no others.
func (i *instance) startHelpers(ctx context.Context) error {
	runs, err := i.manifest.ResolvedRuns(i.cid)
	if err != nil {
		return err
	}
	for index, run := range runs {
		i.logger.Info("starting helper process", "index", index, "exec", run.Exec)
		command := executor.Command(run.Exec[0], run.Exec[1:], run.Env)
		command.Dir = run.Dir
		command.Stdout = os.Stderr
		command.Stderr = os.Stderr
		process, err := i.backend.runner.Start(command)
		if err != nil {
			return err
		}
		process.SetGracePeriod(stopGracePeriod)
		i.processes.Add(process)
	}
	if len(i.plan.VirtioFSSocketPaths) == 0 {
		return nil
	}
	return launch.WaitForSockets(ctx, launch.SocketWait{
		Stage:        "virtiofs startup",
		SocketPaths:  i.plan.VirtioFSSocketPaths,
		SocketWaiter: i.backend.socketWaiter,
		PollDelay:    launch.DefaultSocketPollInterval,
		Watchers:     i.processes.Watchers(),
	})
}
