package manager

import (
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"syscall"

	govmmQemu "github.com/kata-containers/govmm/qemu"
	"github.com/shazow/virtle/internal/balloon"
	"github.com/shazow/virtle/internal/executor"
	"github.com/shazow/virtle/internal/manifest"
)

func buildQEMUCommand(manifest *manifest.Manifest, cid int, incoming bool) (*exec.Cmd, error) {
	qemu, err := manifest.ResolvedQEMU()
	if err != nil {
		return nil, err
	}

	args, err := buildQEMUArgs(qemu, cid, incoming)
	if err != nil {
		return nil, err
	}

	cmd := executor.Command(qemu.BinaryPath, args, nil)
	cmd.Dir = manifest.Paths.WorkingDir
	if qemu.Console.Interactive {
		cmd.Stdin = os.Stdin
	} else {
		cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	}
	cmd.Stdout = os.Stderr
	cmd.Stderr = os.Stderr
	return cmd, nil
}

func buildQEMUArgs(qemu manifest.QEMU, cid int, incoming bool) ([]string, error) {
	config := &govmmQemu.Config{
		Machine: govmmQemu.Machine{
			Type: qemu.Machine.Type,
		},
	}

	args := make([]string, 0, 64)

	args = append(args, "-name", qemu.Name)

	machineArg := qemu.Machine.Type
	if len(qemu.Machine.Options) > 0 {
		machineArg = machineArg + "," + strings.Join(qemu.Machine.Options, ",")
	}
	args = append(args, "-M", machineArg)

	args = append(args, "-m", fmt.Sprintf("%d", qemu.Memory.Size))
	args = append(args, "-smp", fmt.Sprintf("%d", qemuCPUCount(qemu.SMP.CPUs)))

	for i := 0; i < qemu.Hotplug.PCIEPorts; i++ {
		args = append(args, "-device", fmt.Sprintf("pcie-root-port,id=pcie.hotplug.%d,bus=pcie.0,chassis=%d,slot=%d", i, i+1, i+1))
	}

	if qemu.Knobs.NoDefaults {
		args = append(args, "-nodefaults")
	}
	if qemu.Knobs.NoUserConfig {
		args = append(args, "-no-user-config")
	}
	if qemu.Knobs.NoReboot {
		args = append(args, "-no-reboot")
	}

	args = append(args, "-kernel", qemu.Kernel.Path)
	args = append(args, "-initrd", qemu.Kernel.InitrdPath)

	if qemu.Console.StdioChardev {
		// mux=on enables QEMU's Ctrl-A escape commands, including Ctrl-A x to quit.
		// See https://www.qemu.org/docs/master/system/mux-chardev.html.
		args = append(args, "-chardev", "stdio,id=stdio,mux=on,signal=off")
	}

	rngTransport, err := resolveQEMUTransport(qemu.Devices.RNG.Transport)
	if err != nil {
		return nil, err
	}
	args = append(args, govmmQemu.RngDevice{
		ID:        qemu.Devices.RNG.ID,
		Transport: rngTransport,
	}.QemuParams(config)...)

	if qemu.MachineID != "" {
		args = append(args, "-smbios", fmt.Sprintf("type=1,uuid=%s", qemu.MachineID))
	}

	if qemu.Console.SerialConsole {
		args = append(args, "-serial", "chardev:stdio")
	}
	if qemu.CPU.EnableKVM {
		args = append(args, "-enable-kvm")
	}

	args = append(args, "-cpu", qemu.CPU.Model)

	if qemu.Kernel.Params != "" {
		args = append(args, "-append", qemu.Kernel.Params)
	}
	if qemu.Devices.I8042 {
		args = append(args, "-device", "i8042")
	}
	if qemu.NoGraphicEnabled() {
		args = append(args, "-nographic")
	} else if !qemu.Graphics.IsZero() {
		displayArgs, err := qemuGraphicsArgs(qemu.Graphics)
		if err != nil {
			return nil, err
		}
		args = append(args, displayArgs...)
	}
	if qemu.Knobs.SeccompSandbox {
		args = append(args, "-sandbox", "on")
	}

	args = append(args, "-qmp", fmt.Sprintf("unix:%s,server,nowait", qemu.QMP.SocketPath))

	if qemu.GuestAgent.SocketPath != "" {
		serialDriver, err := guestAgentSerialDriver(qemu.Devices.VSOCK.Transport)
		if err != nil {
			return nil, err
		}
		args = append(args,
			"-chardev", fmt.Sprintf("socket,path=%s,server=on,wait=off,id=qga0", qemu.GuestAgent.SocketPath),
			"-device", fmt.Sprintf("%s,id=qga0-serial", serialDriver),
			"-device", "virtserialport,chardev=qga0,name=org.qemu.guest_agent.0",
		)
	}

	if qemu.SSHReady.SocketPath != "" {
		serialDriver, err := guestAgentSerialDriver(qemu.Devices.VSOCK.Transport)
		if err != nil {
			return nil, err
		}
		args = append(args,
			"-chardev", fmt.Sprintf("socket,path=%s,server=on,wait=off,id=ready_char", qemu.SSHReady.SocketPath),
			"-device", fmt.Sprintf("%s,id=ready-serial", serialDriver),
			"-device", "virtserialport,chardev=ready_char,name=virtle.ready",
		)
	}

	switch qemu.Memory.Backend {
	case "", "default":
		// No extra memory object required.
	case "memfd":
		args = append(args, "-numa", "node,memdev=mem")
		args = append(args, "-object", fmt.Sprintf("memory-backend-memfd,id=mem,size=%dM,share=%s", qemu.Memory.Size, onOff(qemu.Memory.Shared)))
	default:
		return nil, fmt.Errorf("unsupported qemu memory backend %q", qemu.Memory.Backend)
	}

	args, err = balloon.AppendQEMUArgs(args, config, resolveQEMUTransport, qemu.Devices.Balloon)
	if err != nil {
		return nil, err
	}

	if len(qemu.Devices.Mounts) > 0 {
		for _, mount := range qemu.Devices.Mounts {
			args, err = appendQEMUMountArgs(config, args, mount)
			if err != nil {
				return nil, err
			}
		}
	} else {
		for _, share := range qemu.Devices.VirtioFS {
			args, err = appendVirtioFSArgs(config, args, share)
			if err != nil {
				return nil, err
			}
		}
		for _, share := range qemu.Devices.NineP {
			args, err = appendNinePArgs(args, share)
			if err != nil {
				return nil, err
			}
		}
		for _, block := range qemu.Devices.Block {
			args, err = appendBlockArgs(args, block)
			if err != nil {
				return nil, err
			}
		}
	}

	for _, netdev := range qemu.Devices.Network {
		netTransport, err := resolveQEMUTransport(netdev.Transport)
		if err != nil {
			return nil, err
		}
		driver := govmmQemu.VirtioNetTransport[netTransport]

		netdevParams := []string{
			netdev.Backend,
			fmt.Sprintf("id=%s", netdev.ID),
		}
		netdevParams = append(netdevParams, netdev.NetdevOptions...)

		deviceParams := []string{
			driver,
			fmt.Sprintf("netdev=%s", netdev.ID),
			fmt.Sprintf("mac=%s", netdev.MacAddress),
		}
		if netdev.DisableROM {
			deviceParams = append(deviceParams, "romfile=")
		} else if netdev.RomFile != "" {
			deviceParams = append(deviceParams, fmt.Sprintf("romfile=%s", netdev.RomFile))
		}
		if netdev.MQVectors > 0 {
			deviceParams = append(deviceParams, "mq=on", fmt.Sprintf("vectors=%d", netdev.MQVectors))
		}

		args = append(args, "-netdev", strings.Join(netdevParams, ","))
		args = append(args, "-device", strings.Join(deviceParams, ","))
	}

	vsockTransport, err := resolveQEMUTransport(qemu.Devices.VSOCK.Transport)
	if err != nil {
		return nil, err
	}
	args = append(args, govmmQemu.VSOCKDevice{
		ID:        qemu.Devices.VSOCK.ID,
		ContextID: uint64(cid),
		Transport: vsockTransport,
	}.QemuParams(config)...)

	if incoming {
		args = append(args, "-incoming", "defer")
	}

	args = append(args, qemu.PassthroughArgs...)

	return args, nil
}

func appendQEMUMountArgs(config *govmmQemu.Config, args []string, mount manifest.QEMUMountDevice) ([]string, error) {
	switch mount.Type {
	case manifest.MountTypeVirtioFS:
		if mount.VirtioFS == nil {
			return nil, fmt.Errorf("qemu mount %q missing virtiofs device", mount.Type)
		}
		return appendVirtioFSArgs(config, args, *mount.VirtioFS)
	case manifest.MountTypeNineP:
		if mount.NineP == nil {
			return nil, fmt.Errorf("qemu mount %q missing 9p device", mount.Type)
		}
		return appendNinePArgs(args, *mount.NineP)
	case manifest.MountTypeImage:
		if mount.Block == nil {
			return nil, fmt.Errorf("qemu mount %q missing block device", mount.Type)
		}
		return appendBlockArgs(args, *mount.Block)
	default:
		return nil, fmt.Errorf("unsupported qemu mount type %q", mount.Type)
	}
}

func appendVirtioFSArgs(config *govmmQemu.Config, args []string, share manifest.QEMUVirtioFSShare) ([]string, error) {
	shareTransport, err := resolveQEMUTransport(share.Transport)
	if err != nil {
		return nil, err
	}
	return append(args, govmmQemu.VhostUserDevice{
		SocketPath:    share.SocketPath,
		CharDevID:     "char-" + share.ID,
		Tag:           share.Tag,
		VhostUserType: govmmQemu.VhostUserFS,
		Transport:     shareTransport,
	}.QemuParams(config)...), nil
}

func appendNinePArgs(args []string, share manifest.QEMUNinePShare) ([]string, error) {
	driver, err := ninePDriver(share.Transport)
	if err != nil {
		return nil, err
	}
	fsdevParams := []string{
		"local",
		fmt.Sprintf("id=%s", share.ID),
		fmt.Sprintf("path=%s", share.SourcePath),
		fmt.Sprintf("security_model=%s", share.SecurityModel),
		fmt.Sprintf("readonly=%s", onOff(share.ReadOnly)),
	}
	deviceParams := []string{
		driver,
		fmt.Sprintf("fsdev=%s", share.ID),
		fmt.Sprintf("mount_tag=%s", share.Tag),
	}
	args = append(args, "-fsdev", strings.Join(fsdevParams, ","))
	args = append(args, "-device", strings.Join(deviceParams, ","))
	return args, nil
}

func appendBlockArgs(args []string, block manifest.QEMUBlockDevice) ([]string, error) {
	blockTransport, err := resolveQEMUTransport(block.Transport)
	if err != nil {
		return nil, err
	}
	driver := govmmQemu.VirtioBlockTransport[blockTransport]
	format := block.Format
	if format == "" {
		format = "raw"
	}

	driveParams := []string{
		fmt.Sprintf("id=%s", block.ID),
		fmt.Sprintf("format=%s", format),
		fmt.Sprintf("file=%s", block.ImagePath),
		"if=none",
	}
	if block.AIO != "" {
		driveParams = append(driveParams, fmt.Sprintf("aio=%s", block.AIO))
	}
	driveParams = append(driveParams, "discard=unmap")
	if block.Cache != "" {
		driveParams = append(driveParams, fmt.Sprintf("cache=%s", block.Cache))
	}
	driveParams = append(driveParams, fmt.Sprintf("read-only=%s", onOff(block.ReadOnly)))

	deviceParams := []string{
		driver,
		fmt.Sprintf("drive=%s", block.ID),
	}
	if block.Serial != "" {
		deviceParams = append(deviceParams, fmt.Sprintf("serial=%s", block.Serial))
	}

	args = append(args, "-drive", strings.Join(driveParams, ","))
	args = append(args, "-device", strings.Join(deviceParams, ","))
	return args, nil
}

func qemuCPUCount(cpus manifest.CPUCount) int {
	if cpus.Set {
		return cpus.Value
	}
	count := runtime.NumCPU()
	if count < 1 {
		return 1
	}
	return count
}

func qemuGraphicsArgs(graphics manifest.QEMUGraphics) ([]string, error) {
	args := []string{}
	switch graphics.Backend {
	case "gtk":
		args = append(args, "-display", "gtk,gl=off", "-device", "virtio-vga")
	case "cocoa":
		args = append(args, "-display", "cocoa", "-device", "virtio-gpu")
	default:
		return nil, fmt.Errorf("unsupported qemu graphics backend %q", graphics.Backend)
	}

	return append(args,
		"-device", "qemu-xhci",
		"-device", "usb-tablet",
		"-device", "usb-kbd",
	), nil
}

func guestAgentSerialDriver(transport string) (string, error) {
	switch transport {
	case "pci":
		return "virtio-serial-pci", nil
	case "mmio":
		return "virtio-serial-device", nil
	case "ccw":
		return "virtio-serial-ccw", nil
	default:
		return "", fmt.Errorf("unsupported qemu transport %q", transport)
	}
}

func ninePDriver(transport string) (string, error) {
	switch transport {
	case "pci":
		return "virtio-9p-pci", nil
	case "mmio":
		return "virtio-9p-device", nil
	case "ccw":
		return "virtio-9p-ccw", nil
	default:
		return "", fmt.Errorf("unsupported qemu transport %q", transport)
	}
}

func resolveQEMUTransport(value string) (govmmQemu.VirtioTransport, error) {
	switch value {
	case "pci":
		return govmmQemu.TransportPCI, nil
	case "mmio":
		return govmmQemu.TransportMMIO, nil
	case "ccw":
		return govmmQemu.TransportCCW, nil
	default:
		return "", fmt.Errorf("unsupported qemu transport %q", value)
	}
}

func onOff(v bool) string {
	if v {
		return "on"
	}
	return "off"
}
