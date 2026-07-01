package manager

import (
	"context"
	"fmt"
	"time"

	"github.com/shazow/virtle/internal/executor"
	"github.com/shazow/virtle/internal/manager/launch"
	"github.com/shazow/virtle/internal/manifest"
	"github.com/shazow/virtle/internal/qga"
)

func (m *manager) writeGuestFiles(ctx context.Context, launchManifest *manifest.Manifest, stats *launch.Stats, watchers executor.Group) error {
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
		if err := m.mountWorkspaceCWD(ctx, client, launchManifest); err != nil {
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
		WriteFile: func(_ context.Context, guestPath string, payloadBase64 string) error {
			return m.writeGuestFile(client, guestPath, payloadBase64)
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

func (m *manager) writeBackGuestFiles(ctx context.Context, launchManifest *manifest.Manifest, watchers executor.Group) error {
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
		ReadFile: func(_ context.Context, guestPath string) ([]byte, error) {
			return m.readGuestFile(client, guestPath)
		},
		WriteHostFile: launch.WriteHostFileAtomic,
		Wrote: func(guestPath string, hostPath string) {
			m.logger.Info("wrote guest file back to host", "guest_path", guestPath, "host_path", hostPath)
		},
	})
}

func (m *manager) writeGuestFile(client qga.Client, guestPath string, payloadBase64 string) error {
	return qga.WriteFile(client, m.effectiveQMPCommandTimeout(), guestPath, payloadBase64)
}

func (m *manager) readGuestFile(client qga.Client, guestPath string) ([]byte, error) {
	return qga.ReadFile(client, m.effectiveQMPCommandTimeout(), guestPath, qga.DefaultFileReadChunkSize)
}

const (
	guestChmodPath   = "/run/current-system/sw/bin/chmod"
	guestChownPath   = "/run/current-system/sw/bin/chown"
	guestInstallPath = "/run/current-system/sw/bin/install"
	guestMountPath   = "/run/current-system/sw/bin/mount"
	guestPSPath      = "/run/current-system/sw/bin/ps"
	guestTestPath    = "/run/current-system/sw/bin/test"
)

func (m *manager) mountWorkspaceCWD(ctx context.Context, client qga.Client, launchManifest *manifest.Manifest) error {
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

// installGuestFileDirectory ensures that the parent directory for guestPath exists.
// It walks upward until it finds an existing ancestor, then creates only the
// missing directories from top to bottom. owner and mode are passed to install(1)
// for newly-created directories only; existing directories are left unchanged.
// mode is expected to be a file mode and is converted to a directory mode by
// adding execute bits wherever read bits are set.
func (m *manager) installGuestFileDirectory(ctx context.Context, client qga.Client, guestPath string, owner string, mode string) error {
	return launch.InstallGuestFileDirectory(ctx, launch.GuestDirectoryInstaller{
		Exists: func(ctx context.Context, guestDir string) (bool, error) {
			return m.guestDirectoryExists(ctx, client, guestDir)
		},
		Install: func(ctx context.Context, guestDir string, args []string) error {
			return m.runGuestFileCommand(ctx, client, "install -d", guestInstallPath, args, guestDir)
		},
	}, guestPath, owner, mode)
}

func (m *manager) guestDirectoryExists(ctx context.Context, client qga.Client, guestDir string) (bool, error) {
	status, err := m.runGuestFileCommandStatus(ctx, client, "test -d", guestTestPath, []string{"-d", guestDir}, guestDir)
	if err != nil {
		return false, err
	}
	return status.ExitCode == 0, nil
}

func (m *manager) guestPathExists(ctx context.Context, client qga.Client, guestPath string) (bool, error) {
	status, err := m.runGuestFileCommandStatus(ctx, client, "test -e", guestTestPath, []string{"-e", guestPath}, guestPath)
	if err != nil {
		return false, err
	}
	return status.ExitCode == 0, nil
}

func (m *manager) chownGuestFile(ctx context.Context, client qga.Client, guestPath string, owner string) error {
	return m.runGuestFileCommand(ctx, client, "chown", guestChownPath, []string{owner, guestPath}, guestPath)
}

func (m *manager) chmodGuestFile(ctx context.Context, client qga.Client, guestPath string, mode string) error {
	return m.runGuestFileCommand(ctx, client, "chmod", guestChmodPath, []string{mode, guestPath}, guestPath)
}

func (m *manager) runGuestFileCommand(ctx context.Context, client qga.Client, name string, path string, args []string, guestPath string) error {
	status, err := m.runGuestFileCommandStatus(ctx, client, name, path, args, guestPath)
	if err != nil {
		return err
	}
	if status.ExitCode != 0 {
		return fmt.Errorf("%s %q exited with status %d%s", name, guestPath, status.ExitCode, qga.ExecOutputSuffix(status))
	}
	return nil
}

func (m *manager) runGuestFileCommandStatus(ctx context.Context, client qga.Client, name string, path string, args []string, guestPath string) (qga.ExecStatus, error) {
	return m.runGuestCommandStatus(ctx, client, name, path, args, guestPath)
}

func (m *manager) runGuestCommandStatus(ctx context.Context, client qga.Client, name string, path string, args []string, subject string) (qga.ExecStatus, error) {
	return qga.RunCommandStatus(ctx, client, qga.ExecWait{
		Timeout:       m.effectiveQMPCommandTimeout(),
		PollDelay:     defaultMigrationPollDelay,
		Name:          name,
		Path:          path,
		Args:          args,
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
		CommandTimeout: m.effectiveQMPCommandTimeout(),
		RetryDelay:     retryDelay,
		PollDelay:      defaultSocketPollInterval,
		Watchers:       watchers,
	})
}
