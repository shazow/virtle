package vmm

import (
	"context"

	"github.com/shazow/virtle/backend/qemu/internal/launch"
	"github.com/shazow/virtle/backend/qemu/internal/qmpclient"
	runtimepkg "github.com/shazow/virtle/backend/qemu/internal/runtime"
	"github.com/shazow/virtle/internal/console"
)

type runningLaunch struct {
	runtime        *runtimepkg.Core
	plan           *launch.Plan
	stats          *launch.Stats
	qmp            qmpclient.Client
	suspend        *launch.SuspendCoordinator
	suspendHandler *launchSuspendHandler
	processes      *launch.ProcessSet
	console        *console.Hub // serial console fan-out; nil without a print console
}

func (r *runningLaunch) Close() error {
	if r == nil || r.runtime == nil {
		return nil
	}
	return r.runtime.Shutdown(context.Background())
}
