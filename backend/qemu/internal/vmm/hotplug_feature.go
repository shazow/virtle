package vmm

import (
	"context"
	"crypto/sha256"
	"fmt"

	"github.com/shazow/virtle/backend/qemu/internal/hotplug"
	"github.com/shazow/virtle/backend/qemu/internal/launch"
	"github.com/shazow/virtle/backend/qemu/internal/qmpclient"
	controlpkg "github.com/shazow/virtle/internal/control"
	"github.com/shazow/virtle/internal/manifest"
	"github.com/shazow/virtle/vm"
)

type managerHotplugFeature struct {
	runner   hotplug.Runner
	resolver *manifest.Manifest
}

func (f managerHotplugFeature) Hotplug(ctx context.Context, req controlpkg.HotplugRequest) (controlpkg.HotplugResponse, error) {
	if req.Device != nil {
		device, err := controlHotplugDevice(f.resolver, *req.Device)
		if err != nil {
			return controlpkg.HotplugResponse{}, err
		}
		if req.Detach {
			if err := f.runner.Detach(ctx, device.ID); err != nil {
				return controlpkg.HotplugResponse{}, launch.WrapHotplugError(err)
			}
			return controlpkg.HotplugResponse{ID: device.ID, Detach: true}, nil
		}
		if err := f.runner.AttachDevice(ctx, device); err != nil {
			return controlpkg.HotplugResponse{}, launch.WrapHotplugError(err)
		}
		return controlpkg.HotplugResponse{ID: device.ID}, nil
	}
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

func controlHotplugDevice(resolver interface {
	ResolveHotplugMount(manifest.MountEntry) (manifest.HotplugDevice, error)
	ResolveHotplugNetwork(manifest.NetworkInput) (manifest.HotplugDevice, error)
}, req controlpkg.DeviceRequest) (manifest.HotplugDevice, error) {
	switch {
	case req.Share != nil:
		share := *req.Share
		return resolver.ResolveHotplugMount(manifest.VirtioFSMountInput{
			Type:       manifest.MountTypeVirtioFS,
			MountInput: manifest.MountInput{Tag: share.Tag, SourcePath: share.HostPath, ReadOnly: share.ReadOnly},
			Target:     share.GuestPath,
		})
	case req.Disk != nil:
		disk := *req.Disk
		id := controlDeviceID("disk", disk.Path)
		return resolver.ResolveHotplugMount(manifest.ImageMountInput{
			Type: manifest.MountTypeImage, SourcePath: disk.Path,
			Image: manifest.ImageInput{Format: disk.Format, Serial: &id},
		})
	case req.Forward != nil:
		forward := *req.Forward
		proto := forward.Proto
		if proto == "" {
			proto = vm.TCP
		}
		identity := string(proto) + "\x00" + forward.HostAddr + "\x00" + forward.GuestAddr
		return resolver.ResolveHotplugNetwork(manifest.NetworkInput{
			ID: controlDeviceID("fwd", identity), MAC: controlDeviceMAC(identity),
			Forward: []manifest.ForwardPort{{Proto: string(proto), From: "host", Host: forward.HostAddr, Guest: forward.GuestAddr}},
		})
	default:
		return manifest.HotplugDevice{}, &controlpkg.RPCError{Code: controlpkg.ErrInvalidParams, Message: "hotplug device is required"}
	}
}

func controlDeviceID(kind, identity string) string {
	digest := sha256.Sum256([]byte(identity))
	return fmt.Sprintf("%s-%x", kind, digest[:8])
}

func controlDeviceMAC(identity string) string {
	digest := sha256.Sum256([]byte(identity))
	return fmt.Sprintf("02:%02x:%02x:%02x:%02x:%02x", digest[0], digest[1], digest[2], digest[3], digest[4])
}

func (m *manager) hotplugFeature(client qmpclient.Client) managerHotplugFeature {
	return managerHotplugFeature{runner: m.hotplugRunner(client), resolver: m.launchManifest}
}

func (m *manager) hotplugRunner(client qmpclient.Client) hotplug.Runner {
	launchManifest := m.launchManifest
	return hotplug.Runner{
		WorkDir: launchManifest.Paths.WorkingDir,
		Devices: launchManifest.Hotplug,
		Start:   managedProcessStarter{m: m},
		Sockets: socketReadinessWaiter{m: m},
		QMP:     hotplug.QMPDeviceAdapter{Client: client},
		Guest:   guestCommandRunner{m: m},
		Runtime: m.hotplugRuntime,
		Ports:   launchManifest.QEMU.Hotplug.PCIEPorts,
	}
}
