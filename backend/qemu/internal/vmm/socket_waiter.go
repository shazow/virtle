package vmm

import (
	"context"
	"fmt"
	"os"
	"time"
)

const defaultSocketPollInterval = 100 * time.Millisecond

// pollingSocketWaiter reports readiness once every socket path exists.
type pollingSocketWaiter struct{}

func (w *pollingSocketWaiter) Wait(ctx context.Context, socketPaths []string) error {
	ticker := time.NewTicker(defaultSocketPollInterval)
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
