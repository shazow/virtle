package manifest

import (
	"fmt"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"text/template"
	"text/template/parse"
	"time"

	hotplugpkg "github.com/shazow/virtle/internal/hotplug"
	"github.com/shazow/virtle/internal/units"
)

const (
	defaultSSHRetryDelaySeconds = 0.5
	defaultSSHReadySocket       = ""
	defaultVSockCIDStart        = 3
	defaultVSockCIDEnd          = 65535
	defaultVolumeFSType         = "ext4"
	minAutoVolumeSize           = units.MiB(256)
)

var writeFileModePattern = regexp.MustCompile(`^0?[0-7]{3}$`)

func (m *Manifest) applyDefaults() {
	if m == nil {
		return
	}

	if m.QEMU.SSHReady.SocketPath == "" {
		m.QEMU.SSHReady.SocketPath = defaultSSHReadySocket
	}

	if m.VSock.CIDRange.Start == 0 {
		m.VSock.CIDRange.Start = defaultVSockCIDStart
	}
	if m.VSock.CIDRange.End == 0 {
		m.VSock.CIDRange.End = defaultVSockCIDEnd
	}

	for i := range m.Volumes {
		if m.Volumes[i].FSType == "" {
			m.Volumes[i].FSType = defaultVolumeFSType
		}
	}

	applyBalloonDefaults(m.QEMU.Memory.Size, m.QEMU.Devices.Balloon)
}

func (m *Manifest) Validate() error {
	m.applyDefaults()

	switch {
	case m == nil:
		return fmt.Errorf("manifest is nil")
	case m.Identity.HostName == "":
		return fmt.Errorf("manifest.identity.hostName is required")
	case m.Paths.WorkingDir == "":
		return fmt.Errorf("manifest.paths.workingDir is required")
	case m.Paths.LockPath == "":
		return fmt.Errorf("manifest.paths.lockPath is required")
	case m.SSH.User == "":
		return fmt.Errorf("manifest.ssh.user is required")
	case m.SSH.RetryDelay < 0:
		return fmt.Errorf("manifest.ssh.retryDelay must be a finite number greater than or equal to zero")
	case m.QEMU.BinaryPath == "":
		return fmt.Errorf("manifest.qemu.binaryPath is required")
	case m.QEMU.QMP.SocketPath == "":
		return fmt.Errorf("manifest.qemu.qmp.socketPath is required")
	case len(m.WriteFiles) > 0 && m.QEMU.GuestAgent.SocketPath == "":
		return fmt.Errorf("manifest.qemu.guestAgent.socketPath is required when manifest.writeFiles is set")
	case m.VSock.CIDRange.Start < defaultVSockCIDStart:
		return fmt.Errorf("manifest.vsock.cidRange.start must be at least %d", defaultVSockCIDStart)
	case m.VSock.CIDRange.End < m.VSock.CIDRange.Start:
		return fmt.Errorf("manifest.vsock.cidRange.end must be greater than or equal to start")
	case !validQEMUTransport(m.QEMU.Devices.RNG.Transport):
		return fmt.Errorf("manifest.qemu.devices.rng.transport must be one of pci, mmio, or ccw")
	case !validQEMUTransport(m.QEMU.Devices.VSOCK.Transport):
		return fmt.Errorf("manifest.qemu.devices.vsock.transport must be one of pci, mmio, or ccw")
	}

	if !m.QEMU.Graphics.IsZero() && !validQEMUGraphicsBackend(m.QEMU.Graphics.Backend) {
		return fmt.Errorf("manifest.qemu.graphics.backend must be one of gtk or cocoa")
	}

	for i, run := range m.Run {
		if err := validateRun(i, run); err != nil {
			return err
		}
	}
	if m.QEMU.Hotplug.PCIEPorts < 0 {
		return fmt.Errorf("manifest.qemu.hotplug.pciePorts must be greater than or equal to zero")
	}
	if m.QEMU.Hotplug.PCIEPorts < len(m.Hotplug) {
		return fmt.Errorf("manifest.qemu.hotplug.pciePorts must be at least manifest.hotplug length")
	}
	if m.QEMU.Hotplug.PCIEPorts > 0 && m.QEMU.Devices.RNG.Transport != "pci" {
		return fmt.Errorf("manifest.qemu.hotplug.pciePorts requires pci transport")
	}
	hotplugIDs := make(map[string]int, len(m.Hotplug))
	for i, hotplug := range m.Hotplug {
		if err := validateHotplug(i, hotplug); err != nil {
			return err
		}
		if previous, ok := hotplugIDs[hotplug.ID]; ok {
			return fmt.Errorf("manifest.hotplug[%d].id duplicates manifest.hotplug[%d].id %q", i, previous, hotplug.ID)
		}
		hotplugIDs[hotplug.ID] = i
	}
	for i, path := range m.CleanupFiles {
		if path == "" {
			return fmt.Errorf("manifest.cleanupFiles[%d] must not be empty", i)
		}
	}

	for i, share := range m.QEMU.Devices.VirtioFS {
		switch {
		case share.SocketPath == "":
			return fmt.Errorf("manifest.qemu.devices.virtiofs[%d].socketPath is required", i)
		case !validQEMUTransport(share.Transport):
			return fmt.Errorf("manifest.qemu.devices.virtiofs[%d].transport must be one of pci, mmio, or ccw", i)
		}
	}

	for i, share := range m.QEMU.Devices.NineP {
		if !validQEMUTransport(share.Transport) {
			return fmt.Errorf("manifest.qemu.devices.9p[%d].transport must be one of pci, mmio, or ccw", i)
		}
	}

	for i, block := range m.QEMU.Devices.Block {
		if !validQEMUTransport(block.Transport) {
			return fmt.Errorf("manifest.qemu.devices.block[%d].transport must be one of pci, mmio, or ccw", i)
		}
		if block.Format != "" && !validImageFormat(block.Format) {
			return fmt.Errorf("manifest.qemu.devices.block[%d].format must be raw or qcow2", i)
		}
	}

	for i, mount := range m.QEMU.Devices.Mounts {
		if mount.Block != nil && mount.Block.Format != "" && !validImageFormat(mount.Block.Format) {
			return fmt.Errorf("manifest.qemu.devices.mounts[%d].block.format must be raw or qcow2", i)
		}
	}

	for i, netdev := range m.QEMU.Devices.Network {
		if !validQEMUTransport(netdev.Transport) {
			return fmt.Errorf("manifest.qemu.devices.network[%d].transport must be one of pci, mmio, or ccw", i)
		}
	}

	if err := validateBalloonDevice(m.QEMU.Memory.Size, m.QEMU.Devices.Balloon); err != nil {
		return err
	}

	if err := validateWriteFiles(m.WriteFiles); err != nil {
		return err
	}
	for i, volume := range m.Volumes {
		if volume.AutoCreate && volume.ImagePath == "" {
			return fmt.Errorf("manifest.mounts.image[%d].source is required", i)
		}
		if volume.AutoCreate && volume.Size <= 0 {
			return fmt.Errorf("manifest.mounts.image[%d].image.size must be greater than zero when image.create is true", i)
		}
		if volume.AutoCreate && volume.Size < minAutoVolumeSize {
			return fmt.Errorf("manifest.mounts.image[%d].image.size must be at least %d when image.create is true", i, minAutoVolumeSize)
		}
		if volume.AutoCreate && volume.FSType != defaultVolumeFSType {
			return fmt.Errorf("manifest.mounts.image[%d].image.fs must be %q when image.create is true", i, defaultVolumeFSType)
		}
		if volume.AutoCreate && len(volume.MkfsExtraArgs) > 0 {
			return fmt.Errorf("manifest.mounts.image[%d].image.mkfs_extra_args is not supported when image.create is true", i)
		}
	}

	return nil
}

func validateHotplug(index int, device hotplugpkg.Device) error {
	if device.ID == "" {
		return fmt.Errorf("manifest.hotplug[%d].id is required", index)
	}
	if strings.ContainsAny(device.ID, `/\`) {
		return fmt.Errorf("manifest.hotplug[%d].id must not contain path separators", index)
	}
	switch device.Kind {
	case hotplugpkg.KindVirtioFS:
		if device.VirtioFS.Source == "" {
			return fmt.Errorf("manifest.hotplug[%d].virtiofs.source is required", index)
		}
		if device.VirtioFS.SocketPath == "" {
			return fmt.Errorf("manifest.hotplug[%d].virtiofs.socket is required", index)
		}
		if device.VirtioFS.Bin == "" {
			return fmt.Errorf("manifest.hotplug[%d].virtiofs.bin is required", index)
		}
	case hotplugpkg.KindNet:
		if device.Net.Backend != "user" {
			return fmt.Errorf("manifest.hotplug[%d].net.backend must be user", index)
		}
		if device.Net.MAC == "" {
			return fmt.Errorf("manifest.hotplug[%d].net.mac is required", index)
		}
	case hotplugpkg.KindBlock:
		if device.Block.ImagePath == "" {
			return fmt.Errorf("manifest.hotplug[%d].block.image is required", index)
		}
		if !validImageFormat(device.Block.Format) {
			return fmt.Errorf("manifest.hotplug[%d].block.format must be raw or qcow2", index)
		}
	default:
		return fmt.Errorf("manifest.hotplug[%d].kind is required", index)
	}
	return nil
}

func validateRun(index int, run Run) error {
	switch {
	case len(run.Exec) == 0:
		return fmt.Errorf("manifest.run[%d].exec is required", index)
	case run.Exec[0] == "":
		return fmt.Errorf("manifest.run[%d].exec[0] is required", index)
	}
	if err := validateRunTemplates(index, "exec", run.Exec); err != nil {
		return err
	}
	if err := validateRunTemplates(index, "env", run.Env); err != nil {
		return err
	}
	for key := range run.Vars {
		if key == "CID" || key == "StateDir" || key == "Workspace" || key == "Env" {
			return fmt.Errorf("manifest.run[%d].vars key %q is reserved", index, key)
		}
	}
	if _, err := NewTemplateRenderer(RunTemplateProvider{
		CID:      3,
		StateDir: ".virtle",
		Workspace: Workspace{
			GuestDir: "/workspace",
			HostDir:  "/host/workspace",
		},
		Vars: run.Vars,
	}); err != nil {
		return fmt.Errorf("manifest.run[%d].vars: %w", index, err)
	}
	return nil
}

func validateRunTemplates(index int, field string, values []string) error {
	for i, value := range values {
		tmpl, err := template.New("exec").Parse(value)
		if err != nil {
			continue
		}
		if templateUsesBareWorkspace(tmpl.Tree.Root) {
			return fmt.Errorf("manifest.run[%d].%s[%d] uses {{.Workspace}}; use {{.Workspace.GuestPath}} or {{.Workspace.HostPath}}", index, field, i)
		}
	}
	return nil
}

func templateUsesBareWorkspace(node parse.Node) bool {
	switch node := node.(type) {
	case nil:
		return false
	case *parse.ListNode:
		for _, child := range node.Nodes {
			if templateUsesBareWorkspace(child) {
				return true
			}
		}
	case *parse.ActionNode:
		return templateUsesBareWorkspace(node.Pipe)
	case *parse.IfNode:
		return templateUsesBareWorkspace(node.Pipe) || templateUsesBareWorkspace(node.List) || templateUsesBareWorkspace(node.ElseList)
	case *parse.RangeNode:
		return templateUsesBareWorkspace(node.Pipe) || templateUsesBareWorkspace(node.List) || templateUsesBareWorkspace(node.ElseList)
	case *parse.WithNode:
		return templateUsesBareWorkspace(node.Pipe) || templateUsesBareWorkspace(node.List) || templateUsesBareWorkspace(node.ElseList)
	case *parse.PipeNode:
		for _, command := range node.Cmds {
			if templateUsesBareWorkspace(command) {
				return true
			}
		}
	case *parse.CommandNode:
		for _, arg := range node.Args {
			if templateUsesBareWorkspace(arg) {
				return true
			}
		}
	case *parse.FieldNode:
		return len(node.Ident) == 1 && node.Ident[0] == "Workspace"
	case *parse.VariableNode:
		return len(node.Ident) == 1 && node.Ident[0] == "Workspace"
	}
	return false
}

func pathEscapesBase(path string) bool {
	cleaned := filepath.Clean(path)
	return cleaned == ".." || strings.HasPrefix(cleaned, ".."+string(filepath.Separator))
}

func (m *Manifest) SSHRetryDelay(fallback time.Duration) time.Duration {
	if m == nil {
		return fallback
	}
	return m.SSH.RetryDelay
}

func validateWriteFiles(files WriteFiles) error {
	paths := make([]string, 0, len(files))
	for guestPath := range files {
		paths = append(paths, guestPath)
	}
	sort.Strings(paths)

	for _, guestPath := range paths {
		entry := files[guestPath]
		switch {
		case guestPath == "":
			return fmt.Errorf("manifest.writeFiles contains an empty guest path")
		case !filepath.IsAbs(guestPath):
			return fmt.Errorf("manifest.writeFiles[%q] guest path must be absolute", guestPath)
		case entry.Content.Kind == WriteFileContentNone:
			return fmt.Errorf("manifest.writeFiles[%q] must set exactly one of text or path", guestPath)
		case entry.Content.Kind != WriteFileContentText && entry.Content.Kind != WriteFileContentPath:
			return fmt.Errorf("manifest.writeFiles[%q] must set exactly one of text or path", guestPath)
		case entry.Content.Kind == WriteFileContentPath && entry.Content.Path == "":
			return fmt.Errorf("manifest.writeFiles[%q].path must not be empty", guestPath)
		case entry.WriteBack && entry.Content.Kind != WriteFileContentPath:
			return fmt.Errorf("manifest.writeFiles[%q].writeBack requires path", guestPath)
		case entry.Mode != "" && !writeFileModePattern.MatchString(entry.Mode):
			return fmt.Errorf("manifest.writeFiles[%q].mode must match ^0?[0-7]{3}$", guestPath)
		}
	}
	return nil
}

func validQEMUTransport(transport string) bool {
	switch transport {
	case "pci", "mmio", "ccw":
		return true
	default:
		return false
	}
}

func validQEMUGraphicsBackend(backend string) bool {
	switch backend {
	case "gtk", "cocoa":
		return true
	default:
		return false
	}
}

func validImageFormat(format string) bool {
	switch format {
	case "raw", "qcow2":
		return true
	default:
		return false
	}
}
