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
	config *Config
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
	return &Launcher{config: &config}
}

// LaunchWithOptions runs the supported virtle sandbox session with explicit launch options.
func LaunchWithOptions(ctx context.Context, manifest *manifest.Manifest, remoteCommand []string, options LaunchOptions) error {
	return NewLauncher().launchWithOptions(ctx, manifest, remoteCommand, options)
}

func (l *Launcher) launchWithOptions(ctx context.Context, manifest *manifest.Manifest, remoteCommand []string, options launch.Options) error {
	if l == nil || l.config == nil {
		l = NewLauncher()
	}
	return newManagerFromConfig(*l.config).launchWithOptions(ctx, manifest, remoteCommand, options)
}
