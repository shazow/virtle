// Package session is the virtle CLI's session layer over the qemu VM
// machinery: boot with CLI semantics, run the foreground session (the
// ssh-ready gate, the interactive SSH attach loop with autoprovision and
// retries, suspend-on-signal, stats), and the out-of-process suspend and
// hotplug commands.
//
// It lives under backend/qemu because today's session implementation is
// qemu-machinery-bound (vsock SSH destinations, chardev readiness
// sockets, QMP-migration suspend), and it is deliberately transitional
// scaffolding: most of it is QGA-era plumbing that the virtle guest
// daemon (design D9) deletes — readiness becomes the daemon handshake,
// autoprovision becomes a host-pushed key at boot, and the residual
// foreground loop moves onto the public backend API. Keep this surface
// minimal; it is not a designed session API.
package session

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"

	"github.com/shazow/virtle/backend"
	"github.com/shazow/virtle/backend/qemu"
	"github.com/shazow/virtle/backend/qemu/internal/vmm"
	"github.com/shazow/virtle/internal/manifest"
	"github.com/shazow/virtle/vm"
)

// Options configures a CLI session.
type Options struct {
	Resume        string   // resume mode: "no", "auto", "force"; default "no"
	SSH           bool     // attach the interactive SSH session loop
	RemoteCommand []string // remote command for the SSH session

	Logger *slog.Logger // default: discard
}

// Run boots the VM described by spec on b with CLI semantics (resume
// modes, --ssh preflight validation, process signal handlers) and runs
// the foreground session to completion. A session that ends in a saved
// suspend reports success. b must be a *qemu.Backend, typically the one
// manifest.Load returns; the session's VM machinery is QEMU-bound.
func Run(ctx context.Context, b backend.Backend, spec *vm.Spec, opts Options) error {
	logger := opts.Logger
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	qb, ok := b.(*qemu.Backend)
	if !ok {
		return fmt.Errorf("session requires the qemu backend, got %T: %w", b, errors.ErrUnsupported)
	}
	mf, err := qb.ResolvedManifest(spec)
	if err != nil {
		return err
	}
	sessionLogger := logger.With("package", "session")
	sessionLogger.Info("starting vm session", "resume", opts.Resume, "ssh", opts.SSH)
	sessionOpts := vmm.SessionOptions{
		SSH:           opts.SSH,
		RemoteCommand: opts.RemoteCommand,
		Stdout:        os.Stdout,
		Stderr:        os.Stderr,
	}
	vmHandle, err := vmm.StartSessionVM(ctx, mf, vmm.StartOptions{
		Resume: vmm.ResumeMode(opts.Resume),
	}, sessionOpts, vmm.Config{
		Logger:        logger,
		ConsoleOutput: os.Stderr,
	})
	if err != nil {
		if vmm.IsSavedSuspendExit(err) {
			return nil
		}
		return err
	}
	sessionLogger.Info("vm started; entering foreground session")
	err = vmm.RunSession(ctx, vmHandle, sessionOpts)
	if err == nil {
		sessionLogger.Info("vm session ended")
	}
	return err
}

// Suspend suspends a running session out-of-process: over the control
// socket, falling back to signalling the launch process.
func Suspend(ctx context.Context, mf *manifest.Manifest) error {
	return vmm.Suspend(ctx, mf)
}

// Hotplug attaches or detaches a manifest-declared hotplug device on a
// running session, over the control socket.
func Hotplug(ctx context.Context, mf *manifest.Manifest, id string, detach bool) error {
	return vmm.Hotplug(ctx, mf, id, detach)
}

// ExitCode maps session errors onto CLI exit codes.
func ExitCode(err error) int { return vmm.ExitCode(err) }
