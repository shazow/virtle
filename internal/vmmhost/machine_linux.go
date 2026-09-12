package vmmhost

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"sync"
	"syscall"
	"time"

	"github.com/shazow/virtle/backend"
	"github.com/shazow/virtle/internal/console"
	"github.com/shazow/virtle/internal/control"
	"github.com/shazow/virtle/internal/executor"
	imanifest "github.com/shazow/virtle/internal/manifest"
	"github.com/shazow/virtle/vm"
)

// Launch describes one VMM process to Start. Command, Configure, and
// Graceful are the VMM-specific parts; everything else is shared.
type Launch struct {
	Name             string              // VMM name for logs and errors, such as "firecracker"
	RuntimeDirPrefix string              // prefix of the private directory holding the API socket
	Manifest         *imanifest.Manifest // supplies the lock, control socket, and working directory
	EphemeralState   string              // temporary state directory removed when the machine exits, or ""
	StartupTimeout   time.Duration       // bound on API startup and Configure
	ShutdownTimeout  time.Duration       // bound on Graceful before the VMM is killed
	Console          bool                // wire the guest serial port, carried on the process's standard streams, to ConsoleOutput and Machine.Console
	ConsoleOutput    io.Writer           // where console output is printed; required when Console is set
	Logger           *slog.Logger        // lifecycle logs; nil discards them
	Networks         []backend.NetworkStatus

	// ConsoleTerminal hands the VMM a pseudo-terminal as its standard input
	// and output instead of pipes, for a VMM that reads console input only
	// from a terminal (Cloud Hypervisor). Output still reaches ConsoleOutput
	// and Machine.Console; the VMM's stderr stays a pipe.
	ConsoleTerminal bool
	// ConsoleInteractive puts the guest's serial port on the process's own
	// terminal instead of the hub, as QEMU's interactive console does: the
	// VMM reads the process's standard input, prints to ConsoleOutput, and
	// stays in the caller's process group so it may read the terminal.
	// Machine.Console is then unavailable. It takes precedence over Console.
	ConsoleInteractive bool

	// Command returns the VMM command serving its API on socket. Start sets
	// its working directory, process group, and standard streams.
	Command func(socket string) *exec.Cmd
	// Prepare runs before the VMM starts (disk images, helper daemons). The
	// returned cleanup, when non-nil, runs once the VMM has exited, before the
	// lock is released, and when startup fails before the VMM runs.
	Prepare func(ctx context.Context) (cleanup func() error, err error)
	// Configure runs once the API socket accepts connections; it configures
	// and boots the guest. ctx ends when the VMM exits or StartupTimeout passes.
	Configure func(ctx context.Context, socket string) error
	// Graceful asks the guest to stop; the VMM is expected to exit on its own
	// afterwards. Nil means Shutdown reports errors.ErrUnsupported and kills.
	Graceful func(ctx context.Context, socket string) error
}

func (l Launch) logger() *slog.Logger {
	if l.Logger != nil {
		return l.Logger
	}
	return slog.New(slog.DiscardHandler)
}

// Machine is one running VMM process: the private directory holding its API
// socket, the control socket, and the VM-name lock shared with QEMU.
type Machine struct {
	name            string
	socket          string
	process         *executor.Process
	control         *control.Server
	lock            io.Closer
	runtimeDir      string // private directory holding the API socket
	ephemeralState  string // temporary state directory removed on exit, or ""
	shutdownTimeout time.Duration
	diagnostics     *DiagnosticWriter
	console         *console.Hub  // serial console fan-out; nil when the console is off
	terminal        *os.File      // master of the VMM's pseudo-terminal; nil when its console rides pipes
	consoleDone     chan struct{} // closes once the terminal's last output reached the hub
	graceful        func(ctx context.Context, socket string) error
	cleanup         func() error // Prepare's cleanup, or nil
	logger          *slog.Logger

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

// newMachine wires a launch to its runtime resources before the VMM process
// exists. The caller starts the process, the control server, and the reaper.
func newMachine(l Launch, socket string, lock io.Closer, runtimeDir string, cleanup func() error) *Machine {
	return &Machine{
		name:            l.Name,
		socket:          socket,
		lock:            lock,
		runtimeDir:      runtimeDir,
		ephemeralState:  l.EphemeralState,
		shutdownTimeout: l.ShutdownTimeout,
		diagnostics:     &DiagnosticWriter{},
		graceful:        l.Graceful,
		cleanup:         cleanup,
		logger:          l.logger(),
		stopped:         make(chan struct{}),
		done:            make(chan struct{}),
		shutdownDone:    make(chan struct{}),
	}
}

// Start launches the VMM and returns once Configure has succeeded. That is
// not guest readiness. Canceling ctx afterwards kills the machine; on
// failure every resource, EphemeralState included, is released.
func Start(ctx context.Context, l Launch) (*Machine, error) {
	mf := l.Manifest
	started := false
	defer func() {
		if !started && l.EphemeralState != "" {
			_ = os.RemoveAll(l.EphemeralState)
		}
	}()
	lock, err := LockState(mf)
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
	dir, err := os.MkdirTemp("", l.RuntimeDirPrefix)
	if err != nil {
		_ = listener.Close()
		_ = lock.Close()
		return nil, err
	}
	var cleanup func() error
	rollback := func() {
		if cleanup != nil {
			_ = cleanup()
		}
		_ = listener.Close()
		_ = os.RemoveAll(dir)
		_ = lock.Close()
	}
	socket := filepath.Join(dir, "api.sock")
	if len(socket) >= UnixPathMax {
		rollback()
		return nil, fmt.Errorf("%s API socket path %q is too long; set a shorter TMPDIR", l.Name, socket)
	}
	logger := l.logger()
	if l.Prepare != nil {
		cleanup, err = l.Prepare(ctx)
		if err != nil {
			rollback()
			return nil, err
		}
	}
	cmd := l.Command(socket)
	cmd.Dir = mf.Paths.WorkingDir
	cmd.WaitDelay = time.Second // inherited output descriptors cannot hold teardown open
	// The VMM leads its own process group, so teardown signals reach anything
	// it spawned, and the host's Ctrl-C does not reach it directly; an
	// interactive console must stay in the foreground process group to read
	// the terminal.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: !l.ConsoleInteractive}
	m := newMachine(l, socket, lock, dir, cleanup)
	cmd.Stdout = io.Discard
	cmd.Stderr = m.diagnostics
	var slave *os.File // the VMM's end of its pseudo-terminal, until it holds its own
	switch {
	case l.ConsoleInteractive:
		// The guest's serial port is the process's own terminal: the VMM
		// reads what the user types and prints where ConsoleOutput points.
		serialized := &lockedWriter{writer: l.ConsoleOutput}
		cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, serialized, io.MultiWriter(m.diagnostics, serialized)
	case l.Console:
		// The guest's serial port rides the VMM's standard streams: the hub
		// prints it, retains it, and serves Machine.Console sessions. The
		// VMM's own stderr diagnostics share the output writer.
		serialized := &lockedWriter{writer: l.ConsoleOutput}
		hub, err := console.New(serialized, logger.With("host_name", mf.Identity.HostName))
		if err != nil {
			rollback()
			return nil, err
		}
		m.console = hub
		cmd.Stdin, cmd.Stdout, cmd.Stderr = hub.Stdin(), hub, io.MultiWriter(m.diagnostics, serialized)
		if l.ConsoleTerminal {
			m.terminal, slave, err = openTerminal()
			if err != nil {
				_ = hub.Close()
				rollback()
				return nil, err
			}
			m.consoleDone = make(chan struct{})
			cmd.Stdin, cmd.Stdout = slave, slave
		}
	}
	logger.Info("starting "+l.Name, "binary", cmd.Args[0], "api_socket", socket)
	m.process, err = (&executor.Runner{Logger: logger}).Start(cmd)
	if slave != nil {
		// Only the VMM's descriptors keep the terminal open now, so its exit
		// ends the master's reads.
		_ = slave.Close()
	}
	if err != nil {
		m.closeConsole()
		rollback()
		return nil, err
	}
	if m.terminal != nil {
		m.serveTerminal()
	} else if m.console != nil {
		m.console.Started()
	}
	// The graceful path is the VMM's API, never SIGTERM: Stop only ever
	// hard-kills here, and the grace period bounds its wait for the reaper.
	m.process.SetGracePeriod(killWaitTimeout)
	m.status = backend.Status{
		State:    backend.StateStarting,
		PID:      m.process.PID(),
		Paths:    backend.StatusPaths{ControlSocket: controlPath, MonitorSocket: socket},
		Stats:    backend.RuntimeStats{StartedAt: time.Now()},
		Networks: l.Networks,
	}
	router, err := control.NewMachineRouter(controlMachine{m})
	if err != nil {
		_ = m.process.KillAndWait()
		m.closeConsole()
		rollback()
		return nil, err
	}
	m.control, err = control.NewServer(router)
	if err != nil {
		_ = m.process.KillAndWait()
		m.closeConsole()
		rollback()
		return nil, err
	}
	go func() {
		if err := m.control.Serve(listener); err != nil {
			// The control socket is a peripheral: losing it leaves the machine
			// running for the caller that holds it. Serve has let go of the
			// listener by now, so close it here rather than through the server.
			logger.Warn("control server stopped", "err", err)
			_ = listener.Close()
		}
	}()
	<-m.control.Started()
	go m.reap()
	started = true // the reaper now owns all cleanup, including failed startup
	startupCtx, cancel := context.WithTimeout(ctx, l.StartupTimeout)
	defer cancel()
	// Process exit interrupts both API startup and in-flight configuration,
	// before the reaper's cleanup stops Prepare's helpers, so a Configure
	// still waiting on them sees the VMM's exit as the cause, not theirs.
	go func() {
		select {
		case <-m.process.Done():
			cancel()
		case <-startupCtx.Done():
		}
	}()
	if err := m.waitAPI(startupCtx); err != nil {
		return nil, m.startupFailure(err)
	}
	if err := l.Configure(startupCtx, socket); err != nil {
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

// LockState creates the state directory privately when it is missing (an
// existing directory is used as it is, as the QEMU backend does), then takes
// the exclusive VM-name lock every backend shares, so two launches of one
// manifest exclude each other whichever backend they use.
func LockState(mf *imanifest.Manifest) (*os.File, error) {
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

func (m *Machine) waitAPI(ctx context.Context) error {
	// Neither VMM offers an inherited readiness descriptor. Only connection
	// establishment is retried; configuration mutations are never retried.
	timer := time.NewTicker(socketRetryInterval)
	defer timer.Stop()
	for {
		conn, err := (&net.Dialer{}).DialContext(ctx, "unix", m.socket)
		if err == nil {
			return conn.Close()
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("wait for %s API at %q: %w", m.name, m.socket, ctx.Err())
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
			err = errors.Join(err, fmt.Errorf("%s exited during startup: %w", m.name, exitErr))
		} else {
			err = errors.Join(err, fmt.Errorf("%s exited during startup", m.name))
		}
	}
	err = errors.Join(err, m.Kill())
	if text := m.diagnostics.String(); text != "" {
		err = fmt.Errorf("%w; %s stderr: %q", err, m.name, text)
	}
	return err
}

func (m *Machine) reap() {
	err := m.process.Wait()
	var cleanupErr error
	if m.cleanup != nil {
		cleanupErr = m.cleanup() // helpers stop while the state lock is still held
	}
	cleanupErr = errors.Join(cleanupErr, m.control.Close(), os.RemoveAll(m.runtimeDir), m.lock.Close())
	if m.terminal != nil {
		// The VMM's descriptors are gone: let the last of its output reach
		// the hub before the console closes, bounded in case something it
		// spawned inherited the terminal.
		select {
		case <-m.consoleDone:
		case <-time.After(killWaitTimeout):
		}
		cleanupErr = errors.Join(cleanupErr, m.terminal.Close())
	}
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
	m.control.WaitLifecycle() // as QEMU: a slow status client cannot hold up done
	close(m.done)
}

// serveTerminal copies the guest's console between the pseudo-terminal and
// the hub: output to the hub, and so to ConsoleOutput and attached Terms;
// Term input to the VMM. The output copier ends once the VMM's side of the
// terminal is gone, the input copier once the hub closes its pipe.
func (m *Machine) serveTerminal() {
	go func() {
		defer close(m.consoleDone)
		if _, err := io.Copy(m.console, m.terminal); err != nil && !consoleClosed(err) {
			m.logger.Warn("console output stopped", "err", err)
		}
	}()
	go func() {
		if _, err := io.Copy(m.terminal, m.console.Stdin()); err != nil && !consoleClosed(err) {
			m.logger.Warn("console input stopped", "err", err)
		}
	}()
}

// consoleClosed reports the errors that end a console copier in the normal
// course of teardown: the VMM's side of the terminal gone, or the master and
// the hub's pipe closed by the reaper.
func consoleClosed(err error) bool {
	return terminalClosed(err) || errors.Is(err, os.ErrClosed) || errors.Is(err, io.ErrClosedPipe)
}

// closeConsole releases the hub and the pseudo-terminal of a launch that
// failed before the reaper existed to do it; closing the master ends the
// copiers serveTerminal may have started.
func (m *Machine) closeConsole() {
	if m.terminal != nil {
		_ = m.terminal.Close()
	}
	if m.console != nil {
		_ = m.console.Close()
	}
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

// Kill hard-stops the VMM and releases runtime state.
func (m *Machine) Kill() error { return m.finish(m.kill()) }

func (m *Machine) kill() error {
	// Stop with an expired context skips the graceful rungs: it SIGKILLs the
	// process group and bounds its own wait for the process.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := m.process.Stop(ctx)
	if exited, _ := m.process.PollExit(); exited {
		// What the reaper still does after the exit (Prepare's cleanup,
		// the console drain, the runtime files) is bounded on its own, so
		// wait it out rather than report a teardown that is completing.
		<-m.stopped
		m.mu.Lock()
		defer m.mu.Unlock()
		return errors.Join(err, m.cleanupErr)
	}
	// A process wedged past SIGKILL keeps the caller bounded instead.
	waitCtx, stop := context.WithTimeout(context.Background(), killWaitTimeout)
	defer stop()
	select {
	case <-m.stopped:
		m.mu.Lock()
		defer m.mu.Unlock()
		return errors.Join(err, m.cleanupErr)
	case <-waitCtx.Done():
		return errors.Join(err, fmt.Errorf("%s teardown: %w", m.name, waitCtx.Err()))
	}
}

// Shutdown sends Launch.Graceful to the guest and waits for the VMM to exit,
// killing it when Launch.ShutdownTimeout or ctx expires or the request
// fails. Repeated and concurrent calls share one attempt.
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
	if m.graceful == nil {
		return errors.Join(fmt.Errorf("%s has no graceful shutdown request: %w", m.name, errors.ErrUnsupported), m.kill())
	}
	ctx, cancel := context.WithTimeout(context.Background(), m.shutdownTimeout)
	defer cancel()
	m.mu.Lock()
	if m.status.State != backend.StateStopped {
		m.status.State = backend.StateStopping
	}
	m.mu.Unlock()
	if err := m.graceful(ctx, m.socket); err != nil {
		// A guest that powered off on its own while the request was in
		// flight reached the state shutdown wanted; only a live VMM is
		// killed. A request the VMM cannot make at all kills regardless.
		if exited, _ := m.process.PollExit(); !exited || errors.Is(err, errors.ErrUnsupported) {
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
// known to have stopped; a process wedged past kill's SIGKILL keeps the
// caller bounded by kill's own timeout instead.
func (m *Machine) finish(err error) error {
	select {
	case <-m.stopped:
		<-m.done
	default:
	}
	return err
}

// RemoteControl reports errors.ErrUnsupported: these machines have no guest
// control transport yet.
func (m *Machine) RemoteControl() (vm.Guest, error) {
	return nil, fmt.Errorf("%s has no guest control transport: %w", m.name, errors.ErrUnsupported)
}

// Console implements backend.ConsoleProvider: a vm.Term over the guest's
// serial port, available when Launch.Console was set. The session replays
// the recent console output first, so one attached after boot still sees
// what the guest printed; its Resize and Wait report errors.ErrUnsupported.
// Closing it leaves the machine running.
func (m *Machine) Console(ctx context.Context) (vm.Term, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if m.console == nil {
		return nil, fmt.Errorf("%s: no serial console to attach; set Backend.Console to ConsolePrint: %w", m.name, errors.ErrUnsupported)
	}
	return m.console.Attach(), nil
}

// Status reports the machine's lifecycle state, PID, socket paths, and NICs.
// The API socket is reported as the monitor socket.
func (m *Machine) Status(ctx context.Context) (backend.Status, error) {
	if err := ctx.Err(); err != nil {
		return backend.Status{}, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	status := m.status
	status.Networks = slices.Clone(status.Networks)
	return status, nil
}

var (
	_ backend.Machine         = (*Machine)(nil)
	_ backend.StatusReporter  = (*Machine)(nil)
	_ backend.ConsoleProvider = (*Machine)(nil)
)
