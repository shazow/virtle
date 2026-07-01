package manager

import (
	"time"
)

const (
	defaultQMPRetryDelay       = 200 * time.Millisecond
	defaultQMPConnectTimeout   = 500 * time.Millisecond
	defaultQMPQuitTimeout      = 500 * time.Millisecond
	defaultQMPMigrationTimeout = 30 * time.Second
)
