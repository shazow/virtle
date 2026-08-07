package manager

import (
	"context"

	"github.com/shazow/virtle/internal/hotplug"
	controlpkg "github.com/shazow/virtle/internal/manager/control"
	"github.com/shazow/virtle/internal/manager/launch"
	"github.com/shazow/virtle/internal/qmpclient"
)

type managerHotplugFeature struct {
	runner hotplug.Runner
}

func (f managerHotplugFeature) Hotplug(ctx context.Context, req controlpkg.HotplugRequest) (controlpkg.HotplugResponse, error) {
	if req.Detach {
		if err := f.runner.Detach(ctx, req.ID); err != nil {
			return controlpkg.HotplugResponse{}, launch.WrapHotplugError(err)
		}
		return controlpkg.HotplugResponse{ID: req.ID, Detach: true}, nil
	}
	if err := f.runner.Attach(ctx, req.ID); err != nil {
		return controlpkg.HotplugResponse{}, launch.WrapHotplugError(err)
	}
	return controlpkg.HotplugResponse{ID: req.ID}, nil
}

func (m *manager) hotplugFeature(client qmpclient.Client) managerHotplugFeature {
	return managerHotplugFeature{runner: m.hotplugRunner(client)}
}

func (m *manager) hotplugRunner(client qmpclient.Client) hotplug.Runner {
	launchManifest := m.launchManifest
	return hotplug.Runner{
		StateDir: launchManifest.ResolvedPersistenceStateDir(),
		WorkDir:  launchManifest.Paths.WorkingDir,
		Devices:  launchManifest.Hotplug,
		Start:    managedProcessStarter{m: m},
		Sockets:  socketReadinessWaiter{m: m},
		QMP:      hotplug.QMPDeviceAdapter{Client: client},
		Guest:    guestCommandRunner{m: m},
	}
}
