package manager

import (
	"io"
	"log/slog"
	"os"
	"time"

	"github.com/shazow/virtle/internal/manager/launch"
	"github.com/shazow/virtle/internal/qga"
	"github.com/shazow/virtle/internal/qmpclient"
)

type Config struct {
	Locker              launch.Locker
	VSockCIDChecker     launch.VSockCIDChecker
	Runner              launch.Runner
	SocketWaiter        launch.SocketWaiter
	QMPDialer           qmpclient.Dialer
	GuestAgentDialer    qga.Dialer
	SSHReadyDialer      launch.SSHReadyDialer
	Logger              *slog.Logger
	LogWriter           io.Writer
	SSHReadyTimeout     time.Duration
	ShutdownDelay       time.Duration
	QMPRetryDelay       time.Duration
	QMPConnectTimeout   time.Duration
	QMPQuitTimeout      time.Duration
	QMPMigrationTimeout time.Duration
	Signals             <-chan os.Signal
	PIDSignaler         launch.PIDSignaler
	Notifier            launch.NotificationSink
	// SuspendStateVersion is the backend's state version token (e.g.
	// qemu.StateVersion) stamped on saved suspend state and compared on
	// resume.
	SuspendStateVersion string
}

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
	if override.SSHReadyDialer != nil {
		base.SSHReadyDialer = override.SSHReadyDialer
	}
	if override.Logger != nil {
		base.Logger = override.Logger
	}
	if override.LogWriter != nil {
		base.LogWriter = override.LogWriter
	}
	if override.SSHReadyTimeout != 0 {
		base.SSHReadyTimeout = override.SSHReadyTimeout
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
	if override.Signals != nil {
		base.Signals = override.Signals
	}
	if override.PIDSignaler != nil {
		base.PIDSignaler = override.PIDSignaler
	}
	if override.Notifier != nil {
		base.Notifier = override.Notifier
	}
	if override.SuspendStateVersion != "" {
		base.SuspendStateVersion = override.SuspendStateVersion
	}
	return base
}
