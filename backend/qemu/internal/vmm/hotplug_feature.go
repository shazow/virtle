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

// hotplugResolver lowers manifest-shaped device inputs into executable
// hotplug plans; *manifest.Manifest implements it.
type hotplugResolver interface {
	ResolveHotplugMount(manifest.MountEntry) (manifest.HotplugDevice, error)
	ResolveHotplugNetwork(manifest.NetworkInput) (manifest.HotplugDevice, error)
}

// hotplugDeviceFor maps the sealed vm.Device union onto manifest inputs and
// resolves them into an executable hotplug device description. It serves the
// public backend (Machine.Attach and Detach) and control-socket device
// requests alike.
func hotplugDeviceFor(resolver hotplugResolver, dev vm.Device) (manifest.HotplugDevice, error) {
	switch d := dev.(type) {
	case vm.Share:
		if d.Tag == "" {
			return manifest.HotplugDevice{}, fmt.Errorf("share device: Tag is required")
		}
		return resolver.ResolveHotplugMount(manifest.VirtioFSMountInput{
			Type:       manifest.MountTypeVirtioFS,
			MountInput: manifest.MountInput{Tag: d.Tag, SourcePath: d.HostPath, ReadOnly: d.ReadOnly},
			Target:     d.GuestPath,
		})
	case vm.Disk:
		if d.Path == "" {
			return manifest.HotplugDevice{}, fmt.Errorf("disk device: Path is required")
		}
		id := deviceID("disk", d.Path)
		return resolver.ResolveHotplugMount(manifest.ImageMountInput{
			Type:       manifest.MountTypeImage,
			SourcePath: d.Path,
			Image:      manifest.ImageInput{Format: d.Format, Serial: &id},
		})
	case vm.Forward:
		proto := d.Proto
		if proto == "" {
			proto = vm.TCP
		}
		identity := string(proto) + "\x00" + d.HostAddr + "\x00" + d.GuestAddr
		return resolver.ResolveHotplugNetwork(manifest.NetworkInput{
			ID:  deviceID("fwd", identity),
			MAC: deviceMAC(identity),
			Forward: []manifest.ForwardPort{{
				Proto: string(proto),
				From:  "host",
				Host:  d.HostAddr,
				Guest: d.GuestAddr,
			}},
		})
	default:
		return manifest.HotplugDevice{}, fmt.Errorf("unsupported device type %T", dev)
	}
}

func controlHotplugDevice(resolver hotplugResolver, req controlpkg.DeviceRequest) (manifest.HotplugDevice, error) {
	switch {
	case req.Share != nil:
		return hotplugDeviceFor(resolver, *req.Share)
	case req.Disk != nil:
		return hotplugDeviceFor(resolver, *req.Disk)
	case req.Forward != nil:
		return hotplugDeviceFor(resolver, *req.Forward)
	default:
		return manifest.HotplugDevice{}, controlpkg.InvalidParams("hotplug device is required")
	}
}

// deviceID derives a stable, collision-resistant QEMU ID without embedding
// caller-controlled path or address syntax.
func deviceID(kind, identity string) string {
	digest := sha256.Sum256([]byte(identity))
	return fmt.Sprintf("%s-%x", kind, digest[:8])
}

func deviceMAC(identity string) string {
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
