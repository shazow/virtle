package manifest

import (
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	shellquote "github.com/kballard/go-shellquote"
	"github.com/shazow/virtle/internal/executor"
)

const virtioFSSocketProbeTimeout = 100 * time.Millisecond

func (d Document) Manifest() (*Manifest, error) {
	return d.ManifestWithOptions(ResolveOptions{})
}

// validateHostName rejects VM names that cannot serve as a file name: the
// name is embedded in the state lock path (<state_dir>/<host_name>.lock)
// that both backends share.
func validateHostName(name string) error {
	if name == "." || name == ".." || strings.ContainsRune(name, filepath.Separator) {
		return fmt.Errorf("manifest.host_name %q must be a plain name without path separators", name)
	}
	return nil
}

func (d Document) ManifestWithOptions(options ResolveOptions) (*Manifest, error) {
	switch d.Backend {
	case "", BackendQEMU:
	case BackendFirecracker:
		return d.firecrackerManifest()
	default:
		return nil, fmt.Errorf("manifest.backend must be %s or %s, got %q", BackendQEMU, BackendFirecracker, d.Backend)
	}
	if d.Firecracker != (FirecrackerInput{}) {
		return nil, fmt.Errorf("manifest.firecracker requires backend = %q", BackendFirecracker)
	}
	d = DocumentWithDefaults(d)
	if err := validateHostName(d.HostName); err != nil {
		return nil, err
	}
	if d.Kernel.Path == "" {
		return nil, fmt.Errorf("manifest.kernel.path is required")
	}
	rootIndex, rootParams, err := rootDevice(d.Mounts.Image())
	if err != nil {
		return nil, err
	}
	if err := validateBootSource(d.Kernel, d.Mounts.Image(), rootIndex); err != nil {
		return nil, err
	}
	host := d.Host.withDefaults()
	m := &Manifest{
		Backend: BackendQEMU,
		Identity: Identity{
			HostName: d.HostName,
		},
		Paths: Paths{
			WorkingDir: d.WorkingDir,
		},
		Persistence: Persistence{
			BaseDir:  d.StateDir,
			StateDir: d.StateDir,
		},
		SSH: SSH{
			Argv:          append([]string(nil), d.SSH.Exec...),
			User:          d.SSH.User,
			RetryDelay:    d.SSH.RetryDelay.Duration(),
			Autoprovision: d.SSH.Autoprovision,
		},
		VSock: VSock{
			CIDRange: VSockCIDRange{
				Start: d.VSock.CIDRange.Min,
				End:   d.VSock.CIDRange.Max,
			},
		},
		Notifications: resolveNotifications(d.Notifications),
		Workspace:     resolveWorkspace(d.Workspace),
	}
	imageMounts := d.Mounts.Image()
	virtioFSMounts := d.Mounts.VirtioFS()
	m.Persistence.Directories = persistenceDirectories(imageMounts, m.Persistence.StateDir)
	m.Paths.LockPath = filepath.Join(m.Persistence.StateDir, m.Identity.HostName+".lock")
	m.Paths.RuntimeDir = RuntimeDir{Mode: RuntimeDirPath, Path: m.Persistence.StateDir}
	if d.QEMU.HotplugPorts < 0 {
		return nil, fmt.Errorf("manifest.qemu.hotplug_ports must not be negative, got %d", d.QEMU.HotplugPorts)
	}
	hotplugCount := d.hotplugCount()
	qemu, err := d.resolveQEMU(host, hotplugCount, rootParams)
	if err != nil {
		return nil, err
	}
	m.QEMU = qemu
	m.Volumes = resolveVolumes(imageMounts)
	virtioFSRuns, err := m.resolveVirtioFSRuns(virtioFSMounts, options)
	if err != nil {
		return nil, err
	}
	m.Run = append(virtioFSRuns, resolveRun(d.Run)...)
	hotplug, err := m.resolveHotplug(d)
	if err != nil {
		return nil, err
	}
	m.Hotplug = hotplug
	m.WriteFiles = resolveWriteFiles(d.WriteFiles)

	if err := m.Validate(); err != nil {
		return nil, err
	}
	return m, nil
}

func (h HostInput) withDefaults() HostInput {
	if h.OS == "" {
		h.OS = runtime.GOOS
	}
	if h.Arch == "" {
		h.Arch = qemuArch(runtime.GOARCH)
	}
	if h.System == "" {
		h.System = h.Arch + "-" + h.OS
	}
	return h
}

func (d Document) resolveQEMU(host HostInput, hotplugCount int, rootParams []string) (QEMU, error) {
	machineType := d.Machine.Type
	graphics := resolveGraphics(d.Graphics)
	transport := qemuTransport(machineType, d.Mounts, graphics, hotplugCount > 0)
	virtioFSMounts := d.Mounts.VirtioFS()
	hasVirtioFS := len(virtioFSMounts) > 0 || len(d.Hotplug.VirtioFS()) > 0
	memorySize := d.Machine.Memory
	cpuModel := d.Machine.CPU
	if cpuModel == "" {
		cpuModel = defaultCPUModel(host)
	}
	enableKVM := host.OS == "linux" && d.Machine.CPU == ""
	if d.Machine.KVM != nil {
		enableKVM = *d.Machine.KVM
	}
	qemuRenderer, err := NewTemplateRenderer(QEMUTemplateProvider{
		HostName:   d.HostName,
		WorkingDir: d.WorkingDir,
		StateDir:   d.StateDir,
		Host:       host,
	})
	if err != nil {
		return QEMU{}, fmt.Errorf("manifest.qemu.exec: %w", err)
	}
	qemuExec, err := qemuRenderer.RenderArgv(d.QEMU.Exec)
	if err != nil {
		return QEMU{}, fmt.Errorf("manifest.qemu.exec: %w", err)
	}
	binaryPath := ""
	if len(qemuExec) > 0 {
		binaryPath = qemuExec[0]
	}
	if binaryPath == "" {
		binaryPath = "qemu-system-" + host.Arch
	}
	qmpSocket := d.QEMU.QMPSocket
	guestAgentSocket := d.QEMU.GuestAgentSocket
	vsockID := "vsock0"
	if d.VSock.Enabled != nil && !*d.VSock.Enabled {
		vsockID = ""
	}
	sshReadySocket := d.SSH.ReadySocket
	noGraphic := graphics.IsZero()
	cpus := resolveCPUCount(d.Machine.VCPU)
	networks, err := resolveNetwork(d.Networks, d.QEMU.FwdTunnelExec, host, transport, cpus)
	if err != nil {
		return QEMU{}, err
	}
	serialMode, err := kernelSerialMode(d.Kernel)
	if err != nil {
		return QEMU{}, err
	}

	ninePMounts := d.Mounts.NineP()
	imageMounts := d.Mounts.Image()
	qemu := QEMU{
		BinaryPath: binaryPath,
		RunAsUser:  d.QEMU.User,
		Name:       d.HostName,
		Machine: QEMUMachine{
			Type:    machineType,
			Options: resolveMachineOptions(host, machineType, d.QEMU.MachineOptions, transport == "pci", d.Machine.KVM),
		},
		CPU: QEMUCPU{
			Model:     cpuModel,
			EnableKVM: enableKVM,
		},
		Memory: QEMUMemory{
			Size:    memorySize,
			Backend: memoryBackend(host, hasVirtioFS),
			Shared:  hasVirtioFS,
		},
		Kernel: QEMUKernel{
			Path:       d.Kernel.Path,
			InitrdPath: d.Kernel.InitrdPath,
			Params:     kernelParams(host, serialMode, rootParams, d.Kernel.Params),
		},
		SMP: QEMUSMP{
			CPUs: cpus,
		},
		Console: QEMUConsole(serialMode),
		Knobs: QEMUKnobs{
			NoDefaults:     true,
			NoUserConfig:   true,
			NoReboot:       true,
			NoGraphic:      noGraphic,
			SeccompSandbox: d.QEMU.Seccomp,
		},
		Graphics: graphics,
		QMP: QEMUQMP{
			SocketPath: qmpSocket,
		},
		GuestAgent: QEMUGuestAgent{
			SocketPath:      guestAgentSocket,
			CommandTimeout:  d.QEMU.GuestDefaultTimeout.Duration(),
			ShutdownExec:    d.QEMU.ShutdownExec,
			ShutdownTimeout: d.QEMU.ShutdownTimeout.Duration(),
		},
		SSHReady: QEMUSSHReady{
			SocketPath: sshReadySocket,
		},
		Hotplug: QEMUHotplug{
			PCIEPorts: hotplugCount,
		},
		Devices: QEMUDevices{
			RNG: QEMURNGDevice{
				ID:        "rng0",
				Transport: transport,
			},
			I8042:    host.System == "x86_64-linux",
			Balloon:  resolveBalloon(d.Balloon, transport),
			VirtioFS: resolveVirtioFSMounts(virtioFSMounts, transport),
			NineP:    resolveNinePMounts(ninePMounts, transport),
			Block:    resolveBlocks(imageMounts, host, transport),
			Mounts:   resolveQEMUMounts(d.Mounts, host, transport),
			Network:  networks,
			VSOCK: QEMUVSOCKDevice{
				ID:        vsockID,
				Transport: transport,
			},
		},
		MachineID:       d.Machine.ID,
		PassthroughArgs: qemuPassthroughArgs(d.QEMU, qemuExec),
	}
	return qemu, nil
}

func resolveCPUCount(cpus int) CPUCount {
	if cpus == 0 {
		return CPUCount{}
	}
	return ExplicitCPUs(cpus)
}

// hotplugCount returns the number of PCIe hotplug root ports to reserve:
// the listed hotplug devices, or qemu.hotplug_ports when it reserves more
// (extra ports allow attaching devices the manifest does not describe).
// Port-based reservation is QEMU-specific, hence the [qemu] table.
func (d Document) hotplugCount() int {
	return max(d.Hotplug.Len(), d.QEMU.HotplugPorts)
}

func qemuTransport(machineType string, mounts MountsInput, graphics QEMUGraphics, requirePCI bool) string {
	if requirePCI || !strings.HasPrefix(machineType, "microvm") || mounts.RequiresPCI() || !graphics.IsZero() {
		return "pci"
	}
	return "mmio"
}

func resolveMachineOptions(host HostInput, machineType string, explicit map[string]string, requirePCI bool, kvm *bool) []string {
	usingDefaults := explicit == nil
	options := explicit
	if options == nil {
		options = defaultMachineOptions(host, machineType, requirePCI)
	} else {
		options = cloneStringMap(explicit)
		if requirePCI && strings.HasPrefix(machineType, "microvm") {
			options["pcie"] = "on"
		}
	}
	if kvm != nil {
		options["accel"] = "tcg"
		if *kvm {
			options["accel"] = "kvm"
		} else if usingDefaults && host.System == "x86_64-linux" && strings.HasPrefix(machineType, "microvm") {
			// TCG has no KVM clock, so retain the legacy timers that the KVM
			// microvm defaults can safely omit.
			options["pic"] = "on"
			options["pit"] = "on"
		}
	}
	keys := make([]string, 0, len(options))
	for key := range options {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	result := make([]string, 0, len(keys))
	for _, key := range keys {
		result = append(result, key+"="+options[key])
	}
	return result
}

func defaultMachineOptions(host HostInput, machineType string, requirePCI bool) map[string]string {
	accel := "tcg"
	switch host.OS {
	case "linux":
		accel = "kvm:tcg"
	case "darwin":
		accel = "hvf:tcg"
	}
	switch host.System {
	case "x86_64-linux":
		options := map[string]string{
			"accel":     accel,
			"mem-merge": "on",
			"acpi":      "on",
		}
		if machineType == "microvm" {
			options["pit"] = "off"
			options["pic"] = "off"
			options["pcie"] = OnOff(requirePCI)
			options["rtc"] = "on"
			options["usb"] = "off"
		}
		return options
	case "aarch64-linux":
		return map[string]string{"accel": accel, "gic-version": "max"}
	case "aarch64-darwin":
		return map[string]string{"accel": accel}
	default:
		return map[string]string{"accel": accel}
	}
}

func defaultCPUModel(host HostInput) string {
	if host.System == "x86_64-linux" {
		return "host,+x2apic,-sgx"
	}
	return "host"
}

func memoryBackend(host HostInput, hasVirtioFS bool) string {
	if host.OS == "linux" && hasVirtioFS {
		return "memfd"
	}
	return "default"
}

// kernelParams assembles the guest command line: console parameters for the
// serial mode, virtle's fixed reboot/panic policy, the root device (see
// rootDevice), then the manifest's own parameters, which therefore win.
func kernelParams(host HostInput, serialMode string, root []string, extra []string) string {
	params := make([]string, 0, len(extra)+len(root)+3)
	if serialMode != KernelSerialOff {
		switch host.System {
		case "x86_64-linux":
			params = append(params, "earlyprintk=ttyS0 console=ttyS0")
		case "aarch64-linux":
			params = append(params, "console=ttyAMA0")
		}
	}
	params = append(params, "reboot=t", "panic=-1")
	params = append(params, root...)
	params = append(params, extra...)
	return strings.Join(params, " ")
}

// rootDevice picks the image mount the guest boots from: the one whose
// target is "/". virtle passes the kernel's root= for it on every backend
// (virtio-blk devices enumerate as /dev/vda, /dev/vdb, ... in mount order),
// so one Spec boots the same way under QEMU and Firecracker. It returns the
// mount's index, or -1, and the kernel parameters that select it. Errors
// name image mounts by their position among the images
// (manifest.mounts.image[i]), as the volume validation does.
func rootDevice(mounts []ImageMountInput) (int, []string, error) {
	root := -1
	for i, mount := range mounts {
		switch mount.Target {
		case "":
		case "/":
			if root >= 0 {
				return 0, nil, fmt.Errorf("manifest.mounts.image[%d].target: mounts.image[%d] is already the root device", i, root)
			}
			root = i
		default:
			return 0, nil, fmt.Errorf("manifest.mounts.image[%d].target %q: only \"/\" (the root device) is supported for images at boot", i, mount.Target)
		}
	}
	if root < 0 {
		return -1, nil, nil
	}
	if root >= 26 {
		return 0, nil, fmt.Errorf("manifest.mounts.image[%d].target: the root device must be among the first 26 images", root)
	}
	access := "rw"
	if mounts[root].ReadOnly {
		access = "ro"
	}
	return root, []string{fmt.Sprintf("root=/dev/vd%c", 'a'+root), access}, nil
}

// validateBootSource rejects a boot that can only end in a kernel panic:
// disks but no initrd, and neither a root device nor a root= parameter.
func validateBootSource(kernel KernelInput, mounts []ImageMountInput, rootIndex int) error {
	if kernel.InitrdPath != "" || len(mounts) == 0 || rootIndex >= 0 || hasKernelParam(kernel.Params, "root=") {
		return nil
	}
	return fmt.Errorf("manifest.kernel.initrd_path is empty and no disk is the root device: set target = \"/\" on the root image mount (vm.Disk.GuestPath) or pass root= in kernel.params")
}

// hasKernelParam reports whether any parameter starts with prefix; a single
// entry may carry several space-separated parameters.
func hasKernelParam(params []string, prefix string) bool {
	for _, param := range params {
		for _, field := range strings.Fields(param) {
			if strings.HasPrefix(field, prefix) {
				return true
			}
		}
	}
	return false
}

func kernelSerialMode(kernel KernelInput) (string, error) {
	switch kernel.Serial {
	case "":
		return KernelSerialOff, nil
	case KernelSerialOff, KernelSerialPrint, KernelSerialConsole:
		return kernel.Serial, nil
	default:
		return "", fmt.Errorf("manifest.kernel.serial must be one of off, print, or console")
	}
}

func resolveVolumes(volumes []ImageMountInput) []Volume {
	result := make([]Volume, 0, len(volumes))
	for _, volume := range volumes {
		result = append(result, Volume{
			ImagePath:  volume.SourcePath,
			Size:       volume.Image.Size,
			FSType:     volume.Image.FSType,
			AutoCreate: volume.Image.AutoCreate,
			Label:      stringValue(volume.Image.Label),
		})
	}
	return result
}

// resolveBlocks, resolveVirtioFSMounts, and resolveNinePMounts populate the
// per-kind device slices with the same devices Devices.Mounts carries, so
// they reuse the single-mount resolvers.
func resolveBlocks(volumes []ImageMountInput, host HostInput, transport string) []QEMUBlockDevice {
	blocks := make([]QEMUBlockDevice, 0, len(volumes))
	for i, volume := range volumes {
		blocks = append(blocks, *resolveQEMUImageMount(volume, i, host, transport).Block)
	}
	return blocks
}

func resolveQEMUMounts(mounts MountsInput, host HostInput, transport string) []QEMUMountDevice {
	devices := make([]QEMUMountDevice, 0, len(mounts))
	ctx := mountResolveContext{
		host:      host,
		transport: transport,
	}
	for _, mount := range mounts {
		devices = append(devices, mount.resolveQEMUMount(&ctx))
	}
	return devices
}

type mountResolveContext struct {
	host           HostInput
	transport      string
	virtioFSIndex  int
	ninePIndex     int
	blockDiskIndex int
}

func (mount VirtioFSMountInput) resolveQEMUMount(ctx *mountResolveContext) QEMUMountDevice {
	device := resolveQEMUVirtioFSMount(mount, ctx.virtioFSIndex, ctx.transport)
	ctx.virtioFSIndex++
	return device
}

func (mount NinePMountInput) resolveQEMUMount(ctx *mountResolveContext) QEMUMountDevice {
	device := resolveQEMUNinePMount(mount, ctx.ninePIndex, ctx.transport)
	ctx.ninePIndex++
	return device
}

func (mount ImageMountInput) resolveQEMUMount(ctx *mountResolveContext) QEMUMountDevice {
	device := resolveQEMUImageMount(mount, ctx.blockDiskIndex, ctx.host, ctx.transport)
	ctx.blockDiskIndex++
	return device
}

func resolveQEMUVirtioFSMount(mount VirtioFSMountInput, index int, transport string) QEMUMountDevice {
	share := QEMUVirtioFSShare{
		ID:         "fs" + strconv.Itoa(index),
		SocketPath: mount.VirtioFS.Socket,
		Tag:        mount.Tag,
		Transport:  transport,
	}
	return QEMUMountDevice{Type: MountTypeVirtioFS, VirtioFS: &share}
}

func resolveQEMUNinePMount(mount NinePMountInput, index int, transport string) QEMUMountDevice {
	securityModel := mount.NineP.SecurityModel
	if securityModel == "" {
		securityModel = "mapped"
	}
	share := QEMUNinePShare{
		ID:            "fs9p" + strconv.Itoa(index),
		SourcePath:    mount.SourcePath,
		Tag:           mount.Tag,
		SecurityModel: securityModel,
		ReadOnly:      mount.ReadOnly,
		Transport:     transport,
	}
	return QEMUMountDevice{Type: MountTypeNineP, NineP: &share}
}

func resolveQEMUImageMount(mount ImageMountInput, index int, host HostInput, transport string) QEMUMountDevice {
	block := QEMUBlockDevice{
		ID:        "vd" + string(rune('a'+index)),
		ImagePath: mount.SourcePath,
		Format:    resolveImageFormat(mount.Image.Format),
		AIO:       aioEngine(host),
		ReadOnly:  mount.ReadOnly,
		Serial:    stringValue(mount.Image.Serial),
		Transport: transport,
	}
	if mount.Image.Direct {
		block.Cache = "none"
	}
	return QEMUMountDevice{Type: MountTypeImage, Block: &block}
}

func aioEngine(host HostInput) string {
	if host.OS == "linux" {
		return "io_uring"
	}
	return "threads"
}

func resolveWorkspace(workspace WorkspaceInput) Workspace {
	return Workspace(workspace)
}

func resolveVirtioFSMounts(mounts []VirtioFSMountInput, transport string) []QEMUVirtioFSShare {
	shares := make([]QEMUVirtioFSShare, 0, len(mounts))
	for i, mount := range mounts {
		shares = append(shares, *resolveQEMUVirtioFSMount(mount, i, transport).VirtioFS)
	}
	return shares
}

func resolveNinePMounts(mounts []NinePMountInput, transport string) []QEMUNinePShare {
	shares := make([]QEMUNinePShare, 0, len(mounts))
	for i, mount := range mounts {
		shares = append(shares, *resolveQEMUNinePMount(mount, i, transport).NineP)
	}
	return shares
}

func (m *Manifest) resolveVirtioFSRuns(mounts []VirtioFSMountInput, options ResolveOptions) ([]Run, error) {
	runs := make([]Run, 0, len(mounts))
	for _, mount := range mounts {
		if mount.VirtioFS.Socket == "" {
			continue
		}
		if mount.VirtioFS.Bin == "" && len(mount.VirtioFS.Args) == 0 {
			continue
		}
		socketPath, err := m.resolveSocketPath(mount.VirtioFS.Socket)
		if err != nil {
			return nil, err
		}
		if info, err := os.Stat(socketPath); err == nil {
			if info.Mode()&os.ModeSocket == 0 {
				if options.Logger != nil {
					options.Logger.Warn("virtiofs socket path exists but is not a socket (possibly leftover from crash); starting virtiofsd anyway", "socket", socketPath)
				}
			} else {
				stale, err := staleUnixSocket(socketPath)
				if stale {
					if options.Logger != nil {
						options.Logger.Warn("virtiofs socket path exists but appears stale; starting virtiofsd anyway", "socket", socketPath, "error", err)
					}
				} else {
					if err != nil && options.Logger != nil {
						options.Logger.Warn("virtiofs socket liveness probe failed; assuming it is externally managed", "socket", socketPath, "error", err)
					} else if options.Logger != nil {
						options.Logger.Info("using existing virtiofs socket", "socket", socketPath)
					}
					continue
				}
			}
		} else if !os.IsNotExist(err) {
			return nil, fmt.Errorf("stat virtiofs socket %q: %w", socketPath, err)
		}

		args := append([]string(nil), mount.VirtioFS.Args...)
		if len(args) == 0 {
			args = []string{
				"--socket-path={{.Socket}}",
				"--shared-dir={{.MountSource}}",
				"--tag={{.MountTag}}",
			}
		}
		runs = append(runs, Run{
			Exec: append([]string{m.resolveOptionalBin(mount.VirtioFS.Bin, defaultVirtioFSBin)}, args...),
			Env:  []string{"VIRTIOFSD_SOCKET={{.Socket}}"},
			Vars: VirtioFSTemplateProvider{
				SocketPath: socketPath,
				SourcePath: m.resolvePath(mount.SourcePath),
				Tag:        mount.Tag,
			}.TemplateContext(),
		})
		m.addCleanupFile(mount.VirtioFS.Socket)
	}
	return runs, nil
}

func (m *Manifest) resolveHotplug(d Document) ([]HotplugDevice, error) {
	hotplugs := make([]HotplugDevice, 0, d.hotplugCount())
	for i, mount := range d.Hotplug.Mounts {
		device, err := m.resolveHotplugMount(mount)
		if err != nil {
			return nil, fmt.Errorf("manifest.hotplug.mounts[%d]: %w", i, err)
		}
		hotplugs = append(hotplugs, device)
	}
	for i, network := range d.Hotplug.Networks {
		device, err := resolveNetworkHotplug(network, i)
		if err != nil {
			return nil, fmt.Errorf("manifest.hotplug.networks[%d]: %w", i, err)
		}
		hotplugs = append(hotplugs, device)
	}
	return hotplugs, nil
}

func (m *Manifest) resolveHotplugMount(entry MountEntry) (HotplugDevice, error) {
	switch typed := entry.(type) {
	case VirtioFSMountInput:
		return m.resolveVirtioFSHotplug(typed)
	case ImageMountInput:
		return m.resolveImageHotplug(typed)
	default:
		return HotplugDevice{}, fmt.Errorf("type %q does not support hotplug", entry.mountType())
	}
}

// ResolveHotplugMount lowers one ad-hoc mount through the same path used by
// document-declared hotplug mounts, including path, socket, command, and image
// format defaults.
func (m *Manifest) ResolveHotplugMount(entry MountEntry) (HotplugDevice, error) {
	device, err := m.resolveHotplugMount(entry)
	if err != nil {
		return HotplugDevice{}, err
	}
	if err := validateHotplug("hotplug", device); err != nil {
		return HotplugDevice{}, err
	}
	return device, nil
}

func (m *Manifest) resolveImageHotplug(entry ImageMountInput) (HotplugDevice, error) {
	if entry.Target != "" {
		// Only a boot-time image can be the root device; a hotplugged one has
		// no guest mount point without a guest agent.
		return HotplugDevice{}, fmt.Errorf("target %q is not supported for hotplugged images", entry.Target)
	}
	// The image serial doubles as the hotplug id and may itself be a template.
	serial := stringValue(entry.Image.Serial)
	if serial == "" {
		return HotplugDevice{}, fmt.Errorf("id is required")
	}
	format := resolveImageFormat(entry.Image.Format)
	renderer, err := NewTemplateRenderer(StaticTemplateContext(executor.Context{
		"Serial": serial,
		"Source": entry.SourcePath,
		"Format": format,
	}))
	if err != nil {
		return HotplugDevice{}, err
	}
	id, err := renderer.RenderString(serial)
	if err != nil {
		return HotplugDevice{}, err
	}
	if id == "" {
		return HotplugDevice{}, fmt.Errorf("id is required")
	}
	return HotplugDevice{
		Kind: HotplugKindBlock,
		ID:   id,
		Block: HotplugBlock{
			ImagePath: m.resolvePath(entry.SourcePath),
			Format:    format,
			ReadOnly:  entry.ReadOnly,
			Serial:    serial,
		},
	}, nil
}

func (m *Manifest) resolveVirtioFSHotplug(mount VirtioFSMountInput) (HotplugDevice, error) {
	id := mount.Tag
	socket := mount.VirtioFS.Socket
	if socket == "" {
		socket = id + ".sock"
	}
	socketPath, err := m.resolveSocketPath(socket)
	if err != nil {
		return HotplugDevice{}, err
	}
	source := m.resolvePath(mount.SourcePath)
	args := append([]string(nil), mount.VirtioFS.Args...)
	if len(args) == 0 {
		args = DefaultVirtioFSArgs(socketPath, source, id)
	} else {
		renderedArgs, err := renderVirtioFSArgv(args, socketPath, source, id)
		if err != nil {
			return HotplugDevice{}, err
		}
		args = renderedArgs
	}
	return HotplugDevice{
		Kind: HotplugKindVirtioFS,
		ID:   id,
		VirtioFS: HotplugVirtioFS{
			Source:     source,
			Target:     mount.Target,
			SocketPath: socketPath,
			Bin:        m.resolveOptionalBin(mount.VirtioFS.Bin, defaultVirtioFSBin),
			Args:       args,
		},
	}, nil
}

// defaultVirtioFSBin is the virtiofsd binary used when a mount names none;
// it is left unresolved so the host PATH supplies it.
const defaultVirtioFSBin = "virtiofsd"

func (m *Manifest) resolveOptionalBin(bin string, defaultBin string) string {
	if bin == "" || bin == defaultBin {
		return defaultBin
	}
	return m.resolvePath(bin)
}

func resolveNetworkHotplug(entry NetworkInput, index int) (HotplugDevice, error) {
	id := entry.ID
	if id == "" {
		id = fmt.Sprintf("net%d", index)
	}
	backend := entry.Type
	if backend == "" {
		backend = "user"
	}
	mac := entry.MAC
	if mac == "" {
		mac = defaultNetworkMAC
	}
	forward := make([]HotplugForward, 0, len(entry.Forward))
	for i, fwd := range entry.Forward {
		normalized, err := normalizeForwardPort(fwd, fmt.Sprintf("forward[%d]", i))
		if err != nil {
			return HotplugDevice{}, err
		}
		if normalized.From == "guest" {
			return HotplugDevice{}, fmt.Errorf("forward[%d].from guest is not supported for hotplug networks", i)
		}
		forward = append(forward, HotplugForward{
			Proto: normalized.Proto,
			Host:  formatPortEndpoint(normalized.Host),
			Guest: formatPortEndpoint(normalized.Guest),
		})
	}
	return HotplugDevice{
		Kind: HotplugKindNet,
		ID:   id,
		Net: HotplugNet{
			Backend: backend,
			MAC:     mac,
			Forward: forward,
		},
	}, nil
}

// ResolveHotplugNetwork lowers one ad-hoc network through the same path used
// by document-declared hotplug networks, including forward normalization and
// backend defaults.
func (m *Manifest) ResolveHotplugNetwork(entry NetworkInput) (HotplugDevice, error) {
	device, err := resolveNetworkHotplug(entry, 0)
	if err != nil {
		return HotplugDevice{}, err
	}
	if err := validateHotplug("hotplug", device); err != nil {
		return HotplugDevice{}, err
	}
	return device, nil
}

func staleUnixSocket(path string) (bool, error) {
	conn, err := net.DialTimeout("unix", path, virtioFSSocketProbeTimeout)
	if err == nil {
		_ = conn.Close()
		return false, nil
	}
	if errors.Is(err, syscall.ECONNREFUSED) || errors.Is(err, syscall.ENOENT) {
		return true, err
	}
	return false, err
}

func (m *Manifest) addCleanupFile(path string) {
	if path == "" {
		return
	}
	for _, existing := range m.CleanupFiles {
		if existing == path {
			return
		}
	}
	m.CleanupFiles = append(m.CleanupFiles, path)
}

func resolveNetwork(networks []NetworkInput, fwdTunnelExec []string, host HostInput, transport string, cpus CPUCount) ([]QEMUNetDevice, error) {
	devices := make([]QEMUNetDevice, 0, len(networks))
	managed := 0
	for i, network := range networks {
		id := network.ID
		if id == "" {
			id = defaultNetworkID
		}
		netType := network.Type
		if netType == "" {
			netType = defaultNetworkType
		}
		mac := network.MAC
		if mac == "" {
			mac = defaultNetworkMAC
		}
		disableROM := false
		if transport == "pci" || (host.System != "x86_64-linux") {
			disableROM = true
		}
		mqVectors := 0
		if cpus.Set && cpus.Value > 1 && transport == "pci" {
			mqVectors = 2*cpus.Value + 2
		}
		if network.Tap != "" && netType != NetworkTypeTAP {
			return nil, fmt.Errorf("manifest.networks[%d].tap applies to type tap only", i)
		}
		device := QEMUNetDevice{
			ID:         id,
			MacAddress: mac,
			Transport:  transport,
			DisableROM: disableROM,
			MQVectors:  mqVectors,
		}
		switch netType {
		case NetworkTypeUser:
			device.Backend = "user"
			forwardOptions, err := resolveForwardPorts(network.Forward, fwdTunnelExec, i)
			if err != nil {
				return nil, err
			}
			device.NetdevOptions = forwardOptions
		case NetworkTypeVirtle:
			if managed++; managed > 1 {
				return nil, fmt.Errorf("manifest.networks[%d]: a machine attaches to one virtle network", i)
			}
			device.Backend = "stream"
			device.Managed = true
			// The default MAC is the same address for every machine; on a
			// shared network each port needs its own, so only a MAC the
			// manifest chose is requested.
			if network.MAC == "" || network.MAC == defaultNetworkMAC {
				device.MacAddress = ""
			}
			forwards, err := resolveManagedForwards(network.Forward, i)
			if err != nil {
				return nil, err
			}
			device.Forward = forwards
		case NetworkTypeTAP:
			if err := validateTapName(network.Tap); err != nil {
				return nil, fmt.Errorf("manifest.networks[%d].tap %w", i, err)
			}
			if len(network.Forward) > 0 {
				return nil, fmt.Errorf("manifest.networks[%d].forward is not supported on a tap network; the host kernel routes it", i)
			}
			device.Backend = "tap"
			device.NetdevOptions = []string{"ifname=" + network.Tap, "script=no", "downscript=no"}
		default:
			return nil, fmt.Errorf("manifest.networks[%d].type must be one of user, virtle, or tap", i)
		}
		devices = append(devices, device)
	}
	return devices, nil
}

// resolveManagedForwards normalizes the host->guest forwards a virtle
// network's port exposes.
func resolveManagedForwards(ports []ForwardPort, networkIndex int) ([]HotplugForward, error) {
	forwards := make([]HotplugForward, 0, len(ports))
	for i, port := range ports {
		normalized, err := normalizeForwardPort(port, fmt.Sprintf("manifest.networks[%d].forward[%d]", networkIndex, i))
		if err != nil {
			return nil, err
		}
		if normalized.From != "host" {
			return nil, fmt.Errorf("manifest.networks[%d].forward[%d].from guest is not supported on a virtle network yet", networkIndex, i)
		}
		forwards = append(forwards, HotplugForward{
			Proto: normalized.Proto,
			Host:  formatPortEndpoint(normalized.Host),
			Guest: formatPortEndpoint(normalized.Guest),
		})
	}
	return forwards, nil
}

// validateTapName accepts a Linux interface name, which also keeps QEMU's
// comma-separated option syntax intact.
func validateTapName(name string) error {
	const ifnamsiz = 15
	if name == "" {
		return errors.New("is required for type tap")
	}
	if len(name) > ifnamsiz {
		return fmt.Errorf("%q is longer than %d characters", name, ifnamsiz)
	}
	for _, r := range name {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '_' || r == '-' || r == '.') {
			return fmt.Errorf("%q is not an interface name", name)
		}
	}
	return nil
}

func parsePortEndpoint(value string) (PortEndpoint, error) {
	host, port, err := net.SplitHostPort(value)
	if err != nil {
		index := strings.LastIndex(value, ":")
		if index < 0 {
			return PortEndpoint{}, fmt.Errorf("missing :port")
		}
		host = value[:index]
		port = value[index+1:]
	}
	if port == "" {
		return PortEndpoint{}, fmt.Errorf("missing port")
	}
	parsedPort, err := strconv.Atoi(port)
	if err != nil {
		return PortEndpoint{}, fmt.Errorf("port must be an integer")
	}
	if parsedPort <= 0 || parsedPort > 65535 {
		return PortEndpoint{}, fmt.Errorf("port must be between 1 and 65535")
	}
	return PortEndpoint{Address: host, Port: parsedPort}, nil
}

type normalizedForwardPort struct {
	Proto string
	From  string
	Host  PortEndpoint
	Guest PortEndpoint
}

func normalizeForwardPort(port ForwardPort, fieldPath string) (normalizedForwardPort, error) {
	proto := port.Proto
	if proto == "" {
		proto = "tcp"
	}
	if proto != "tcp" && proto != "udp" {
		return normalizedForwardPort{}, fmt.Errorf("%s.proto must be one of tcp or udp", fieldPath)
	}
	from := port.From
	if from == "" {
		from = "host"
	}
	if from != "host" && from != "guest" {
		return normalizedForwardPort{}, fmt.Errorf("%s.from must be one of host or guest", fieldPath)
	}
	hostEndpoint, err := parsePortEndpoint(port.Host)
	if err != nil {
		return normalizedForwardPort{}, fmt.Errorf("%s.host %s", fieldPath, err)
	}
	guestEndpoint, err := parsePortEndpoint(port.Guest)
	if err != nil {
		return normalizedForwardPort{}, fmt.Errorf("%s.guest %s", fieldPath, err)
	}
	return normalizedForwardPort{
		Proto: proto,
		From:  from,
		Host:  hostEndpoint,
		Guest: guestEndpoint,
	}, nil
}

func formatPortEndpoint(endpoint PortEndpoint) string {
	return net.JoinHostPort(endpoint.Address, strconv.Itoa(endpoint.Port))
}

func resolveForwardPorts(ports []ForwardPort, fwdTunnelExec []string, networkIndex int) ([]string, error) {
	options := make([]string, 0, len(ports))
	if len(fwdTunnelExec) == 0 {
		fwdTunnelExec = []string{"nc", "{{.Host}}", "{{.Port}}"}
	}
	for i, port := range ports {
		normalized, err := normalizeForwardPort(port, fmt.Sprintf("manifest.networks[%d].forward[%d]", networkIndex, i))
		if err != nil {
			return nil, err
		}
		if normalized.From == "host" {
			options = append(options, fmt.Sprintf("hostfwd=%s:%s:%d-%s:%d", normalized.Proto, normalized.Host.Address, normalized.Host.Port, normalized.Guest.Address, normalized.Guest.Port))
		} else {
			if err := rejectLegacyFwdTunnelExecEnv(fwdTunnelExec); err != nil {
				return nil, fmt.Errorf("manifest.qemu.fwd_tunnel_exec (manifest.networks[%d].forward[%d]): %w", networkIndex, i, err)
			}
			command, err := renderFwdTunnelExec(fwdTunnelExec, normalized.Host)
			if err != nil {
				return nil, fmt.Errorf("manifest.qemu.fwd_tunnel_exec (manifest.networks[%d].forward[%d]): %w", networkIndex, i, err)
			}
			options = append(options, fmt.Sprintf("guestfwd=%s:%s:%d-cmd:%s", normalized.Proto, normalized.Guest.Address, normalized.Guest.Port, shellquote.Join(command...)))
		}
	}
	return options, nil
}

func rejectLegacyFwdTunnelExecEnv(exec []string) error {
	for i, arg := range exec {
		switch arg {
		case "$HOST":
			return fmt.Errorf("exec[%d] uses legacy $HOST; use {{.Host}}", i)
		case "$PORT":
			return fmt.Errorf("exec[%d] uses legacy $PORT; use {{.Port}}", i)
		}
	}
	return nil
}

func renderFwdTunnelExec(exec []string, hostEndpoint PortEndpoint) ([]string, error) {
	renderer, err := NewTemplateRenderer(ForwardTemplateProvider{
		Host: hostEndpoint.Address,
		Port: hostEndpoint.Port,
	})
	if err != nil {
		return nil, err
	}
	command, err := renderer.RenderArgv(exec)
	if err != nil {
		return nil, err
	}
	return command, nil
}

func resolveBalloon(facts *BalloonInput, transport string) *BalloonDevice {
	if facts == nil || !facts.Enabled {
		return nil
	}
	device := &BalloonDevice{
		ID:                "balloon0",
		Transport:         transport,
		DeflateOnOOM:      facts.DeflateOnOOM,
		FreePageReporting: boolValueDefault(facts.FreePageReporting, true),
	}
	if facts.Controller != nil {
		device.Controller = &BalloonControllerConfig{
			MinActual:             facts.Controller.MinActual,
			MaxActual:             facts.Controller.MaxActual,
			GrowBelowAvailable:    facts.Controller.GrowBelowAvailable,
			ReclaimAboveAvailable: facts.Controller.ReclaimAboveAvailable,
			Step:                  facts.Controller.Step,
			PollInterval:          facts.Controller.PollInterval.Duration(),
			ReclaimHoldoff:        facts.Controller.ReclaimHoldoff.Duration(),
		}
	}
	return device
}

func resolveWriteFiles(files []WriteFileInput) WriteFiles {
	result := make(WriteFiles, len(files))
	for _, file := range files {
		result[file.GuestPath] = WriteFile{
			Chown:       stringValue(file.Chown),
			Mode:        stringValue(file.Mode),
			Overwrite:   boolValue(file.Overwrite),
			FollowLinks: boolValueDefault(file.FollowLinks, true),
			WriteBack:   boolValue(file.WriteBack),
			Content:     resolveWriteFileContent(file),
		}
	}
	return result
}

func resolveWriteFileContent(file WriteFileInput) WriteFileContent {
	switch {
	case file.Text != nil && file.Path != nil:
		return WriteFileContent{Kind: WriteFileContentNone, Text: *file.Text, Path: *file.Path}
	case file.Text != nil:
		return WriteFileContent{Kind: WriteFileContentText, Text: *file.Text}
	case file.Path != nil:
		return WriteFileContent{Kind: WriteFileContentPath, Path: *file.Path}
	default:
		return WriteFileContent{}
	}
}

func resolveImageFormat(format string) string {
	if format == "" {
		return "raw"
	}
	return format
}

func renderVirtioFSArgv(argv []string, socketPath string, source string, tag string) ([]string, error) {
	renderer, err := NewTemplateRenderer(VirtioFSTemplateProvider{
		SocketPath: socketPath,
		SourcePath: source,
		Tag:        tag,
	})
	if err != nil {
		return nil, err
	}
	return renderer.RenderArgv(argv)
}

func resolveGraphics(graphics *GraphicsInput) QEMUGraphics {
	if graphics == nil || graphics.Backend == "" || graphics.Backend == defaultGraphicsBackend {
		return QEMUGraphics{}
	}
	return QEMUGraphics{Backend: graphics.Backend}
}

func resolveNotifications(notifications NotificationsInput) Notifications {
	return Notifications{
		States:  append([]string(nil), notifications.States...),
		Command: commandFromExec(notifications.Exec),
	}
}

func resolveRun(runs []RunInput) []Run {
	result := make([]Run, 0, len(runs))
	for _, run := range runs {
		result = append(result, Run{
			Exec: append([]string(nil), run.Exec...),
			Vars: cloneValueMap(run.Vars),
		})
	}
	return result
}

func commandFromExec(exec []string) Command {
	if len(exec) == 0 {
		return Command{}
	}
	return Command{
		Path: exec[0],
		Args: append([]string(nil), exec[1:]...),
	}
}

func cloneStringMap(values map[string]string) map[string]string {
	if len(values) == 0 {
		return map[string]string{}
	}
	clone := make(map[string]string, len(values))
	for key, value := range values {
		clone[key] = value
	}
	return clone
}

func cloneValueMap(values map[string]any) map[string]any {
	if len(values) == 0 {
		return nil
	}
	clone := make(map[string]any, len(values))
	for key, value := range values {
		clone[key] = value
	}
	return clone
}

func persistenceDirectories(volumes []ImageMountInput, stateDir string) []string {
	dirs := []string{stateDir}
	for _, volume := range volumes {
		if volume.SourcePath == "" {
			continue
		}
		dir := filepath.Dir(volume.SourcePath)
		if dir == "." {
			continue
		}
		dirs = append(dirs, dir)
	}
	return uniqueStrings(dirs)
}

func uniqueStrings(values []string) []string {
	seen := make(map[string]struct{}, len(values))
	result := make([]string, 0, len(values))
	for _, value := range values {
		if value == "" {
			continue
		}
		if _, ok := seen[value]; ok {
			continue
		}
		seen[value] = struct{}{}
		result = append(result, value)
	}
	return result
}

func qemuPassthroughArgs(qemu QEMUInput, exec []string) []string {
	extraArgs := []string(nil)
	if len(exec) > 1 {
		extraArgs = exec[1:]
	}
	args := make([]string, 0, len(extraArgs)+2)
	if qemu.User != "" {
		args = append(args, "-user", qemu.User)
	}
	args = append(args, extraArgs...)
	return args
}

func qemuArch(goArch string) string {
	switch goArch {
	case "amd64":
		return "x86_64"
	case "arm64":
		return "aarch64"
	default:
		return goArch
	}
}

// OnOff renders value as the "on"/"off" text QEMU option strings expect.
func OnOff(value bool) string {
	if value {
		return "on"
	}
	return "off"
}

func stringValue(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}

func boolValue(value *bool) bool {
	return value != nil && *value
}

func boolValueDefault(value *bool, fallback bool) bool {
	if value == nil {
		return fallback
	}
	return *value
}
