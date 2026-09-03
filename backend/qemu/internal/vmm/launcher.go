package vmm

import (
	"io"

	"github.com/shazow/virtle/backend/qemu/internal/launch"
	"github.com/shazow/virtle/backend/qemu/internal/qga"
	"github.com/shazow/virtle/backend/qemu/internal/qmpclient"
)

type ResumeMode = launch.ResumeMode

const (
	ResumeModeNo    = launch.ResumeModeNo
	ResumeModeAuto  = launch.ResumeModeAuto
	ResumeModeForce = launch.ResumeModeForce
)

func DefaultConfig() Config {
	return Config{
		Locker:              &fileLocker{},
		VSockCIDChecker:     newHostVSockCIDChecker(),
		SocketWaiter:        &pollingSocketWaiter{},
		QMPDialer:           &qmpclient.SocketMonitorDialer{},
		GuestAgentDialer:    &qga.SocketDialer{},
		Logger:              discardLogger,
		ConsoleOutput:       io.Discard,
		ShutdownDelay:       defaultShutdownDelay,
		QMPRetryDelay:       defaultQMPRetryDelay,
		QMPConnectTimeout:   defaultQMPConnectTimeout,
		QMPQuitTimeout:      defaultQMPQuitTimeout,
		QMPMigrationTimeout: defaultQMPMigrationTimeout,
	}
}
