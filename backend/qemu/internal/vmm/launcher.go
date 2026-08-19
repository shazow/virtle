package vmm

import (
	"io"

	"github.com/shazow/virtle/backend/qemu/internal/launch"
	"github.com/shazow/virtle/backend/qemu/internal/qga"
	"github.com/shazow/virtle/backend/qemu/internal/qmpclient"
	"github.com/shazow/virtle/internal/executor"
)

type ResumeMode = launch.ResumeMode

const (
	ResumeModeNo    = launch.ResumeModeNo
	ResumeModeAuto  = launch.ResumeModeAuto
	ResumeModeForce = launch.ResumeModeForce
)

type LaunchOptions = launch.Options

func DefaultConfig() Config {
	return Config{
		Locker:              &fileLocker{},
		VSockCIDChecker:     newHostVSockCIDChecker(),
		Runner:              &executor.Runner{},
		SocketWaiter:        &pollingSocketWaiter{},
		QMPDialer:           &qmpclient.SocketMonitorDialer{},
		GuestAgentDialer:    &qga.SocketDialer{},
		SSHReadyDialer:      &unixSSHReadyDialer{},
		Logger:              discardLogger,
		ConsoleOutput:       io.Discard,
		SSHOutput:           io.Discard,
		SSHError:            io.Discard,
		BackgroundOutput:    io.Discard,
		SSHReadyTimeout:     configuredSSHReadyTimeout(),
		ShutdownDelay:       defaultShutdownDelay,
		QMPRetryDelay:       defaultQMPRetryDelay,
		QMPConnectTimeout:   defaultQMPConnectTimeout,
		QMPQuitTimeout:      defaultQMPQuitTimeout,
		QMPMigrationTimeout: defaultQMPMigrationTimeout,
	}
}
