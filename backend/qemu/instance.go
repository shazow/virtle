package qemu

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sync"

	"github.com/shazow/virtle/backend"
	"github.com/shazow/virtle/internal/manager/launch"
	"github.com/shazow/virtle/internal/manifest"
	"github.com/shazow/virtle/internal/qmpclient"
	"github.com/shazow/virtle/vm"
)

// instance is a running QEMU machine.
//
// It owns everything the launch created — the QEMU process, the virtiofs
// daemons, the monitor connection, and the guest-agent connection once one is
// dialed — and releases all of it on Kill.
type instance struct {
	backend   *qemuBackend
	manifest  *manifest.Manifest
	plan      *launch.Plan
	cid       int
	processes *launch.ProcessSet
	qmp       qmpclient.Client
	logger    *slog.Logger

	mu       sync.Mutex
	guest    vm.Guest
	killed   bool
	killErr  error
	guestErr error
}

var _ backend.Instance = (*instance)(nil)

func (i *instance) Wait(ctx context.Context) error {
	return i.processes.QEMU().WaitContext(ctx)
}

// Kill stops the VM and releases the host resources behind it. It is safe to
// call more than once: later calls return the first call's result.
func (i *instance) Kill() error {
	i.mu.Lock()
	defer i.mu.Unlock()
	if i.killed {
		return i.killErr
	}
	i.killed = true

	var err error
	if i.guest != nil {
		err = errors.Join(err, i.guest.Close())
		i.guest = nil
	}
	if i.qmp != nil {
		err = errors.Join(err, i.qmp.Disconnect())
		i.qmp = nil
	}
	// Stopping the process set covers QEMU and every helper daemon, each
	// escalating from SIGTERM to SIGKILL after its grace period.
	err = errors.Join(err, i.processes.Close(context.Background()))
	err = errors.Join(err, launch.RemoveStaleSockets(i.plan.RuntimeSocketCleanupFiles()...))

	i.killErr = err
	return err
}

// RemoteControl returns guest control for this instance, dialing the guest
// agent on first use. The connection is memoized, so the wait for a booting
// guest is paid once, and it is closed with the instance.
//
// The first call blocks until the guest agent answers or Config.GuestAgentTimeout
// expires, since a guest that has just started booting has not opened its end
// yet. A machine whose guest never runs an agent therefore costs that whole
// timeout before reporting errors.ErrUnsupported.
func (i *instance) RemoteControl() (vm.Guest, error) {
	i.mu.Lock()
	defer i.mu.Unlock()
	if i.killed {
		return nil, fmt.Errorf("instance is stopped: %w", errors.ErrUnsupported)
	}
	if i.guest != nil {
		return i.guest, nil
	}
	if i.guestErr != nil {
		return nil, i.guestErr
	}
	if i.backend.guestControl == nil {
		return nil, fmt.Errorf("backend was built without guest control: %w", errors.ErrUnsupported)
	}
	guest, err := i.backend.guestControl(i)
	if err != nil {
		i.guestErr = err
		return nil, err
	}
	i.guest = guest
	return guest, nil
}

// qgaGuestControl dials the QEMU guest agent and wraps it as a vm.Guest.
func (i *instance) qgaGuestControl() (vm.Guest, error) {
	socketPath := i.manifest.QEMU.GuestAgent.SocketPath
	if socketPath == "" {
		return nil, fmt.Errorf("machine has no guest agent socket: %w", errors.ErrUnsupported)
	}
	resolved, err := i.manifest.ResolvedGuestAgentSocketPath()
	if err != nil {
		return nil, err
	}

	timeout := i.backend.config.guestAgentTimeout()
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	client, err := launch.WaitForGuestAgent(ctx, launch.GuestAgentWait{
		Stage:          "guest agent",
		SocketPath:     resolved,
		SocketWaiter:   i.backend.socketWaiter,
		Dialer:         i.backend.qgaDialer,
		ConnectTimeout: timeout,
		RetryDelay:     qmpRetryDelay,
		PollDelay:      launch.DefaultSocketPollInterval,
		Watchers:       i.processes.Watchers(),
	})
	if err != nil {
		return nil, err
	}
	return &qgaGuest{client: client, timeout: i.manifest.QEMU.GuestAgent.CommandTimeout}, nil
}

// writeFiles places the spec's files in the guest through guest control, which
// is why a spec with files makes Start wait for the guest agent.
func (i *instance) writeFiles(ctx context.Context, files []vm.File) error {
	guest, err := i.RemoteControl()
	if err != nil {
		return err
	}
	for index, file := range files {
		if file.GuestPath == "" {
			return fmt.Errorf("spec.Files[%d].GuestPath is required", index)
		}
		if err := writeGuestFile(ctx, guest, file); err != nil {
			return fmt.Errorf("spec.Files[%d] (%s): %w", index, file.GuestPath, err)
		}
	}
	return nil
}

func writeGuestFile(ctx context.Context, guest vm.Guest, file vm.File) error {
	writer, err := guest.Create(ctx, file.GuestPath, file.Mode)
	if err != nil {
		return err
	}
	if file.Content != nil {
		if _, err := io.Copy(writer, file.Content); err != nil {
			return errors.Join(err, writer.Close())
		}
	}
	return writer.Close()
}

// monitor returns the instance's QMP connection, for the capability
// operations that drive QEMU directly.
func (i *instance) monitor() (qmpclient.Client, error) {
	i.mu.Lock()
	defer i.mu.Unlock()
	if i.qmp == nil {
		return nil, fmt.Errorf("instance is stopped")
	}
	return i.qmp, nil
}
