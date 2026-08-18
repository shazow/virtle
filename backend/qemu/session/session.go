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
	"log/slog"

	"github.com/shazow/virtle/backend/qemu/internal/balloon"
	"github.com/shazow/virtle/backend/qemu/internal/vmm"
	"github.com/shazow/virtle/internal/manifest"
)

// Options configures a CLI session.
type Options struct {
	Resume        string   // resume mode: "no", "auto", "force"; default "no"
	SSH           bool     // attach the interactive SSH session loop
	RemoteCommand []string // remote command for the SSH session
}

// Run boots the VM with CLI semantics (resume modes, --ssh preflight
// validation, process signal handlers) and runs the foreground session to
// completion. A session that ends in a saved suspend reports success.
func Run(ctx context.Context, mf *manifest.Manifest, opts Options) error {
	sessionOpts := vmm.SessionOptions{
		SSH:           opts.SSH,
		RemoteCommand: opts.RemoteCommand,
	}
	vmHandle, err := vmm.StartSessionVM(ctx, mf, vmm.StartOptions{
		Resume: vmm.ResumeMode(opts.Resume),
	}, sessionOpts)
	if err != nil {
		if vmm.IsSavedSuspendExit(err) {
			return nil
		}
		return err
	}
	return vmm.RunSession(ctx, vmHandle, sessionOpts)
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

// SetLogger sets the machinery's package logger.
func SetLogger(l *slog.Logger) { vmm.SetLogger(l) }

// SetBalloonLogger sets the balloon controller's logger. It is separate
// from SetLogger because the CLI enables balloon logging at a higher
// verbosity tier than the rest of the machinery.
func SetBalloonLogger(l *slog.Logger) { balloon.SetLogger(l) }
