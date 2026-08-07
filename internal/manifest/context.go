package manifest

import (
	"context"
	"fmt"
	"time"
)

// GuestCommandContext bounds a guest-agent command with timeout. Zero means
// the command is bounded only by ctx.
func GuestCommandContext(ctx context.Context, timeout time.Duration) (context.Context, context.CancelFunc) {
	if timeout <= 0 {
		return context.WithCancel(ctx)
	}
	return context.WithTimeoutCause(ctx, timeout, fmt.Errorf("guest command timed out after %s", timeout))
}

// GuestCommandContext bounds a guest-agent command with the manifest's guest
// command timeout.
func (m *Manifest) GuestCommandContext(ctx context.Context) (context.Context, context.CancelFunc) {
	return GuestCommandContext(ctx, m.QEMU.GuestAgent.CommandTimeout)
}
