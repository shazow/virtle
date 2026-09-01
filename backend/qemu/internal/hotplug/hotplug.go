package hotplug

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"syscall"

	"github.com/shazow/virtle/internal/executor"
	"github.com/shazow/virtle/internal/manifest"
)

type Runner struct {
	WorkDir string
	Devices []manifest.HotplugDevice
	Start   ProcessStarter
	Sockets SocketWaiter
	QMP     DeviceQMP
	Guest   GuestRunner
	Runtime *Runtime
	Ports   int
}

type ProcessStarter interface {
	Start(ctx context.Context, cmd *exec.Cmd) (*executor.Process, error)
	Stop(process *executor.Process) error
}

type SocketWaiter interface {
	Wait(ctx context.Context, stage string, socketPaths []string, process *executor.Process) error
}

type DeviceQMP interface {
	AttachDevice(context.Context, manifest.HotplugDevice, string) (func(context.Context), error)
	DetachDevice(context.Context, manifest.HotplugDevice) error
}

type GuestRunner interface {
	Run(ctx context.Context, command []string) error
}

func (r Runner) Attach(ctx context.Context, id string) error {
	r.Runtime.mu.Lock()
	defer r.Runtime.mu.Unlock()
	device, index, err := r.find(id)
	if err != nil {
		return err
	}
	return r.attach(ctx, device, index)
}

// AttachDevice attaches an already-resolved ad-hoc device. Manifest devices
// use Attach so callers can select them by ID.
func (r Runner) AttachDevice(ctx context.Context, device manifest.HotplugDevice) error {
	r.Runtime.mu.Lock()
	defer r.Runtime.mu.Unlock()
	return r.attach(ctx, device, len(r.Devices))
}

func (r Runner) Detach(ctx context.Context, id string) error {
	r.Runtime.mu.Lock()
	defer r.Runtime.mu.Unlock()
	return r.detach(ctx, id)
}

// BusName is the PCIe root port a hotplug device with the given manifest
// index attaches to. It must match the pcie-root-port IDs created at QEMU
// boot (see buildQEMUArgs in backend/qemu/internal/vmm).
func BusName(index int) string {
	return fmt.Sprintf("pcie.hotplug.%d", index)
}

// find resolves one validated manifest device by ID.
func (r Runner) find(id string) (manifest.HotplugDevice, int, error) {
	for i, device := range r.Devices {
		if device.ID == id {
			return device, i, nil
		}
	}
	return manifest.HotplugDevice{}, -1, fmt.Errorf("manifest.hotplug id %q not found", id)
}

func (r Runner) attach(ctx context.Context, device manifest.HotplugDevice, preferredPort int) error {
	if _, exists := r.Runtime.attachments[device.ID]; exists {
		return fmt.Errorf("hotplug %q is already attached", device.ID)
	}
	var proc *executor.Process
	var err error
	if device.Kind == manifest.HotplugKindVirtioFS {
		proc, err = r.attachVirtioFSHost(ctx, device)
		if err != nil {
			return err
		}
	}
	port := r.Runtime.allocatePort(preferredPort, r.Ports)
	if port < 0 {
		r.rollbackHost(proc)
		if r.Ports <= 0 {
			return fmt.Errorf("no PCIe hotplug ports are reserved")
		}
		return fmt.Errorf("all %d PCIe hotplug ports are occupied", r.Ports)
	}
	rollbackQMP, err := r.QMP.AttachDevice(ctx, device, BusName(port))
	if err != nil {
		r.rollbackHost(proc)
		return err
	}
	if rollbackQMP == nil {
		rollbackQMP = func(context.Context) {}
	}
	if err := r.attachGuest(ctx, device); err != nil {
		rollbackQMP(ctx)
		r.rollbackHost(proc)
		return err
	}
	r.Runtime.register(device, port, proc)
	return nil
}

func (r Runner) detach(ctx context.Context, id string) error {
	attached := r.Runtime.attachments[id]
	if attached == nil {
		return fmt.Errorf("hotplug %q is not attached", id)
	}
	device := attached.device
	guestUnmounted := device.Kind == manifest.HotplugKindVirtioFS && device.VirtioFS.Target != ""
	if err := r.detachGuest(ctx, device); err != nil {
		return err
	}
	cleanupCtx := ctx
	if guestUnmounted {
		cleanupCtx = context.WithoutCancel(ctx)
	}
	if err := r.QMP.DetachDevice(cleanupCtx, device); err != nil {
		return err
	}
	if device.Kind == manifest.HotplugKindVirtioFS {
		if attached.helper != nil {
			if err := r.Start.Stop(attached.helper); err != nil {
				return err
			}
		}
		if device.VirtioFS.SocketPath != "" {
			_ = os.Remove(device.VirtioFS.SocketPath)
		}
	}
	r.Runtime.remove(id)
	return nil
}

func (r Runner) attachVirtioFSHost(ctx context.Context, device manifest.HotplugDevice) (*executor.Process, error) {
	fs := device.VirtioFS
	if r.Start == nil {
		return nil, fmt.Errorf("hotplug process starter is not configured")
	}
	cmd := executor.Command(fs.Bin, fs.Args, []string{"VIRTIOFSD_SOCKET=" + fs.SocketPath})
	cmd.Dir = r.WorkDir
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	proc, err := r.Start.Start(ctx, cmd)
	if err != nil {
		return nil, err
	}
	if fs.SocketPath != "" && r.Sockets != nil {
		if err := r.Sockets.Wait(ctx, "hotplug host exec", []string{fs.SocketPath}, proc); err != nil {
			_ = r.Start.Stop(proc)
			return nil, err
		}
	}
	return proc, nil
}

func (r Runner) attachGuest(ctx context.Context, device manifest.HotplugDevice) error {
	if device.Kind != manifest.HotplugKindVirtioFS || device.VirtioFS.Target == "" {
		return nil
	}
	return r.Guest.Run(ctx, []string{"mount", "-t", "virtiofs", device.ID, device.VirtioFS.Target})
}

func (r Runner) detachGuest(ctx context.Context, device manifest.HotplugDevice) error {
	if device.Kind != manifest.HotplugKindVirtioFS || device.VirtioFS.Target == "" {
		return nil
	}
	return r.Guest.Run(ctx, []string{"umount", device.VirtioFS.Target})
}

func (r Runner) rollbackHost(proc *executor.Process) {
	if proc != nil && r.Start != nil {
		_ = r.Start.Stop(proc)
	}
}

// devicePlan spells out the QMP commands for one device kind. Every sequence
// is written out per kind; nothing is derived from the attach commands.
type devicePlan struct {
	// backend commands run in order before deviceAdd.
	backend []string
	// deviceAdd makes the device visible to the guest.
	deviceAdd string
	// release tears down this device's backends. It runs both to roll back a
	// partial attach and, during detach, after device_del has been confirmed.
	// Those are one sequence rather than two because QEMU is in the same
	// state at both points: no guest-visible device, backends still present.
	// A frontend outlives nothing here - device_del never releases a chardev,
	// netdev, or blockdev node, so the backends always need explicit removal
	// whether or not a frontend ever existed.
	release []string
}

func planFor(device manifest.HotplugDevice, bus string) devicePlan {
	switch device.Kind {
	case manifest.HotplugKindVirtioFS:
		return virtioFSPlan(device, bus)
	case manifest.HotplugKindNet:
		return netPlan(device, bus)
	case manifest.HotplugKindBlock:
		return blockPlan(device, bus)
	default:
		return devicePlan{}
	}
}

func virtioFSPlan(device manifest.HotplugDevice, bus string) devicePlan {
	id := device.ID
	chardevAdd := qmpCommand("chardev-add", map[string]any{
		"id": charID(id),
		"backend": map[string]any{
			"type": "socket",
			"data": map[string]any{
				"addr":   map[string]any{"type": "unix", "data": map[string]any{"path": device.VirtioFS.SocketPath}},
				"server": false,
			},
		},
	})
	chardevRemove := qmpCommand("chardev-remove", map[string]any{"id": charID(id)})
	return devicePlan{
		backend: []string{chardevAdd},
		deviceAdd: qmpCommand("device_add", map[string]any{
			"driver":  "vhost-user-fs-pci",
			"id":      qemuDeviceID(id),
			"chardev": charID(id),
			"tag":     id,
			"bus":     bus,
		}),
		release: []string{chardevRemove},
	}
}

// netPlan only attaches the QEMU side. Full networking support also needs
// guest-side link naming, DHCP or static address policy, and route setup.
func netPlan(device manifest.HotplugDevice, bus string) devicePlan {
	id := device.ID
	netdev := map[string]any{"id": netdevID(id), "type": "user"}
	if len(device.Net.Forward) > 0 {
		hostfwd := make([]string, 0, len(device.Net.Forward))
		for _, forward := range device.Net.Forward {
			hostfwd = append(hostfwd, fmt.Sprintf("%s:%s-%s", forward.Proto, forward.Host, forward.Guest))
		}
		netdev["hostfwd"] = hostfwd
	}
	netdevDel := qmpCommand("netdev_del", map[string]any{"id": netdevID(id)})
	return devicePlan{
		backend: []string{qmpCommand("netdev_add", netdev)},
		deviceAdd: qmpCommand("device_add", map[string]any{
			"driver": "virtio-net-pci",
			"id":     qemuDeviceID(id),
			"netdev": netdevID(id),
			"mac":    device.Net.MAC,
			"bus":    bus,
		}),
		release: []string{netdevDel},
	}
}

// blockPlan only attaches the QEMU block device. Full storage support also
// needs guest-side discovery, partition/filesystem policy, and mount setup.
func blockPlan(device manifest.HotplugDevice, bus string) devicePlan {
	id := device.ID
	blockdev := map[string]any{
		"node-name": blockNodeID(id),
		"driver":    device.Block.Format,
		"file": map[string]any{
			"driver":   "file",
			"filename": device.Block.ImagePath,
		},
		"read-only": device.Block.ReadOnly,
	}
	deviceAdd := map[string]any{
		"driver": "virtio-blk-pci",
		"id":     qemuDeviceID(id),
		"drive":  blockNodeID(id),
		"bus":    bus,
	}
	if device.Block.Serial != "" {
		deviceAdd["serial"] = device.Block.Serial
	}
	blockdevDel := qmpCommand("blockdev-del", map[string]any{"node-name": blockNodeID(id)})
	return devicePlan{
		backend:   []string{qmpCommand("blockdev-add", blockdev)},
		deviceAdd: qmpCommand("device_add", deviceAdd),
		release:   []string{blockdevDel},
	}
}

func qemuDeviceID(id string) string { return "dev-" + id }
func charID(id string) string       { return "char-" + id }
func netdevID(id string) string     { return "netdev-" + id }
func blockNodeID(id string) string  { return "block-" + id }

func qmpCommand(execute string, arguments map[string]any) string {
	payload := map[string]any{"execute": execute, "arguments": arguments}
	data, _ := json.Marshal(payload)
	return string(data)
}
