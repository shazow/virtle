package vmm

import (
	"context"
	"fmt"
	"time"

	"github.com/shazow/virtle/backend/qemu/internal/launch"
	"github.com/shazow/virtle/backend/qemu/internal/qga"
	"github.com/shazow/virtle/internal/executor"
	"github.com/shazow/virtle/internal/manifest"
)

const (
	guestShutdownResponseTimeout = time.Second
	// guestShutdownExecWait bounds how long the shutdown command's exit status
	// is polled; running out of time means the request was issued and QEMU
	// exit is awaited.
	guestShutdownExecWait      = 3 * time.Second
	guestShutdownExecPollDelay = 250 * time.Millisecond
)

// requestGuestShutdown asks the guest to power down through the guest agent,
// either with the guest-shutdown command or a configured shutdown_exec guest
// command. QEMU exit is the authoritative completion signal; this only issues
// the request.
func (m *manager) requestGuestShutdown(ctx context.Context, socketPath string, exec []string) error {
	dialer := m.guestAgentDialer
	if dialer == nil {
		dialer = &qga.SocketDialer{}
	}
	client, err := dialer.Dial(ctx, socketPath, m.effectiveQMPConnectTimeout())
	if err != nil {
		return fmt.Errorf("connect guest agent: %w", err)
	}
	defer client.Disconnect()

	// The chardev accepts connections even when no agent runs in the guest, so
	// probe with a ping first: a guest that cannot answer (agent missing, or
	// paused after a suspend save) cannot process a shutdown request either,
	// and treating that silence as success would burn the full shutdown wait.
	pingCtx, cancelPing := context.WithTimeout(ctx, guestShutdownResponseTimeout)
	err = client.Ping(pingCtx)
	cancelPing()
	if err != nil {
		return fmt.Errorf("guest agent unavailable: %w", err)
	}

	if len(exec) == 0 {
		// guest-shutdown rarely answers before the guest powers off; cap the
		// wait for its response instead of holding out for a full RPC timeout.
		ctx, cancel := context.WithTimeout(ctx, guestShutdownResponseTimeout)
		defer cancel()
		return client.Shutdown(ctx)
	}

	ctx, cancel := m.launchManifest.GuestCommandContext(ctx)
	defer cancel()
	pid, err := client.Exec(ctx, exec[0], exec[1:], nil, true)
	if err != nil {
		return fmt.Errorf("execute guest shutdown command %v: %w", exec, err)
	}
	// Poll the exit status briefly so a fast-failing command is reported
	// instead of silently burning the shutdown wait. Losing QGA or running out
	// of polling time is expected while the guest powers off; QEMU exit is the
	// authoritative completion signal.
	statusCtx, cancelStatus := context.WithTimeout(ctx, guestShutdownExecWait)
	defer cancelStatus()
	ticker := time.NewTicker(guestShutdownExecPollDelay)
	defer ticker.Stop()
	for {
		status, err := client.ExecStatus(statusCtx, pid)
		if err != nil {
			return nil
		}
		if status.Exited {
			if status.ExitCode != 0 {
				return fmt.Errorf("guest shutdown command %v exited with status %d%s", exec, status.ExitCode, qga.ExecOutputSuffix(status))
			}
			return nil
		}
		select {
		case <-statusCtx.Done():
			return nil
		case <-ticker.C:
		}
	}
}

func (m *manager) writeGuestFiles(ctx context.Context, stats *launch.Stats, watchers executor.Group) error {
	launchManifest := m.launchManifest
	files := launchManifest.ResolvedWriteFiles()
	mountCWD := launchManifest.Workspace.MountCWD
	if len(files) == 0 && !mountCWD {
		return nil
	}

	socketPath, err := launchManifest.ResolvedGuestAgentSocketPath()
	if err != nil {
		return &launch.StageError{Stage: "guest agent", Err: err}
	}

	m.logger.Info("waiting for guest agent readiness")
	client, err := m.waitForGuestAgent(ctx, socketPath, watchers)
	if err != nil {
		return err
	}
	if stats != nil {
		stats.Timer(launch.TimerGuestAgentReady, time.Now())
	}
	defer client.Disconnect()

	if mountCWD {
		if err := m.mountWorkspaceCWD(ctx, client); err != nil {
			return err
		}
	}

	return launch.WriteGuestFiles(ctx, files, launch.GuestFileWriter{
		PathExists: func(ctx context.Context, guestPath string) (bool, error) {
			return m.guestPathExists(ctx, client, guestPath)
		},
		InstallDirectory: func(ctx context.Context, file manifest.ResolvedWriteFile) error {
			return m.installGuestFileDirectory(ctx, client, file.GuestPath, file.Chown, file.Mode)
		},
		WriteFile: func(ctx context.Context, guestPath string, payloadBase64 string) error {
			return m.writeGuestFile(ctx, client, guestPath, payloadBase64)
		},
		Chown: func(ctx context.Context, guestPath string, owner string) error {
			return m.chownGuestFile(ctx, client, guestPath, owner)
		},
		Chmod: func(ctx context.Context, guestPath string, mode string) error {
			return m.chmodGuestFile(ctx, client, guestPath, mode)
		},
		SkipExisting: func(guestPath string) {
			m.logger.Info("skipped existing guest file because overwrite is false", "path", guestPath)
		},
		Wrote: func(guestPath string) {
			m.logger.Info("wrote guest file", "path", guestPath)
		},
	})
}

func (m *manager) writeBackGuestFiles(ctx context.Context, watchers executor.Group) error {
	launchManifest := m.launchManifest
	writeBackFiles := launch.GuestWriteBackFiles(launchManifest.ResolvedWriteFiles())
	if len(writeBackFiles) == 0 {
		return nil
	}

	socketPath, err := launchManifest.ResolvedGuestAgentSocketPath()
	if err != nil {
		return &launch.StageError{Stage: "guest file write-back", Err: err}
	}

	m.logger.Info("waiting for guest agent readiness for write-back")
	client, err := m.waitForGuestAgentStage(ctx, "guest file write-back", socketPath, watchers)
	if err != nil {
		return err
	}
	defer client.Disconnect()

	return launch.WriteBackGuestFiles(ctx, writeBackFiles, launch.GuestFileWriteBacker{
		ReadFile: func(ctx context.Context, guestPath string) ([]byte, error) {
			return m.readGuestFile(ctx, client, guestPath)
		},
		WriteHostFile: launch.WriteHostFileAtomic,
		Wrote: func(guestPath string, hostPath string) {
			m.logger.Info("wrote guest file back to host", "guest_path", guestPath, "host_path", hostPath)
		},
	})
}

const (
	guestChmodPath   = "chmod"
	guestChownPath   = "chown"
	guestInstallPath = "install"
	guestMountPath   = "mount"
	guestPSPath      = "ps"
	guestTestPath    = "test"

	// Default PATH to use to find the commands above, should cover common distros like busybox/alpine, debian/ubuntu, nixos/guix, etc.
	// TODO: Add some way for users to bring their own path, or better yet: preload guest's default PATH and prefix it here.
	guestInternalCommandPathEnv = "PATH=/bin:/usr/bin:/run/current-system/sw/bin:/run/current-system/profile/bin"
)

func (m *manager) mountWorkspaceCWD(ctx context.Context, client qga.Client) error {
	launchManifest := m.launchManifest
	return launch.MountWorkspaceCWD(ctx, launchManifest, launch.WorkspaceCWDMounter{
		InstallDir: func(ctx context.Context, target string, args []string) error {
			return m.runGuestFileCommand(ctx, client, "install -d", guestInstallPath, args, target)
		},
		MountBind: func(ctx context.Context, source string, target string, args []string) error {
			return m.runGuestFileCommand(ctx, client, "mount --bind", guestMountPath, args, target)
		},
		Mounted: func(source string, target string) {
			m.logger.Info("mounted workspace cwd", "source", source, "target", target)
		},
	})
}

// installGuestFileDirectory ensures that the parent directory for guestPath
// exists. owner and mode are applied to newly created directories only;
// existing directories are left unchanged. mode is expected to be a file mode
// and is converted to a directory mode by adding execute bits wherever read
// bits are set. How the directories are created inside the guest is up to the
// installer; the manager only supplies the guest command transport.
func (m *manager) installGuestFileDirectory(ctx context.Context, client qga.Client, guestPath string, owner string, mode string) error {
	installer := launch.ScriptGuestDirectoryInstaller(func(ctx context.Context, guestDir string, path string, args []string) error {
		return m.runGuestFileCommand(ctx, client, "install dirs", path, args, guestDir)
	})
	return launch.InstallGuestFileDirectory(ctx, installer, guestPath, owner, mode)
}

func (m *manager) guestPathExists(ctx context.Context, client qga.Client, guestPath string) (bool, error) {
	status, err := m.runGuestCommandStatus(ctx, client, "test -e", guestTestPath, []string{"-e", guestPath}, []string{guestInternalCommandPathEnv}, guestPath)
	if err != nil {
		return false, err
	}
	return status.ExitCode == 0, nil
}

// writeGuestFile writes one guest file under the manifest's guest command
// bound.
func (m *manager) writeGuestFile(ctx context.Context, client qga.Client, guestPath string, payloadBase64 string) error {
	ctx, cancel := m.launchManifest.GuestCommandContext(ctx)
	defer cancel()
	return qga.WriteFile(ctx, client, guestPath, payloadBase64)
}

// readGuestFile reads one guest file under the manifest's guest command bound.
func (m *manager) readGuestFile(ctx context.Context, client qga.Client, guestPath string) ([]byte, error) {
	ctx, cancel := m.launchManifest.GuestCommandContext(ctx)
	defer cancel()
	return qga.ReadFile(ctx, client, guestPath, qga.DefaultFileReadChunkSize)
}

func (m *manager) chownGuestFile(ctx context.Context, client qga.Client, guestPath string, owner string) error {
	return m.runGuestFileCommand(ctx, client, "chown", guestChownPath, []string{owner, guestPath}, guestPath)
}

func (m *manager) chmodGuestFile(ctx context.Context, client qga.Client, guestPath string, mode string) error {
	return m.runGuestFileCommand(ctx, client, "chmod", guestChmodPath, []string{mode, guestPath}, guestPath)
}

func (m *manager) runGuestFileCommand(ctx context.Context, client qga.Client, name string, path string, args []string, guestPath string) error {
	status, err := m.runGuestCommandStatus(ctx, client, name, path, args, []string{guestInternalCommandPathEnv}, guestPath)
	if err != nil {
		return err
	}
	if status.ExitCode != 0 {
		return fmt.Errorf("%s %q exited with status %d%s", name, guestPath, status.ExitCode, qga.ExecOutputSuffix(status))
	}
	return nil
}

// runGuestCommandStatus runs one guest command under the manifest's guest
// command bound; call sites must not re-wrap.
func (m *manager) runGuestCommandStatus(ctx context.Context, client qga.Client, name string, path string, args []string, env []string, subject string) (qga.ExecStatus, error) {
	ctx, cancel := m.launchManifest.GuestCommandContext(ctx)
	defer cancel()
	return qga.RunCommandStatus(ctx, client, qga.ExecWait{
		Name:          name,
		Path:          path,
		Args:          args,
		Env:           env,
		Subject:       subject,
		CaptureOutput: true,
	})
}

func (m *manager) waitForGuestAgent(ctx context.Context, socketPath string, watchers executor.Group) (qga.Client, error) {
	return m.waitForGuestAgentStage(ctx, "guest agent", socketPath, watchers)
}

func (m *manager) waitForGuestAgentStage(ctx context.Context, stage string, socketPath string, watchers executor.Group) (qga.Client, error) {
	dialer := m.guestAgentDialer
	if dialer == nil {
		dialer = &qga.SocketDialer{}
	}
	retryDelay := m.qmpRetryDelay
	if retryDelay <= 0 {
		retryDelay = defaultQMPRetryDelay
	}
	return launch.WaitForGuestAgent(ctx, launch.GuestAgentWait{
		Stage:          stage,
		SocketPath:     socketPath,
		SocketWaiter:   m.socketWaiter,
		Dialer:         dialer,
		ConnectTimeout: m.effectiveQMPConnectTimeout(),
		RetryDelay:     retryDelay,
		PollDelay:      defaultSocketPollInterval,
		Watchers:       watchers,
	})
}
