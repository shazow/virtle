package vmm

import (
	"context"

	"github.com/shazow/virtle/backend/qemu/internal/launch"
	"github.com/shazow/virtle/internal/executor"
)

// GuestReadiness gates a session on the guest being reachable. It is the
// swap point for the virtle guest daemon (design D9): the QGA-era
// implementation below waits for the guest's ssh-ready socket signal; the
// daemon replaces it with the readiness handshake on the guest's
// virtle.sock, injected via Config.GuestReadiness without touching
// session code. Together with Config.GuestAgentDialer (the guest-control
// dial seam), these are the two seams the daemon migration swaps.
type GuestReadiness interface {
	// WaitReady blocks until the guest is reachable for session use, or
	// a watched process exits, or ctx ends. A plan with no readiness
	// signal configured returns immediately.
	WaitReady(ctx context.Context, plan *launch.Plan, watchers executor.Group) error
}

// guestReadiness returns the configured readiness gate, defaulting to the
// ssh-ready socket wait.
func (m *manager) guestReadiness() GuestReadiness {
	if m.readiness != nil {
		return m.readiness
	}
	return sshReadySocketReadiness{m: m}
}

// sshReadySocketReadiness is the QGA-era readiness gate: the guest's init
// connects to a dedicated chardev socket once sshd is up.
type sshReadySocketReadiness struct {
	m *manager
}

func (r sshReadySocketReadiness) WaitReady(ctx context.Context, plan *launch.Plan, watchers executor.Group) error {
	if plan.Paths.SSHReadySocket == "" {
		return nil
	}
	r.m.sshLifecycleLogger().Info("waiting for ssh readiness")
	return r.m.waitForSSHReady(ctx, plan.Paths.SSHReadySocket, watchers)
}
