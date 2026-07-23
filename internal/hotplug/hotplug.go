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
)

type hotplugDevice interface {
	Attach() error
	Detach() error
	ID() string
}

type Runner struct {
	StateDir string
	WorkDir  string
	Devices  []Device
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
	AttachDevice(context.Context, Device, string) (func(context.Context), error)
	DetachDevice(context.Context, Device) error
}

type GuestRunner interface {
	Run(ctx context.Context, command []string) error
}

func (r Runner) Attach(ctx context.Context, id string) error {
	registry, err := r.hotplugRegistry(ctx)
	if err != nil {
		return err
	}
	hotplug, err := registry.lookup(id)
	if err != nil {
		return err
	}
	return hotplug.Attach()
}

func (r Runner) Detach(ctx context.Context, id string) error {
	registry, err := r.hotplugRegistry(ctx)
	if err != nil {
		return err
	}
	hotplug, err := registry.lookup(id)
	if err != nil {
		return err
	}
	return hotplug.Detach()
}

type hotplugRegistry map[string]hotplugDevice

func (r Runner) hotplugRegistry(ctx context.Context) (hotplugRegistry, error) {
	registry := make(hotplugRegistry, len(r.Devices))
	for i, device := range r.Devices {
		hotplug, err := r.hotplug(ctx, device, i)
		if err != nil {
			return nil, err
		}
		if err := registry.add(hotplug); err != nil {
			return nil, err
		}
	}
	return registry, nil
}

func (r Runner) hotplug(ctx context.Context, device Device, index int) (hotplugDevice, error) {
	base := hotplugBase{
		ctx:    ctx,
		runner: &r,
		id:     device.ID,
		kind:   device.Kind,
		bus:    fmt.Sprintf("pcie.hotplug.%d", index),
	}
	switch device.Kind {
	case KindVirtioFS:
		return &hotplugVirtioFS{hotplugBase: base, VirtioFS: device.VirtioFS}, nil
	case KindNet:
		return &hotplugNet{hotplugBase: base, Net: device.Net}, nil
	case KindBlock:
		return &hotplugBlock{hotplugBase: base, Block: device.Block}, nil
	default:
		return nil, fmt.Errorf("manifest.hotplug id %q has unsupported kind %q", device.ID, device.Kind)
	}
}

func (r hotplugRegistry) add(hotplug hotplugDevice) error {
	id := hotplug.ID()
	if _, ok := r[id]; ok {
		return fmt.Errorf("manifest.hotplug id %q is duplicated", id)
	}
	r[id] = hotplug
	return nil
}

func (r hotplugRegistry) lookup(id string) (hotplugDevice, error) {
	hotplug, ok := r[id]
	if !ok {
		return nil, fmt.Errorf("manifest.hotplug id %q not found", id)
	}
	return hotplug, nil
}

type hotplugBase struct {
	ctx    context.Context
	runner *Runner
	id     string
	kind   Kind
	bus    string
}

func (h hotplugBase) ID() string {
	return h.id
}

func (h hotplugBase) attach(device Device, attachHost func() (*executor.Process, error), detachHost func(*executor.Process)) error {
	if detachHost == nil {
		detachHost = func(*executor.Process) {}
	}
	statePath, err := StatePath(h.runner.StateDir, h.id)
	if err != nil {
		return err
	}
	if _, err := os.Stat(statePath); err == nil {
		return fmt.Errorf("hotplug %q is already attached; state exists at %q", h.id, statePath)
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("stat hotplug state %q: %w", statePath, err)
	}

	var proc *executor.Process
	if attachHost != nil {
		proc, err = attachHost()
		if err != nil {
			return err
		}
	}

	state := State{ID: h.id, Kind: h.kind, Bus: h.bus}
	if proc != nil {
		state.PID = proc.PID()
	}
	rollbackQMP, err := h.runner.QMP.AttachDevice(h.ctx, device, h.bus)
	if err != nil {
		detachHost(proc)
		return err
	}
	if rollbackQMP == nil {
		rollbackQMP = func(context.Context) {}
	}
	if err := h.runner.attachGuest(h.ctx, device); err != nil {
		rollbackQMP(h.ctx)
		detachHost(proc)
		return err
	}
	if err := WriteState(statePath, state); err != nil {
		_ = h.runner.detachGuest(h.ctx, device)
		rollbackQMP(h.ctx)
		detachHost(proc)
		return err
	}
	return nil
}

func (h hotplugBase) detach(device Device, cleanup func(State) error) error {
	statePath, err := StatePath(h.runner.StateDir, h.id)
	if err != nil {
		return err
	}
	state, err := ReadState(statePath)
	if err != nil {
		return err
	}
	if state.ID != h.id {
		return fmt.Errorf("hotplug state %q belongs to %q, not %q", statePath, state.ID, h.id)
	}
	if state.Kind != h.kind {
		return fmt.Errorf("hotplug state %q is kind %q, not current manifest kind %q", statePath, state.Kind, h.kind)
	}

	guestUnmounted := device.Kind == KindVirtioFS && device.VirtioFS.Target != ""
	if err := h.runner.detachGuest(h.ctx, device); err != nil {
		return err
	}
	cleanupCtx := h.ctx
	if guestUnmounted {
		cleanupCtx = context.WithoutCancel(h.ctx)
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
	VirtioFS
}

func (h hotplugVirtioFS) Attach() error {
	return h.attach(h.device(), h.attachHost, h.detachHost)
}

func (h hotplugVirtioFS) Detach() error {
	return h.detach(h.device(), func(state State) error {
		if state.PID > 0 {
			if err := h.runner.terminatePID(state.PID); err != nil {
				return err
			}
		}
		if h.SocketPath != "" {
			_ = os.Remove(h.SocketPath)
		}
		return nil
	})
}

func (h hotplugVirtioFS) device() Device {
	return Device{Kind: KindVirtioFS, ID: h.id, VirtioFS: h.VirtioFS}
}

func (h hotplugVirtioFS) attachHost() (*executor.Process, error) {
	return h.runner.attachVirtioFSHost(h.ctx, h.device())
}

func (h hotplugVirtioFS) detachHost(proc *executor.Process) {
	h.runner.rollbackHost(proc)
}

type hotplugNet struct {
	hotplugBase
	Net
}

func (h hotplugNet) Attach() error {
	return h.attach(h.device(), nil, nil)
}

func (h hotplugNet) Detach() error {
	return h.detach(h.device(), nil)
}

func (h hotplugNet) device() Device {
	return Device{Kind: KindNet, ID: h.id, Net: h.Net}
}

type hotplugBlock struct {
	hotplugBase
	Block
}

func (h hotplugBlock) Attach() error {
	return h.attach(h.device(), nil, nil)
}

func (h hotplugBlock) Detach() error {
	return h.detach(h.device(), nil)
}

func (h hotplugBlock) device() Device {
	return Device{Kind: KindBlock, ID: h.id, Block: h.Block}
}

func (r Runner) attachVirtioFSHost(ctx context.Context, device Device) (*executor.Process, error) {
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

func (r Runner) attachGuest(ctx context.Context, device Device) error {
	if device.Kind != KindVirtioFS || device.VirtioFS.Target == "" {
		return nil
	}
	return r.Guest.Run(ctx, []string{"mount", "-t", "virtiofs", device.ID, device.VirtioFS.Target})
}

func (r Runner) detachGuest(ctx context.Context, device Device) error {
	if device.Kind != KindVirtioFS || device.VirtioFS.Target == "" {
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

func attachCommands(device Device, bus string) []string {
	switch device.Kind {
	case KindVirtioFS:
		return virtioFSAttachCommands(device, bus)
	case KindNet:
		return netAttachCommands(device, bus)
	case KindBlock:
		return blockAttachCommands(device, bus)
	default:
		return nil
	}
}

func detachPostDeviceDelCommands(device Device) []string {
	switch device.Kind {
	case KindVirtioFS:
		return []string{qmpCommand("chardev-remove", map[string]any{"id": charID(device.ID)})}
	case KindNet:
		return []string{qmpCommand("netdev_del", map[string]any{"id": netdevID(device.ID)})}
	case KindBlock:
		return []string{qmpCommand("blockdev-del", map[string]any{"node-name": blockNodeID(device.ID)})}
	default:
		return nil
	}
}

func rollbackAttachCommands(device Device, successful int) []string {
	if successful < 1 {
		return nil
	}
	switch device.Kind {
	case KindVirtioFS:
		return []string{qmpCommand("chardev-remove", map[string]any{"id": charID(device.ID)})}
	case KindNet:
		return []string{qmpCommand("netdev_del", map[string]any{"id": netdevID(device.ID)})}
	case KindBlock:
		return []string{qmpCommand("blockdev-del", map[string]any{"node-name": blockNodeID(device.ID)})}
	default:
		return nil
	}
}

func virtioFSAttachCommands(device Device, bus string) []string {
	id := device.ID
	return []string{
		qmpCommand("chardev-add", map[string]any{
			"id": charID(id),
			"backend": map[string]any{
				"type": "socket",
				"data": map[string]any{
					"addr":   map[string]any{"type": "unix", "data": map[string]any{"path": device.VirtioFS.SocketPath}},
					"server": false,
				},
			},
		}),
		qmpCommand("device_add", map[string]any{
			"driver":  "vhost-user-fs-pci",
			"id":      qemuDeviceID(id),
			"chardev": charID(id),
			"tag":     id,
			"bus":     bus,
		}),
	}
}

// netAttachCommands only attaches the QEMU side. Full networking support also
// needs guest-side link naming, DHCP or static address policy, and route setup.
func netAttachCommands(device Device, bus string) []string {
	id := device.ID
	netdev := map[string]any{"id": netdevID(id), "type": "user"}
	if len(device.Net.Forward) > 0 {
		hostfwd := make([]string, 0, len(device.Net.Forward))
		for _, forward := range device.Net.Forward {
			hostfwd = append(hostfwd, fmt.Sprintf("%s:%s-%s", forward.Proto, forward.Host, forward.Guest))
		}
		netdev["hostfwd"] = hostfwd
	}
	return []string{
		qmpCommand("netdev_add", netdev),
		qmpCommand("device_add", map[string]any{
			"driver": "virtio-net-pci",
			"id":     qemuDeviceID(id),
			"netdev": netdevID(id),
			"mac":    device.Net.MAC,
			"bus":    bus,
		}),
	}
}

// blockAttachCommands only attaches the QEMU block device. Full storage support
// also needs guest-side discovery, partition/filesystem policy, and mount setup.
func blockAttachCommands(device Device, bus string) []string {
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
	return []string{
		qmpCommand("blockdev-add", blockdev),
		qmpCommand("device_add", deviceAdd),
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
