package manager

import (
	"context"
	"fmt"
	"os"
	"time"
)

const defaultSocketPollInterval = 100 * time.Millisecond

type pollingSocketWaiter struct {
	PollInterval time.Duration
}

func (w *pollingSocketWaiter) Wait(ctx context.Context, socketPaths []string) error {
	interval := w.PollInterval
	if interval <= 0 {
		interval = defaultSocketPollInterval
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
