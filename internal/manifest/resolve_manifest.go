package manifest

import (
	"errors"
	"fmt"
	"math"
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
	"github.com/shazow/virtle/internal/balloon"
	"github.com/shazow/virtle/internal/executor"
	"github.com/shazow/virtle/internal/hotplug"
)

const virtioFSSocketProbeTimeout = 100 * time.Millisecond

func (d Document) Manifest() (*Manifest, error) {
	return d.ManifestWithOptions(ResolveOptions{})
}

func (d Document) ManifestWithOptions(options ResolveOptions) (*Manifest, error) {
	d = DocumentWithDefaults(d)
	if d.Kernel.Path == "" {
		return nil, fmt.Errorf("manifest.kernel.path is required")
	}
	if d.Kernel.InitrdPath == "" {
		return nil, fmt.Errorf("manifest.kernel.initrd_path is required")
	}
	retryDelay, err := resolveRetryDelay(d.SSH.RetryDelay)
	if err != nil {
		return nil, err
	}
	guestDefaultTimeout, err := resolveGuestDefaultTimeout(d.QEMU.GuestDefaultTimeout)
	if err != nil {
		return nil, err
	}

	host := d.Host.withDefaults()
	m := &Manifest{
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
			RetryDelay:    retryDelay,
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
	hotplugCount := d.hotplugCount()
	qemu, err := d.resolveQEMU(host, m.Identity.HostName, m.Paths.WorkingDir, m.Persistence.StateDir, hotplugCount)
	if err != nil {
		return nil, err
	}
	qemu.GuestAgent.CommandTimeout = guestDefaultTimeout
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

func resolveRetryDelay(seconds *float64) (time.Duration, error) {
	return resolveSecondsDuration(seconds, defaultSSHRetryDelaySeconds, "manifest.ssh.retry_delay")
}

func resolveGuestDefaultTimeout(seconds *float64) (time.Duration, error) {
	return resolveSecondsDuration(seconds, defaultGuestDefaultTimeoutSeconds, "manifest.qemu.guest_default_timeout")
}

func resolveSecondsDuration(seconds *float64, defaultSeconds float64, field string) (time.Duration, error) {
	if seconds == nil {
		return time.Duration(defaultSeconds * float64(time.Second)), nil
	}
	if math.IsNaN(*seconds) || math.IsInf(*seconds, 0) || *seconds < 0 {
		return 0, fmt.Errorf("%s must be a finite number greater than or equal to zero", field)
	}
	return time.Duration(*seconds * float64(time.Second)), nil
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

func (d Document) resolveQEMU(host HostInput, hostName string, workingDir string, stateDir string, hotplugCount int) (QEMU, error) {
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
		HostName:   hostName,
		WorkingDir: workingDir,
		StateDir:   stateDir,
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
		Name:       hostName,
		Machine: QEMUMachine{
			Type:    machineType,
			Options: resolveMachineOptions(host, machineType, d.QEMU.MachineOptions, transport == "pci"),
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
			Params:     kernelParams(host, d.Kernel),
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
			SocketPath: guestAgentSocket,
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
				ID:        "vsock0",
				Transport: transport,
			},
		},
		MachineID:       stringValue(d.Machine.ID),
		PassthroughArgs: qemuPassthroughArgs(d.QEMU, qemuExec),
	}
	return qemu, nil
}

func resolveCPUCount(cpus *int) CPUCount {
	if cpus == nil {
		return CPUCount{}
	}
	return ExplicitCPUs(*cpus)
}

func (d Document) hotplugCount() int {
	return d.Hotplug.Len()
}

func qemuTransport(machineType string, mounts MountsInput, graphics QEMUGraphics, requirePCI bool) string {
	if requirePCI || !strings.HasPrefix(machineType, "microvm") || mounts.RequiresPCI() || !graphics.IsZero() {
		return "pci"
	}
	return "mmio"
}

func resolveMachineOptions(host HostInput, machineType string, explicit map[string]string, requirePCI bool) []string {
	options := explicit
	if options == nil {
		options = defaultMachineOptions(host, machineType, requirePCI)
	} else {
		options = cloneStringMap(explicit)
		if requirePCI && strings.HasPrefix(machineType, "microvm") {
			options["pcie"] = "on"
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
			options["pcie"] = boolOnOff(requirePCI)
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

func kernelParams(host HostInput, kernel KernelInput) string {
	params := make([]string, 0, len(kernel.Params)+3)
	if mode, _ := kernelSerialMode(kernel); mode != KernelSerialOff {
		switch host.System {
		case "x86_64-linux":
			params = append(params, "earlyprintk=ttyS0 console=ttyS0")
		case "aarch64-linux":
			params = append(params, "console=ttyAMA0")
		}
	}
	params = append(params, "reboot=t", "panic=-1")
	params = append(params, kernel.Params...)
	return strings.Join(params, " ")
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

func resolveBlocks(volumes []ImageMountInput, host HostInput, transport string) []QEMUBlockDevice {
	blocks := make([]QEMUBlockDevice, 0, len(volumes))
	for i, volume := range volumes {
		block := QEMUBlockDevice{
			ID:        "vd" + string(rune('a'+i)),
			ImagePath: volume.SourcePath,
			Format:    resolveImageFormat(volume.Image.Format),
			AIO:       aioEngine(host),
			ReadOnly:  volume.ReadOnly,
			Serial:    stringValue(volume.Image.Serial),
			Transport: transport,
		}
		if volume.Image.Direct {
			block.Cache = "none"
		}
		blocks = append(blocks, block)
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
	return Workspace{
		GuestDir: workspace.GuestDir,
		HostDir:  workspace.HostDir,
		MountCWD: workspace.MountCWD,
	}
}

func resolveVirtioFSMounts(mounts []VirtioFSMountInput, transport string) []QEMUVirtioFSShare {
	shares := make([]QEMUVirtioFSShare, 0, len(mounts))
	for i, mount := range mounts {
		shares = append(shares, QEMUVirtioFSShare{
			ID:         "fs" + strconv.Itoa(i),
			SocketPath: mount.VirtioFS.Socket,
			Tag:        mount.Tag,
			Transport:  transport,
		})
	}
	return shares
}

func resolveNinePMounts(mounts []NinePMountInput, transport string) []QEMUNinePShare {
	shares := make([]QEMUNinePShare, 0, len(mounts))
	for i, mount := range mounts {
		securityModel := mount.NineP.SecurityModel
		if securityModel == "" {
			securityModel = "mapped"
		}
		shares = append(shares, QEMUNinePShare{
			ID:            "fs9p" + strconv.Itoa(i),
			SourcePath:    mount.SourcePath,
			Tag:           mount.Tag,
			SecurityModel: securityModel,
			ReadOnly:      mount.ReadOnly,
			Transport:     transport,
		})
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
		bin := mount.VirtioFS.Bin
		if bin == "" {
			bin = "virtiofsd"
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
			Exec: append([]string{m.resolveOptionalBin(bin, "virtiofsd")}, args...),
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

func (m *Manifest) resolveHotplug(d Document) ([]hotplug.Device, error) {
	hotplugs := make([]hotplug.Device, 0, d.hotplugCount())
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

func (m *Manifest) resolveHotplugMount(entry MountEntry) (hotplug.Device, error) {
	switch typed := entry.(type) {
	case VirtioFSMountInput:
		return m.resolveVirtioFSHotplug(typed)
	case ImageMountInput:
		return m.resolveImageHotplug(typed)
	default:
		return hotplug.Device{}, fmt.Errorf("type %q does not support hotplug", entry.mountType())
	}
}

func (m *Manifest) resolveImageHotplug(entry ImageMountInput) (hotplug.Device, error) {
	serial := stringValue(entry.Image.Serial)
	format := resolveImageFormat(entry.Image.Format)
	id, err := renderHotplugID(serial, "{{.Serial}}", StaticTemplateContext(executor.Context{
		"Serial": serial,
		"Source": entry.SourcePath,
		"Format": format,
	}))
	if err != nil {
		return hotplug.Device{}, err
	}
	return hotplug.Device{
		Kind: hotplug.KindBlock,
		ID:   id,
		Block: hotplug.Block{
			ImagePath: m.resolvePath(entry.SourcePath),
			Format:    format,
			ReadOnly:  entry.ReadOnly,
			Serial:    serial,
		},
	}, nil
}

func (m *Manifest) resolveVirtioFSHotplug(mount VirtioFSMountInput) (hotplug.Device, error) {
	id := mount.Tag
	socket := mount.VirtioFS.Socket
	if socket == "" {
		socket = id + ".sock"
	}
	socketPath, err := m.resolveSocketPath(socket)
	if err != nil {
		return hotplug.Device{}, err
	}
	bin := mount.VirtioFS.Bin
	if bin == "" {
		bin = "virtiofsd"
	}
	source := m.resolvePath(mount.SourcePath)
	args := append([]string(nil), mount.VirtioFS.Args...)
	if len(args) == 0 {
		args = hotplug.DefaultVirtioFSArgs(socketPath, source, id)
	} else {
		renderedArgs, err := renderVirtioFSArgv(args, socketPath, source, id)
		if err != nil {
			return hotplug.Device{}, err
		}
		args = renderedArgs
	}
	return hotplug.Device{
		Kind: hotplug.KindVirtioFS,
		ID:   id,
		VirtioFS: hotplug.VirtioFS{
			Source:     source,
			Target:     mount.Target,
			SocketPath: socketPath,
			Bin:        m.resolveOptionalBin(bin, "virtiofsd"),
			Args:       args,
		},
	}, nil
}

func (m *Manifest) resolveOptionalBin(bin string, defaultBin string) string {
	if bin == "" || bin == defaultBin {
		return defaultBin
	}
	return m.resolvePath(bin)
}

func resolveNetworkHotplug(entry NetworkInput, index int) (hotplug.Device, error) {
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
	forward := make([]hotplug.Forward, 0, len(entry.Forward))
	for i, fwd := range entry.Forward {
		normalized, err := normalizeForwardPort(fwd, fmt.Sprintf("forward[%d]", i))
		if err != nil {
			return hotplug.Device{}, err
		}
		if normalized.From == "guest" {
			return hotplug.Device{}, fmt.Errorf("forward[%d].from guest is not supported for hotplug networks", i)
		}
		forward = append(forward, hotplug.Forward{
			Proto: normalized.Proto,
			Host:  formatPortEndpoint(normalized.Host),
			Guest: formatPortEndpoint(normalized.Guest),
		})
	}
	return hotplug.Device{
		Kind: hotplug.KindNet,
		ID:   id,
		Net: hotplug.Net{
			Backend: backend,
			MAC:     mac,
			Forward: forward,
		},
	}, nil
}

func renderHotplugID(id string, defaultID string, provider TemplateProvider) (string, error) {
	if id == "" {
		id = defaultID
	}
	renderer, err := NewTemplateRenderer(provider)
	if err != nil {
		return "", err
	}
	rendered, err := renderer.RenderString(id)
	if err != nil {
		return "", err
	}
	if rendered == "" {
		return "", fmt.Errorf("id is required")
	}
	return rendered, nil
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
	for i, network := range networks {
		id := network.ID
		if id == "" {
			id = defaultNetworkID
		}
		backend := network.Type
		if backend == "" {
			backend = defaultNetworkType
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
		forwardOptions, err := resolveForwardPorts(network.Forward, fwdTunnelExec, i)
		if err != nil {
			return nil, err
		}
		devices = append(devices, QEMUNetDevice{
			ID:            id,
			Backend:       backend,
			MacAddress:    mac,
			Transport:     transport,
			DisableROM:    disableROM,
			NetdevOptions: forwardOptions,
			MQVectors:     mqVectors,
		})
	}
	return devices, nil
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
				return nil, fmt.Errorf("manifest.networks[%d].forward[%d].fwd_tunnel_exec: %w", networkIndex, i, err)
			}
			command, err := renderFwdTunnelExec(fwdTunnelExec, normalized.Host)
			if err != nil {
				return nil, fmt.Errorf("manifest.networks[%d].forward[%d].fwd_tunnel_exec: %w", networkIndex, i, err)
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

func resolveBalloon(facts *BalloonInput, transport string) *balloon.Device {
	if facts == nil || !facts.Enabled {
		return nil
	}
	device := &balloon.Device{
		ID:                "balloon0",
		Transport:         transport,
		DeflateOnOOM:      facts.DeflateOnOOM,
		FreePageReporting: boolValueDefault(facts.FreePageReporting, true),
	}
	if facts.Controller != nil {
		device.Controller = &balloon.ControllerConfig{
			MinActual:             facts.Controller.MinActual,
			MaxActual:             facts.Controller.MaxActual,
			GrowBelowAvailable:    facts.Controller.GrowBelowAvailable,
			ReclaimAboveAvailable: facts.Controller.ReclaimAboveAvailable,
			Step:                  facts.Controller.Step,
			PollIntervalSeconds:   facts.Controller.PollIntervalSeconds,
			ReclaimHoldoffSeconds: facts.Controller.ReclaimHoldoffSeconds,
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
	result := Notifications{
		States: append([]string(nil), notifications.States...),
	}
	if len(notifications.Exec) > 0 {
		command := commandFromExec(notifications.Exec)
		result.Command = command
	}
	return result
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
	if qemu.User != nil && *qemu.User != "" {
		args = append(args, "-user", *qemu.User)
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

func boolOnOff(value bool) string {
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
