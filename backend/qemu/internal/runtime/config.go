package runtime

import (
	"context"
	"log/slog"
	"time"

	"github.com/shazow/virtle/backend/qemu/internal/launch"
	"github.com/shazow/virtle/internal/manifest"
	"github.com/shazow/virtle/internal/qmpclient"
)

type RuntimeConfig struct {
	Manifest         *manifest.Manifest
	Paths            launch.RuntimePaths
	CID              int
	Stats            *launch.Stats
	QMP              qmpclient.Client
	SuspendRequests  *launch.SuspendCoordinator
	Processes        *launch.ProcessSet
	WriteBack        func(context.Context) error
	Cleanup          func() error
	WriteBackTimeout time.Duration
	Logger           *slog.Logger
	SavedSuspendExit func(error) bool
}
