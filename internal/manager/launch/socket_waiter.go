package launch

import (
	"context"
	"fmt"
	"os"
	"time"
)

// DefaultSocketPollInterval paces the stat loop that waits for QEMU and its
// helpers to create their sockets.
const DefaultSocketPollInterval = 100 * time.Millisecond

// PollingSocketWaiter waits for socket paths to appear by polling the
// filesystem, which is all the notification a Unix socket offers.
type PollingSocketWaiter struct {
	// PollInterval overrides the stat pacing; zero uses
	// DefaultSocketPollInterval.
	PollInterval time.Duration
}

func (w *PollingSocketWaiter) Wait(ctx context.Context, socketPaths []string) error {
	interval := w.PollInterval
	if interval <= 0 {
		interval = DefaultSocketPollInterval
	}

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		allReady := true
		for _, path := range socketPaths {
			if _, err := os.Stat(path); err != nil {
				allReady = false
				break
			}
		}
		if allReady {
			return nil
		}

		select {
		case <-ctx.Done():
			return fmt.Errorf("wait for sockets: %w", ctx.Err())
		case <-ticker.C:
		}
	}
}
