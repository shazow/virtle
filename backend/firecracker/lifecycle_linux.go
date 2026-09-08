package firecracker

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
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/shazow/virtle/backend"
	"github.com/shazow/virtle/internal/control"
	"github.com/shazow/virtle/internal/executor"
	imanifest "github.com/shazow/virtle/internal/manifest"
	"github.com/shazow/virtle/vm"
)

const socketRetryInterval = 10 * time.Millisecond
const killWaitTimeout = 2 * time.Second

// Machine owns one child process and its private runtime directory.
type Machine struct {
	process         *executor.Process
	api             *apiClient
	done            chan struct{}
	stopped         chan struct{}
	runtimeDir      string
	lock            *os.File
	shutdownTimeout time.Duration
	mu              sync.Mutex
	status          backend.Status
	err             error
	cleanupErr      error
	shutdownOnce    sync.Once
	shutdownDone    chan struct{}
	shutdownErr     error
	control         *control.Server
	diagnostics     *diagnosticWriter
	ephemeralState  string
}

func (b *Backend) start(ctx context.Context, mf *imanifest.Manifest, ephemeral bool) (backend.Machine, error) {
	var ephemeralState string
	if ephemeral {
		var err error
		ephemeralState, err = os.MkdirTemp("", "virtle-state-")
		if err != nil {
			return nil, err
		}
		mf.Persistence.StateDir = ephemeralState
	}
	started := false
	defer func() {
		if !started && ephemeralState != "" {
			_ = os.RemoveAll(ephemeralState)
		}
	}()
	stateDir := mf.ResolvedPersistenceStateDir()
	lock, err := lockState(stateDir, mf.Identity.HostName)
	if err != nil {
		return nil, err
	}
	controlPath := filepath.Join(stateDir, "virtle.sock")
	// Binding never removes an existing socket, regular file, or symlink.
	listener, err := net.Listen("unix", controlPath)
	if err != nil {
		_ = lock.Close()
		return nil, fmt.Errorf("listen firecracker control socket: %w", err)
	}
	if err := os.Chmod(controlPath, 0600); err != nil {
		_ = listener.Close()
		_ = lock.Close()
		return nil, err
	}
	// Short, unique names avoid Unix path length limits and never reuse or
	// unlink a caller-supplied API socket. MkdirTemp creates mode 0700.
	dir, err := os.MkdirTemp("", "virtle-fc-")
	if err != nil {
		_ = listener.Close()
		_ = lock.Close()
		return nil, err
	}
	rollback := func() { _ = listener.Close(); _ = os.RemoveAll(dir); _ = lock.Close() }
	socket := filepath.Join(dir, "api.sock")
	if len(socket) >= 108 {
		rollback()
		return nil, fmt.Errorf("firecracker API socket path too long; use a shorter TMPDIR")
	}
	cfg := mf.Firecracker
	cmd := exec.Command(cfg.Binary, "--api-sock", socket)
	cmd.Dir = mf.Paths.WorkingDir
	cmd.WaitDelay = time.Second // inherited output descriptors cannot hold teardown open
	cmd.Stdout = io.Discard
	diagnostics := &diagnosticWriter{}
	cmd.Stderr = diagnostics
	if cfg.Kernel.Serial == imanifest.KernelSerialPrint {
		output := b.ConsoleOutput
		if output == nil {
			output = os.Stderr
		}
		serialized := &lockedWriter{writer: output}
		cmd.Stdout, cmd.Stderr = serialized, io.MultiWriter(diagnostics, serialized)
	}
	logger := b.Logger
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	logger.Info("starting firecracker", "binary", cfg.Binary, "api_socket", socket)
	process, err := startProcess(cmd)
	if err != nil {
		rollback()
		return nil, err
	}
	m := &Machine{process: process, api: newAPIClient(socket), done: make(chan struct{}), stopped: make(chan struct{}), runtimeDir: dir, lock: lock, shutdownTimeout: cfg.ShutdownTimeout, shutdownDone: make(chan struct{}), status: backend.Status{State: backend.StateStarting, PID: process.PID(), Paths: backend.StatusPaths{MonitorSocket: socket}, Stats: backend.RuntimeStats{StartedAt: time.Now()}}}
	m.diagnostics = diagnostics
	m.ephemeralState = ephemeralState
	m.status.Paths.ControlSocket = controlPath
	router, err := control.NewMachineRouter(controlMachine{m})
	if err != nil {
		_ = process.KillAndWait()
		rollback()
		return nil, err
	}
	m.control, err = control.NewServer(router)
	if err != nil {
		_ = process.KillAndWait()
		rollback()
		return nil, err
	}
	go func() {
		if err := m.control.Serve(listener); err != nil {
			logger.Warn("firecracker control server stopped", "err", err)
			_ = m.Kill()
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

func lockState(dir, name string) (*os.File, error) {
	if name == "" || name == "." || name == ".." || filepath.Base(name) != name {
		return nil, fmt.Errorf("firecracker host_name must be a file name")
	}
	// Lstat follows a final symlink when a slash follows it. Strip only
	// trailing separators: preserve root and symlink/.. resolution semantics.
	if strings.HasSuffix(dir, "/") {
		dir = strings.TrimRight(dir, "/")
		if dir == "" {
			dir = "/"
		}
	}
	if err := os.MkdirAll(dir, 0700); err != nil {
		return nil, fmt.Errorf("create firecracker state directory: %w", err)
	}
	info, err := os.Lstat(dir)
	if err != nil {
		return nil, err
	}
	if !info.IsDir() || info.Mode().Perm()&0077 != 0 {
		return nil, fmt.Errorf("firecracker state directory must be a private directory (0700): %q", dir)
	}
	// OpenRoot confines lock lookup even if another process renames the dir.
	root, err := os.OpenRoot(dir)
	if err != nil {
		return nil, err
	}
	defer root.Close()
	f, err := root.OpenFile(name+".lock", os.O_CREATE|os.O_RDWR|syscall.O_NOFOLLOW, 0600)
	if err != nil {
		return nil, fmt.Errorf("open firecracker lock: %w", err)
	}
	info, err = f.Stat()
	if err != nil || !info.Mode().IsRegular() {
		_ = f.Close()
		return nil, fmt.Errorf("firecracker lock must be a regular file")
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("firecracker state directory is in use: %w", err)
	}
	return f, nil
}

func (m *Machine) waitAPI(ctx context.Context, socket string) error {
	// Firecracker offers no inherited readiness descriptor. Only connection
	// establishment is retried; configuration mutations are never retried.
	timer := time.NewTicker(socketRetryInterval)
	defer timer.Stop()
	for {
		conn, err := (&net.Dialer{}).DialContext(ctx, "unix", socket)
		if err == nil {
			return conn.Close()
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("wait for firecracker API: %w", ctx.Err())
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

func (m *Machine) Done() <-chan struct{} { return m.done }
func (m *Machine) Err() error            { m.mu.Lock(); defer m.mu.Unlock(); return m.err }
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
func (m *Machine) Kill() error { return m.finish(m.kill()) }

func (m *Machine) kill() error {
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

// Shutdown asks the x86 guest's init to handle Ctrl-Alt-Del and waits for exit.
// The configured timeout and ctx both bound the wait; expiration kills the
// process group. Guests must provide a Ctrl-Alt-Del handler for clean shutdown.
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
			shutdownCtx, cancel := context.WithTimeout(context.Background(), m.shutdownTimeout)
			defer cancel()
			m.mu.Lock()
			if m.status.State != backend.StateStopped {
				m.status.State = backend.StateStopping
			}
			m.mu.Unlock()
			err := m.api.put(shutdownCtx, "/actions", action{Type: "SendCtrlAltDel"})
			if err == nil {
				err = m.waitStopped(shutdownCtx)
			}
			if err != nil {
				err = errors.Join(err, m.kill())
			}
			m.shutdownErr = err
		}()
	})
	select {
	case <-m.shutdownDone:
		return m.shutdownErr
	case <-ctx.Done():
		return errors.Join(ctx.Err(), m.kill())
	}
}

// RPC handlers wait for process cleanup, but cannot wait for their own response
// delivery. Public completion waits for both cleanup and control-server drain.
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

func (m *Machine) finish(err error) error {
	select {
	case <-m.stopped:
		<-m.done
	default:
		// A process wedged during kill must still respect the teardown bound.
	}
	return err
}

func (m *Machine) RemoteControl() (vm.Guest, error) {
	return nil, fmt.Errorf("firecracker has no guest control transport: %w", errors.ErrUnsupported)
}
func (m *Machine) Status(ctx context.Context) (backend.Status, error) {
	if err := ctx.Err(); err != nil {
		return backend.Status{}, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.status, nil
}

var _ backend.Machine = (*Machine)(nil)
var _ backend.StatusReporter = (*Machine)(nil)
