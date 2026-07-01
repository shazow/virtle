package manager

import (
	"context"
	"os"

	"github.com/shazow/virtle/internal/executor"
	"github.com/shazow/virtle/internal/manager/launch"
	"github.com/shazow/virtle/internal/manifest"
	"github.com/shazow/virtle/internal/qga"
	"github.com/shazow/virtle/internal/qmpclient"
)

type ResumeMode = launch.ResumeMode

const (
	ResumeModeNo    = launch.ResumeModeNo
	ResumeModeAuto  = launch.ResumeModeAuto
	ResumeModeForce = launch.ResumeModeForce
)

type LaunchOptions = launch.Options

type WaitMode = launch.WaitMode

const (
	WaitAuto = launch.WaitAuto
	WaitSSH  = launch.WaitSSH
	WaitVM   = launch.WaitVM
)

type Launcher struct {
	manager *manager
}

func DefaultConfig() Config {
	return Config{
		Locker:              &fileLocker{},
		VSockCIDChecker:     newHostVSockCIDChecker(),
		Runner:              &executor.Runner{},
		SocketWaiter:        &pollingSocketWaiter{},
		QMPDialer:           &qmpclient.SocketMonitorDialer{},
		GuestAgentDialer:    &qga.SocketDialer{},
		SSHReadyDialer:      &unixSSHReadyDialer{},
		Logger:              logger,
		LogWriter:           os.Stderr,
		SSHRetryDelay:       defaultSSHRetryDelay,
		SSHReadyTimeout:     configuredSSHReadyTimeout(),
		ShutdownDelay:       defaultShutdownDelay,
		QMPRetryDelay:       defaultQMPRetryDelay,
		QMPConnectTimeout:   defaultQMPConnectTimeout,
		QMPQuitTimeout:      defaultQMPQuitTimeout,
		QMPMigrationTimeout: defaultQMPMigrationTimeout,
	}
}

func NewLauncher(configs ...Config) *Launcher {
	config := DefaultConfig()
	if len(configs) > 0 {
		config = mergeConfig(config, configs[0])
	}
	return &Launcher{manager: newManagerFromConfig(config)}
}

// Launch runs the supported virtle sandbox session.
func Launch(ctx context.Context, manifest *manifest.Manifest, remoteCommand []string) error {
	return NewLauncher().launch(ctx, manifest, remoteCommand)
}

// LaunchWithOptions runs the supported virtle sandbox session with explicit launch options.
func LaunchWithOptions(ctx context.Context, manifest *manifest.Manifest, remoteCommand []string, options LaunchOptions) error {
	return NewLauncher().launchWithOptions(ctx, manifest, remoteCommand, options)
}

func (l *Launcher) launch(ctx context.Context, manifest *manifest.Manifest, remoteCommand []string) (err error) {
	return l.launchWithOptions(ctx, manifest, remoteCommand, launch.Options{Resume: launch.ResumeModeNo, SSH: true})
}

func (l *Launcher) launchWithOptions(ctx context.Context, manifest *manifest.Manifest, remoteCommand []string, options launch.Options) error {
	if l == nil || l.manager == nil {
		l = NewLauncher()
	}
	plan, err := l.manager.planLaunch(launch.Spec{Manifest: manifest, RemoteCommand: remoteCommand, Options: options})
	if err != nil {
		return err
	}
	return l.manager.launchWithPlan(ctx, plan)
}
