package hotplug

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"syscall"

	"github.com/shazow/virtle/internal/executor"
	"github.com/shazow/virtle/internal/manifest"
)

type hotplugDevice interface {
	Attach(ctx context.Context) error
	Detach(ctx context.Context) error
}

type Runner struct {
	StateDir string
	WorkDir  string
	Devices  []manifest.HotplugDevice
	Start    ProcessStarter
	Sockets  SocketWaiter
	QMP      DeviceQMP
	Guest    GuestRunner
}

type ProcessStarter interface {
	Start(ctx context.Context, cmd *exec.Cmd) (*executor.Process, error)
	Stop(process *executor.Process) error
	SignalPIDGroup(pid int, signal syscall.Signal) error
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
	hotplug, err := r.find(id)
	if err != nil {
		return err
	}
	return hotplug.Attach(ctx)
}

func (r Runner) Detach(ctx context.Context, id string) error {
	hotplug, err := r.find(id)
	if err != nil {
		return err
	}
	return hotplug.Detach(ctx)
}

// BusName is the PCIe root port a hotplug device with the given manifest
// index attaches to. It must match the pcie-root-port IDs created at QEMU
// boot (see buildQEMUArgs in backend/qemu/internal/vmm).
func BusName(index int) string {
	return fmt.Sprintf("pcie.hotplug.%d", index)
}

// find resolves only the requested device, so unrelated malformed manifest
// entries do not block an attach or detach of a valid one.
func (r Runner) find(id string) (hotplugDevice, error) {
	found := -1
	for i, device := range r.Devices {
		if device.ID != id {
			continue
		}
		if found >= 0 {
			return nil, fmt.Errorf("manifest.hotplug id %q is duplicated", id)
		}
		found = i
	}
	if found < 0 {
		return nil, fmt.Errorf("manifest.hotplug id %q not found", id)
	}
	device := r.Devices[found]
	base := hotplugBase{
		runner: &r,
		id:     device.ID,
		kind:   device.Kind,
		bus:    BusName(found),
	}
	switch device.Kind {
	case manifest.HotplugKindVirtioFS:
		return &hotplugVirtioFS{hotplugBase: base, fs: device.VirtioFS}, nil
	case manifest.HotplugKindNet:
		return &hotplugNet{hotplugBase: base, net: device.Net}, nil
	case manifest.HotplugKindBlock:
		return &hotplugBlock{hotplugBase: base, block: device.Block}, nil
	default:
		return nil, fmt.Errorf("manifest.hotplug id %q has unsupported kind %q", device.ID, device.Kind)
	}
}

type hotplugBase struct {
	runner *Runner
	id     string
	kind   manifest.HotplugKind
	bus    string
}

func (h hotplugBase) attach(ctx context.Context, device manifest.HotplugDevice, attachHost func(context.Context) (*executor.Process, error), detachHost func(*executor.Process)) error {
	if detachHost == nil {
		detachHost = func(*executor.Process) {}
	}
	statePath, err := h.statePathForAttach()
	if err != nil {
		return err
	}

	var proc *executor.Process
	if attachHost != nil {
		proc, err = attachHost(ctx)
		if err != nil {
			return err
		}
	}

	state := State{ID: h.id, Kind: h.kind, Bus: h.bus}
	if proc != nil {
		state.PID = proc.PID()
	}
	rollbackQMP, err := h.runner.QMP.AttachDevice(ctx, device, h.bus)
	if err != nil {
		detachHost(proc)
		return err
	}
	if rollbackQMP == nil {
		rollbackQMP = func(context.Context) {}
	}
	if err := h.runner.attachGuest(ctx, device); err != nil {
		rollbackQMP(ctx)
		detachHost(proc)
		return err
	}
	if err := WriteState(statePath, state); err != nil {
		_ = h.runner.detachGuest(ctx, device)
		rollbackQMP(ctx)
		detachHost(proc)
		return err
	}
	return nil
}

// statePathForAttach returns the state path for this hotplug after confirming
// no state file exists there yet.
func (h hotplugBase) statePathForAttach() (string, error) {
	statePath, err := StatePath(h.runner.StateDir, h.id)
	if err != nil {
		return "", err
	}
	if _, err := os.Stat(statePath); err == nil {
		return "", fmt.Errorf("hotplug %q is already attached; state exists at %q", h.id, statePath)
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", fmt.Errorf("stat hotplug state %q: %w", statePath, err)
	}
	return statePath, nil
}

// attachedState loads and validates the state file for this hotplug.
func (h hotplugBase) attachedState() (string, State, error) {
	statePath, err := StatePath(h.runner.StateDir, h.id)
	if err != nil {
		return "", State{}, err
	}
	state, err := ReadState(statePath)
	if err != nil {
		return "", State{}, err
	}
	if state.ID != h.id {
		return "", State{}, fmt.Errorf("hotplug state %q belongs to %q, not %q", statePath, state.ID, h.id)
	}
	if state.Kind != h.kind {
		return "", State{}, fmt.Errorf("hotplug state %q is kind %q, not current manifest kind %q", statePath, state.Kind, h.kind)
	}
	return statePath, state, nil
}

func (h hotplugBase) detach(ctx context.Context, device manifest.HotplugDevice, cleanup func(State) error) error {
	statePath, state, err := h.attachedState()
	if err != nil {
		return err
	}

	guestUnmounted := device.Kind == manifest.HotplugKindVirtioFS && device.VirtioFS.Target != ""
	if err := h.runner.detachGuest(ctx, device); err != nil {
		return err
	}
	cleanupCtx := ctx
	if guestUnmounted {
		cleanupCtx = context.WithoutCancel(ctx)
	}
	if err := h.runner.QMP.DetachDevice(cleanupCtx, device); err != nil {
		return err
	}
	if cleanup != nil {
		if err := cleanup(state); err != nil {
			return err
		}
	}
	if err := os.Remove(statePath); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("remove hotplug state %q: %w", statePath, err)
	}
	return nil
}

type hotplugVirtioFS struct {
	hotplugBase
	fs manifest.HotplugVirtioFS
}

func (h hotplugVirtioFS) Attach(ctx context.Context) error {
	return h.attach(ctx, h.device(), h.attachHost, h.detachHost)
}

func (h hotplugVirtioFS) Detach(ctx context.Context) error {
	return h.detach(ctx, h.device(), func(state State) error {
		if state.PID > 0 {
			if err := h.runner.terminatePID(state.PID); err != nil {
				return err
			}
		}
		if h.fs.SocketPath != "" {
			_ = os.Remove(h.fs.SocketPath)
		}
		return nil
	})
}

func (h hotplugVirtioFS) device() manifest.HotplugDevice {
	return manifest.HotplugDevice{Kind: manifest.HotplugKindVirtioFS, ID: h.id, VirtioFS: h.fs}
}

func (h hotplugVirtioFS) attachHost(ctx context.Context) (*executor.Process, error) {
	return h.runner.attachVirtioFSHost(ctx, h.device())
}

func (h hotplugVirtioFS) detachHost(proc *executor.Process) {
	h.runner.rollbackHost(proc)
}

type hotplugNet struct {
	hotplugBase
	net manifest.HotplugNet
}

func (h hotplugNet) Attach(ctx context.Context) error {
	return h.attach(ctx, h.device(), nil, nil)
}

func (h hotplugNet) Detach(ctx context.Context) error {
	return h.detach(ctx, h.device(), nil)
}

func (h hotplugNet) device() manifest.HotplugDevice {
	return manifest.HotplugDevice{Kind: manifest.HotplugKindNet, ID: h.id, Net: h.net}
}

type hotplugBlock struct {
	hotplugBase
	block manifest.HotplugBlock
}

func (h hotplugBlock) Attach(ctx context.Context) error {
	return h.attach(ctx, h.device(), nil, nil)
}

func (h hotplugBlock) Detach(ctx context.Context) error {
	return h.detach(ctx, h.device(), nil)
}

func (h hotplugBlock) device() manifest.HotplugDevice {
	return manifest.HotplugDevice{Kind: manifest.HotplugKindBlock, ID: h.id, Block: h.block}
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

func (r Runner) terminatePID(pid int) error {
	if r.Start != nil {
		return r.Start.SignalPIDGroup(pid, syscall.SIGTERM)
	}
	return executor.SignalProcessGroup(pid, syscall.SIGTERM)
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
