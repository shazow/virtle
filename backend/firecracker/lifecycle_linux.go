package firecracker

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"sync"
	"syscall"
	"time"

	"github.com/shazow/virtle/backend"
	"github.com/shazow/virtle/internal/console"
	"github.com/shazow/virtle/internal/control"
	"github.com/shazow/virtle/internal/diskimage"
	"github.com/shazow/virtle/internal/executor"
	imanifest "github.com/shazow/virtle/internal/manifest"
	"github.com/shazow/virtle/vm"
)

const (
	socketRetryInterval = 10 * time.Millisecond
	killWaitTimeout     = 2 * time.Second
	// unixPathMax is sizeof(sun_path) on Linux; longer socket paths cannot bind.
	unixPathMax = 108
)

// ctrlAltDelSupported reports whether Firecracker's SendCtrlAltDel action,
// its only guest shutdown request, exists on this host; it emulates an i8042
// controller on x86 alone. Tests running a fake VMM override it.
var ctrlAltDelSupported = runtime.GOARCH == "amd64"

// Machine is a Firecracker microVM started by Backend: one VMM process, the
// private directory holding its API socket, the control socket, and the
// VM-name lock shared with QEMU.
type Machine struct {
	process         *executor.Process
	api             *apiClient
	control         *control.Server
	lock            io.Closer
	runtimeDir      string // private directory holding the API socket
	ephemeralState  string // temporary state directory removed on exit, or ""
	shutdownTimeout time.Duration
	diagnostics     *diagnosticWriter
	console         *console.Hub // serial console fan-out; nil when the console is off

	// stopped closes once the VMM has exited and its runtime files are
	// released; done closes after the control server has also delivered the
	// responses of lifecycle RPCs that observed the exit.
	stopped chan struct{}
	done    chan struct{}

	mu         sync.Mutex
	status     backend.Status
	err        error // exit result, valid after stopped closes
	cleanupErr error

	shutdownOnce sync.Once
	shutdownDone chan struct{}
	shutdownErr  error
}

// newMachine wires a started VMM process to its runtime resources. The
// caller starts the control server and the reaper.
func newMachine(process *executor.Process, api *apiClient, lock io.Closer, runtimeDir string, shutdownTimeout time.Duration) *Machine {
	return &Machine{
		process:         process,
		api:             api,
		lock:            lock,
		runtimeDir:      runtimeDir,
		shutdownTimeout: shutdownTimeout,
		diagnostics:     &diagnosticWriter{},
		stopped:         make(chan struct{}),
		done:            make(chan struct{}),
		shutdownDone:    make(chan struct{}),
	}
}

func (b *Backend) start(ctx context.Context, mf *imanifest.Manifest, ephemeralState string) (backend.Machine, error) {
	started := false
	defer func() {
		if !started && ephemeralState != "" {
			_ = os.RemoveAll(ephemeralState)
		}
	}()
	lock, err := lockState(mf)
	if err != nil {
		return nil, err
	}
	controlPath, err := mf.ResolvedControlSocketPath()
	if err != nil {
		_ = lock.Close()
		return nil, err
	}
	// The lock held above proves no live virtle runs this VM name in this
	// state directory, so, as in the QEMU backend, whatever control.Listen
	// replaces is taken to be a leftover of a crashed launch.
	listener, err := control.Listen(controlPath)
	if err != nil {
		_ = lock.Close()
		return nil, fmt.Errorf("listen on control socket: %w", err)
	}
	// The API socket gets a short, unique, private (0700) directory of its own:
	// state directories can sit deeper than a Unix socket path allows, and
	// nothing pre-existing is ever reused or unlinked.
	dir, err := os.MkdirTemp("", "virtle-fc-")
	if err != nil {
		_ = listener.Close()
		_ = lock.Close()
		return nil, err
	}
	rollback := func() { _ = listener.Close(); _ = os.RemoveAll(dir); _ = lock.Close() }
	socket := filepath.Join(dir, "api.sock")
	if len(socket) >= unixPathMax {
		rollback()
		return nil, fmt.Errorf("firecracker API socket path %q is too long; set a shorter TMPDIR", socket)
	}
	cfg := mf.Firecracker
	logger := b.logger()
	for _, disk := range cfg.Disks {
		if !disk.Create {
			continue
		}
		created, err := diskimage.Ensure(diskimage.Image{Path: disk.Path, Size: disk.SizeMiB.Bytes().Int64(), Label: disk.Label})
		if err != nil {
			rollback()
			return nil, err
		}
		if created {
			logger.Info("created disk image", "path", disk.Path, "size_mib", disk.SizeMiB)
		}
	}
	cmd := exec.Command(cfg.Binary, "--api-sock", socket)
	cmd.Dir = mf.Paths.WorkingDir
	cmd.WaitDelay = time.Second // inherited output descriptors cannot hold teardown open
	// Firecracker leads its own process group, so teardown signals reach
	// anything it spawned, and the host's Ctrl-C does not reach it directly.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	m := newMachine(nil, newAPIClient(socket), lock, dir, cfg.ShutdownTimeout)
	m.ephemeralState = ephemeralState
	cmd.Stdout = io.Discard
	cmd.Stderr = m.diagnostics
	if cfg.Console == imanifest.KernelSerialPrint {
		// The guest's serial port rides the VMM's standard streams: the hub
		// prints it, retains it, and serves Machine.Console sessions.
		// Firecracker's own stderr diagnostics share the output writer.
		serialized := &lockedWriter{writer: b.consoleOutput()}
		hub, err := console.New(serialized, logger.With("host_name", mf.Identity.HostName))
		if err != nil {
			rollback()
			return nil, err
		}
		m.console = hub
		cmd.Stdin, cmd.Stdout, cmd.Stderr = hub.Stdin(), hub, io.MultiWriter(m.diagnostics, serialized)
	}
	logger.Info("starting firecracker", "binary", cfg.Binary, "api_socket", socket)
	m.process, err = (&executor.Runner{Logger: logger}).Start(cmd)
	if err != nil {
		if m.console != nil {
			_ = m.console.Close()
		}
		rollback()
		return nil, err
	}
	if m.console != nil {
		m.console.Started()
	}
	// Firecracker's graceful path is its API, never SIGTERM: Stop only ever
	// hard-kills here, and the grace period bounds its wait for the reaper.
	m.process.SetGracePeriod(killWaitTimeout)
	m.status = backend.Status{
		State: backend.StateStarting,
		PID:   m.process.PID(),
		Paths: backend.StatusPaths{ControlSocket: controlPath, MonitorSocket: socket},
		Stats: backend.RuntimeStats{StartedAt: time.Now()},
	}
	router, err := control.NewMachineRouter(controlMachine{m})
	if err != nil {
		_ = m.process.KillAndWait()
		rollback()
		return nil, err
	}
	m.control, err = control.NewServer(router)
	if err != nil {
		_ = m.process.KillAndWait()
		rollback()
		return nil, err
	}
	go func() {
		if err := m.control.Serve(listener); err != nil {
			// The control socket is a peripheral: losing it leaves the machine
			// running for the caller that holds it.
			logger.Warn("control server stopped", "err", err)
			_ = m.control.Close()
		}
	}()
	<-m.control.Started()
	go m.reap()
	started = true // the reaper now owns all cleanup, including failed startup
	startupCtx, cancel := context.WithTimeout(ctx, cfg.StartupTimeout)
	defer cancel()
	// Process exit interrupts both API startup and in-flight configuration.
	go func() {
		select {
		case <-m.done:
			cancel()
		case <-startupCtx.Done():
		}
	}()
	if err := m.waitAPI(startupCtx, socket); err != nil {
		return nil, m.startupFailure(err)
	}
	if err := m.api.configure(startupCtx, cfg); err != nil {
		return nil, m.startupFailure(err)
	}
	m.mu.Lock()
	if m.status.State == backend.StateStarting {
		m.status.State = backend.StateReady
		m.status.Stats.MonitorReadyAt = time.Now()
	}
	m.mu.Unlock()
	go func() {
		select {
		case <-ctx.Done():
			_ = m.Kill()
		case <-m.done:
		}
	}()
	return m, nil
}

// lockState creates the state directory privately when it is missing (an
// existing directory is used as it is, as the QEMU backend does), then takes
// the exclusive VM-name lock both backends share, so two launches of one
// manifest exclude each other whichever backend they use.
func lockState(mf *imanifest.Manifest) (*os.File, error) {
	path := mf.ResolvedLockPath()
	if err := ensurePrivateDirectory(filepath.Dir(path)); err != nil {
		return nil, fmt.Errorf("create state directory: %w", err)
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("acquire lock %q: %w", path, err)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("acquire lock %q: another virtle owns this state directory: %w", path, err)
	}
	// Record the owner, as the QEMU backend does.
	if err := f.Truncate(0); err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("write lock %q: %w", path, err)
	}
	if _, err := f.WriteString(strconv.Itoa(os.Getpid()) + "\n"); err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("write lock %q: %w", path, err)
	}
	return f, nil
}

// ensurePrivateDirectory creates path (and missing parents) with mode 0700.
// An existing directory is left unchanged, so state directories created by
// older virtle versions or by hand keep working.
func ensurePrivateDirectory(path string) error {
	if info, err := os.Stat(path); err == nil {
		if !info.IsDir() {
			return fmt.Errorf("%q is not a directory", path)
		}
		return nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err := os.MkdirAll(path, 0o700); err != nil {
		return err
	}
	// MkdirAll is filtered through the umask; make the directory this call
	// created private regardless.
	return os.Chmod(path, 0o700)
}

func (m *Machine) waitAPI(ctx context.Context, socket string) error {
	// Firecracker offers no inherited readiness descriptor. Only connection
	// establishment is retried; configuration mutations are never retried.
	timer := time.NewTicker(socketRetryInterval)
	defer timer.Stop()
	for {
		conn, err := m.api.dial(ctx)
		if err == nil {
			return conn.Close()
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("wait for firecracker API at %q: %w", socket, ctx.Err())
		case <-timer.C:
		}
	}
}

func (m *Machine) startupFailure(err error) error {
	exited, exitErr := m.process.PollExit()
	if exited {
		// The reaper cancels API setup when the child exits. That internal
		// cancellation must not be mistaken for cancellation by the caller.
		if errors.Is(err, context.Canceled) {
			err = nil
		}
		if exitErr != nil {
			err = errors.Join(err, fmt.Errorf("firecracker exited during startup: %w", exitErr))
		} else {
			err = errors.Join(err, errors.New("firecracker exited during startup"))
		}
	}
	err = errors.Join(err, m.Kill())
	if text := m.diagnostics.String(); text != "" {
		err = fmt.Errorf("%w; firecracker stderr: %q", err, text)
	}
	return err
}

func (m *Machine) reap() {
	err := m.process.Wait()
	cleanupErr := errors.Join(m.control.Close(), os.RemoveAll(m.runtimeDir), m.lock.Close())
	if m.console != nil {
		cleanupErr = errors.Join(cleanupErr, m.console.Close())
	}
	if m.ephemeralState != "" {
		cleanupErr = errors.Join(cleanupErr, os.RemoveAll(m.ephemeralState))
	}
	m.mu.Lock()
	m.err = errors.Join(err, cleanupErr)
	m.cleanupErr = cleanupErr
	m.status.State = backend.StateStopped
	m.status.Stats.CompletedAt = time.Now()
	m.mu.Unlock()
	close(m.stopped)
	m.control.Wait()
	close(m.done)
}

// Done closes after the machine exits and its runtime state is released.
func (m *Machine) Done() <-chan struct{} { return m.done }

// Err reports the exit result after Done closes.
func (m *Machine) Err() error { m.mu.Lock(); defer m.mu.Unlock(); return m.err }

// Wait blocks until the machine exits or ctx ends and returns the exit result.
func (m *Machine) Wait(ctx context.Context) error {
	select {
	case <-m.done:
		return m.Err()
	default:
	}
	select {
	case <-m.done:
		return m.Err()
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Kill hard-stops Firecracker and releases runtime state.
func (m *Machine) Kill() error { return m.finish(m.kill()) }

func (m *Machine) kill() error {
	// Stop with an expired context skips the graceful rungs: it SIGKILLs the
	// process group and bounds its own wait for the reaper.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := m.process.Stop(ctx)
	waitCtx, stop := context.WithTimeout(context.Background(), killWaitTimeout)
	defer stop()
	select {
	case <-m.stopped:
		m.mu.Lock()
		defer m.mu.Unlock()
		return errors.Join(err, m.cleanupErr)
	case <-waitCtx.Done():
		return errors.Join(err, fmt.Errorf("firecracker teardown: %w", waitCtx.Err()))
	}
}

// Shutdown asks the guest to power off and waits for the VMM to exit,
// killing it when Backend.ShutdownTimeout or ctx expires. Firecracker's only
// guest shutdown request is the i8042 Ctrl-Alt-Del, which exists on amd64
// alone: the guest needs the i8042 driver and an init that handles
// Ctrl-Alt-Del with a reboot (virtle's reboot=k turns that into a VMM exit).
// On other architectures Shutdown kills the VMM and returns an error
// wrapping errors.ErrUnsupported. Repeated and concurrent calls share one
// attempt.
func (m *Machine) Shutdown(ctx context.Context) error { return m.finish(m.shutdown(ctx)) }

func (m *Machine) shutdown(ctx context.Context) error {
	select {
	case <-m.stopped:
		return m.Err()
	default:
	}
	if err := ctx.Err(); err != nil {
		return errors.Join(err, m.kill())
	}
	m.shutdownOnce.Do(func() {
		go func() {
			defer close(m.shutdownDone)
			m.shutdownErr = m.gracefulShutdown()
		}()
	})
	select {
	case <-m.shutdownDone:
		return m.shutdownErr
	case <-ctx.Done():
		return errors.Join(ctx.Err(), m.kill())
	}
}

func (m *Machine) gracefulShutdown() error {
	if !ctrlAltDelSupported {
		return errors.Join(fmt.Errorf("firecracker graceful shutdown on %s: %w", runtime.GOARCH, errors.ErrUnsupported), m.kill())
	}
	ctx, cancel := context.WithTimeout(context.Background(), m.shutdownTimeout)
	defer cancel()
	m.mu.Lock()
	if m.status.State != backend.StateStopped {
		m.status.State = backend.StateStopping
	}
	m.mu.Unlock()
	if err := m.api.put(ctx, "/actions", action{Type: "SendCtrlAltDel"}); err != nil {
		// A guest that powered off on its own while the request was in
		// flight reached the state shutdown wanted; only a live VMM is killed.
		if exited, _ := m.process.PollExit(); !exited {
			return errors.Join(err, m.kill())
		}
	}
	if err := m.waitStopped(ctx); err != nil {
		return errors.Join(err, m.kill())
	}
	return nil
}

// controlMachine serves the control socket. Its lifecycle handlers wait for
// process cleanup but must not wait for their own response to be delivered;
// the public Machine methods wait for both.
type controlMachine struct{ *Machine }

func (m controlMachine) Wait(ctx context.Context) error     { return m.waitStopped(ctx) }
func (m controlMachine) Kill() error                        { return m.kill() }
func (m controlMachine) Shutdown(ctx context.Context) error { return m.shutdown(ctx) }

func (m *Machine) waitStopped(ctx context.Context) error {
	select {
	case <-m.stopped:
		return m.Err()
	case <-ctx.Done():
		return ctx.Err()
	}
}

// finish returns err after the control server has drained, once the VMM is
// known to have stopped; a process wedged during kill keeps the caller bounded
// by kill's own timeout instead.
func (m *Machine) finish(err error) error {
	select {
	case <-m.stopped:
		<-m.done
	default:
	}
	return err
}

// RemoteControl reports errors.ErrUnsupported: Firecracker machines have no
// guest control transport yet.
func (m *Machine) RemoteControl() (vm.Guest, error) {
	return nil, fmt.Errorf("firecracker has no guest control transport: %w", errors.ErrUnsupported)
}

// Console implements backend.ConsoleProvider: a vm.Term over the guest's
// serial port, available when Backend.Console is ConsolePrint. The session
// replays the recent console output first, so one attached after boot still
// sees what the guest printed; its Resize and Wait report
// errors.ErrUnsupported. Closing it leaves the machine running.
func (m *Machine) Console(ctx context.Context) (vm.Term, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if m.console == nil {
		return nil, fmt.Errorf("firecracker: no serial console to attach; set Backend.Console to ConsolePrint: %w", errors.ErrUnsupported)
	}
	return m.console.Attach(), nil
}

// Status reports the machine's lifecycle state, PID, and socket paths. The
// API socket is reported as the monitor socket.
func (m *Machine) Status(ctx context.Context) (backend.Status, error) {
	if err := ctx.Err(); err != nil {
		return backend.Status{}, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.status, nil
}

var (
	_ backend.Machine         = (*Machine)(nil)
	_ backend.StatusReporter  = (*Machine)(nil)
	_ backend.ConsoleProvider = (*Machine)(nil)
)
