package manifest

import (
	"fmt"
	"path/filepath"
	"sort"

	"github.com/adrg/xdg"
)

func (m *Manifest) resolvePath(path string) string {
	if path == "" || filepath.IsAbs(path) {
		return path
	}

	return filepath.Join(m.Paths.WorkingDir, path)
}

func (m *Manifest) ResolvedPersistenceDirectories() []string {
	dirs := make([]string, 0, len(m.Persistence.Directories))
	for _, dir := range m.Persistence.Directories {
		dirs = append(dirs, m.resolvePath(dir))
	}
	return dirs
}

func (m *Manifest) ResolvedPersistenceBaseDir() string {
	if m.Persistence.BaseDir == "" {
		return m.resolvePath(".")
	}
	return m.resolvePath(m.Persistence.BaseDir)
}

func (m *Manifest) ResolvedPersistenceStateDir() string {
	if m.Persistence.StateDir != "" {
		return m.resolvePath(m.Persistence.StateDir)
	}
	return m.ResolvedPersistenceBaseDir()
}

func (m *Manifest) resolveSocketPath(path string) (string, error) {
	if path == "" || filepath.IsAbs(path) {
		return path, nil
	}

	switch m.Paths.RuntimeDir.Mode {
	case RuntimeDirWorking:
		return m.resolvePath(path), nil
	case RuntimeDirXDG:
		resolved, err := xdg.RuntimeFile(filepath.Join("agentspace", m.Identity.HostName, path))
		if err != nil {
			return "", fmt.Errorf("resolve runtime socket %q: %w", path, err)
		}
		return resolved, nil
	case RuntimeDirPath:
		return filepath.Join(m.resolvePath(m.Paths.RuntimeDir.Path), path), nil
	default:
		return "", fmt.Errorf("resolve runtime socket %q: invalid runtime dir mode %d", path, m.Paths.RuntimeDir.Mode)
	}
}

func (m *Manifest) ResolvedCleanupFiles() ([]string, error) {
	paths := make([]string, 0, len(m.CleanupFiles))
	for _, path := range m.CleanupFiles {
		resolved, err := m.resolveSocketPath(path)
		if err != nil {
			return nil, err
		}
		paths = append(paths, resolved)
	}
	return paths, nil
}

func (m *Manifest) ResolvedVirtioFSSocketPaths() ([]string, error) {
	paths := make([]string, 0, len(m.QEMU.Devices.VirtioFS))
	for _, share := range m.QEMU.Devices.VirtioFS {
		resolved, err := m.resolveSocketPath(share.SocketPath)
		if err != nil {
			return nil, err
		}
		paths = append(paths, resolved)
	}
	return paths, nil
}

func (m *Manifest) ResolvedExternalVirtioFSSocketPaths() ([]string, error) {
	cleanupFiles := make(map[string]struct{}, len(m.CleanupFiles))
	for _, path := range m.CleanupFiles {
		resolved, err := m.resolveSocketPath(path)
		if err != nil {
			return nil, err
		}
		cleanupFiles[resolved] = struct{}{}
	}

	paths := make([]string, 0, len(m.QEMU.Devices.VirtioFS))
	for _, share := range m.QEMU.Devices.VirtioFS {
		resolved, err := m.resolveSocketPath(share.SocketPath)
		if err != nil {
			return nil, err
		}
		if _, managed := cleanupFiles[resolved]; managed {
			continue
		}
		paths = append(paths, resolved)
	}
	return paths, nil
}

func (m *Manifest) ResolvedLockPath() string {
	return m.resolvePath(m.Paths.LockPath)
}

func (m *Manifest) ResolvedQMPSocketPath() (string, error) {
	return m.resolveSocketPath(m.QEMU.QMP.SocketPath)
}

func (m *Manifest) ResolvedGuestAgentSocketPath() (string, error) {
	return m.resolveSocketPath(m.QEMU.GuestAgent.SocketPath)
}

func (m *Manifest) ResolvedSSHReadySocketPath() (string, error) {
	return m.resolveSocketPath(m.QEMU.SSHReady.SocketPath)
}

func (m *Manifest) ResolvedControlSocketPath() (string, error) {
	return m.resolveSocketPath("virtle.sock")
}

func (m *Manifest) ResolvedQEMU() (QEMU, error) {
	resolved := m.QEMU
	resolved.Kernel.Path = m.resolvePath(resolved.Kernel.Path)
	resolved.Kernel.InitrdPath = m.resolvePath(resolved.Kernel.InitrdPath)
	resolved.PassthroughArgs = append([]string(nil), resolved.PassthroughArgs...)

	if err := m.resolveQEMUSockets(&resolved); err != nil {
		return QEMU{}, err
	}

	resolved.Machine.Options = append([]string(nil), resolved.Machine.Options...)

	if err := m.resolveQEMUDevices(&resolved.Devices); err != nil {
		return QEMU{}, err
	}

	return resolved, nil
}

func (m *Manifest) resolveQEMUSockets(resolved *QEMU) error {
	qmpSocketPath, err := m.resolveSocketPath(resolved.QMP.SocketPath)
	if err != nil {
		return err
	}
	resolved.QMP.SocketPath = qmpSocketPath

	guestAgentSocketPath, err := m.resolveSocketPath(resolved.GuestAgent.SocketPath)
	if err != nil {
		return err
	}
	resolved.GuestAgent.SocketPath = guestAgentSocketPath

	sshReadySocketPath, err := m.resolveSocketPath(resolved.SSHReady.SocketPath)
	if err != nil {
		return err
	}
	resolved.SSHReady.SocketPath = sshReadySocketPath
	return nil
}

func (m *Manifest) resolveQEMUDevices(devices *QEMUDevices) error {
	devices.VirtioFS = append([]QEMUVirtioFSShare(nil), devices.VirtioFS...)
	for i := range devices.VirtioFS {
		socketPath, err := m.resolveSocketPath(devices.VirtioFS[i].SocketPath)
		if err != nil {
			return err
		}
		devices.VirtioFS[i].SocketPath = socketPath
	}

	devices.NineP = append([]QEMUNinePShare(nil), devices.NineP...)
	for i := range devices.NineP {
		devices.NineP[i].SourcePath = m.resolvePath(devices.NineP[i].SourcePath)
	}

	devices.Block = append([]QEMUBlockDevice(nil), devices.Block...)
	for i := range devices.Block {
		devices.Block[i].ImagePath = m.resolvePath(devices.Block[i].ImagePath)
	}

	if err := m.resolveQEMUMounts(devices); err != nil {
		return err
	}

	devices.Network = append([]QEMUNetDevice(nil), devices.Network...)
	for i := range devices.Network {
		devices.Network[i].NetdevOptions = append([]string(nil), devices.Network[i].NetdevOptions...)
	}

	devices.Balloon = cloneBalloonDevice(devices.Balloon)
	return nil
}

func (m *Manifest) resolveQEMUMounts(devices *QEMUDevices) error {
	devices.Mounts = cloneQEMUMountDevices(devices.Mounts)
	for i := range devices.Mounts {
		mount := &devices.Mounts[i]
		if mount.VirtioFS != nil {
			socketPath, err := m.resolveSocketPath(mount.VirtioFS.SocketPath)
			if err != nil {
				return err
			}
			mount.VirtioFS.SocketPath = socketPath
		}
		if mount.NineP != nil {
			mount.NineP.SourcePath = m.resolvePath(mount.NineP.SourcePath)
		}
		if mount.Block != nil {
			mount.Block.ImagePath = m.resolvePath(mount.Block.ImagePath)
		}
	}
	return nil
}

func cloneQEMUMountDevices(mounts []QEMUMountDevice) []QEMUMountDevice {
	if len(mounts) == 0 {
		return nil
	}
	clone := make([]QEMUMountDevice, len(mounts))
	for i, mount := range mounts {
		clone[i] = mount
		if mount.VirtioFS != nil {
			share := *mount.VirtioFS
			clone[i].VirtioFS = &share
		}
		if mount.NineP != nil {
			share := *mount.NineP
			clone[i].NineP = &share
		}
		if mount.Block != nil {
			block := *mount.Block
			clone[i].Block = &block
		}
	}
	return clone
}

func (m *Manifest) ResolvedRuns(cid int) ([]ResolvedRun, error) {
	runs := make([]ResolvedRun, 0, len(m.Run))
	for i, run := range m.Run {
		renderer, err := NewTemplateRenderer(RunTemplateProvider{
			CID:       cid,
			StateDir:  m.ResolvedPersistenceStateDir(),
			Workspace: m.Workspace,
			Vars:      run.Vars,
		})
		if err != nil {
			return nil, fmt.Errorf("manifest.run[%d].exec: %w", i, err)
		}
		exec, err := renderer.RenderArgv(run.Exec)
		if err != nil {
			return nil, fmt.Errorf("manifest.run[%d].exec: %w", i, err)
		}
		env, err := renderer.RenderArgv(run.Env)
		if err != nil {
			return nil, fmt.Errorf("manifest.run[%d].env: %w", i, err)
		}
		env = append(env, renderer.Env()...)
		runs = append(runs, ResolvedRun{
			Exec: exec,
			Env:  env,
			Dir:  m.Paths.WorkingDir,
		})
	}
	return runs, nil
}

func (m *Manifest) ResolvedNotifications() Notifications {
	resolved := Notifications{
		States: append([]string(nil), m.Notifications.States...),
	}
	if !m.Notifications.Command.IsZero() {
		command := m.Notifications.Command
		command.Args = append([]string(nil), command.Args...)
		command.Env = append([]string(nil), command.Env...)
		resolved.Command = command
	}
	return resolved
}

func (m *Manifest) ResolvedVolumes() []Volume {
	volumes := make([]Volume, 0, len(m.Volumes))
	for _, volume := range m.Volumes {
		resolved := volume
		resolved.ImagePath = m.resolvePath(volume.ImagePath)
		resolved.MkfsExtraArgs = append([]string(nil), volume.MkfsExtraArgs...)
		volumes = append(volumes, resolved)
	}
	return volumes
}

func (m *Manifest) ResolvedWriteFiles() []ResolvedWriteFile {
	paths := make([]string, 0, len(m.WriteFiles))
	for guestPath := range m.WriteFiles {
		paths = append(paths, guestPath)
	}
	sort.Strings(paths)

	files := make([]ResolvedWriteFile, 0, len(paths))
	for _, guestPath := range paths {
		entry := m.WriteFiles[guestPath]
		resolved := ResolvedWriteFile{
			GuestPath:   guestPath,
			Chown:       entry.Chown,
			Mode:        entry.Mode,
			Overwrite:   entry.Overwrite,
			FollowLinks: entry.FollowLinks,
			WriteBack:   entry.WriteBack,
			Content:     entry.Content,
		}
		if entry.Content.Kind == WriteFileContentPath {
			resolved.Content.Path = m.resolvePath(entry.Content.Path)
		}
		files = append(files, resolved)
	}
	return files
}
