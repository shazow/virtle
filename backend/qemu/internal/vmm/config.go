package vmm

import (
	"io"
	"log/slog"
	"time"

	"github.com/shazow/virtle/backend/qemu/internal/launch"
	"github.com/shazow/virtle/backend/qemu/internal/qga"
	"github.com/shazow/virtle/backend/qemu/internal/qmpclient"
)

type Config struct {
	Locker              launch.Locker
	VSockCIDChecker     launch.VSockCIDChecker
	Runner              launch.Runner
	SocketWaiter        launch.SocketWaiter
	QMPDialer           qmpclient.Dialer
	GuestAgentDialer    qga.Dialer
	Logger              *slog.Logger
	ConsoleOutput       io.Writer
	ShutdownDelay       time.Duration
	QMPRetryDelay       time.Duration
	QMPConnectTimeout   time.Duration
	QMPQuitTimeout      time.Duration
	QMPMigrationTimeout time.Duration
	Notifier            launch.NotificationSink
}

// StateVersion is the qemu suspend-state version this machinery stamps on
// saves and compares on resume; only an exact match is resumable, since
// the saved VM state is a QEMU migration stream. Bump the revision when
// the format changes incompatibly. Surfaced publicly through the qemu
// backend's Suspender.StateVersion.
const StateVersion = "qemu-v1"

func mergeConfig(base Config, override Config) Config {
	if override.Locker != nil {
		base.Locker = override.Locker
	}
	if override.VSockCIDChecker != nil {
		base.VSockCIDChecker = override.VSockCIDChecker
	}
	if override.Runner != nil {
		base.Runner = override.Runner
	}
	if override.SocketWaiter != nil {
		base.SocketWaiter = override.SocketWaiter
	}
	if override.QMPDialer != nil {
		base.QMPDialer = override.QMPDialer
	}
	if override.GuestAgentDialer != nil {
		base.GuestAgentDialer = override.GuestAgentDialer
	}
	if override.Logger != nil {
		base.Logger = override.Logger
	}
	if override.ConsoleOutput != nil {
		base.ConsoleOutput = override.ConsoleOutput
	}
	if override.ShutdownDelay != 0 {
		base.ShutdownDelay = override.ShutdownDelay
	}
	if override.QMPRetryDelay != 0 {
		base.QMPRetryDelay = override.QMPRetryDelay
	}
	if override.QMPConnectTimeout != 0 {
		base.QMPConnectTimeout = override.QMPConnectTimeout
	}
	if override.QMPQuitTimeout != 0 {
		base.QMPQuitTimeout = override.QMPQuitTimeout
	}
	if override.QMPMigrationTimeout != 0 {
		base.QMPMigrationTimeout = override.QMPMigrationTimeout
	}
	if override.Notifier != nil {
		base.Notifier = override.Notifier
	}
	return base
}
