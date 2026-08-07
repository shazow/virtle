package qmpclient

import (
	"context"
	"fmt"
	"time"
)

// MigrationWait configures polling for QMP migration completion. The overall
// migration deadline is carried by ctx.
type MigrationWait struct {
	PollDelay time.Duration
}

// SaveClient is the QMP capability set needed to save VM state to a file.
type SaveClient interface {
	QueryStatus(ctx context.Context) (string, error)
	Stop(ctx context.Context) error
	MigrateToFile(ctx context.Context, path string) error
	QueryMigrate(ctx context.Context) (string, error)
}

// RestoreClient is the QMP capability set needed to restore VM state from a file.
type RestoreClient interface {
	MigrateIncoming(ctx context.Context, path string) error
	QueryMigrate(ctx context.Context) (string, error)
	Cont(ctx context.Context) error
}

// MigrationMonitor queries migration progress.
type MigrationMonitor interface {
	QueryMigrate(ctx context.Context) (string, error)
}

// SaveToFile saves VM state to path through QMP migration.
func SaveToFile(ctx context.Context, client SaveClient, path string, wait MigrationWait) error {
	status, err := client.QueryStatus(ctx)
	if err != nil {
		return err
	}
	switch status {
	case "paused":
	case "running":
		if err := client.Stop(ctx); err != nil {
			return err
		}
	default:
		return fmt.Errorf("cannot save VM while QMP status is %q", status)
	}
	if err := client.MigrateToFile(ctx, path); err != nil {
		return err
	}
	return WaitForMigration(ctx, client, wait)
}

// RestoreFromFile restores VM state from path through QMP migration.
func RestoreFromFile(ctx context.Context, client RestoreClient, path string, wait MigrationWait) error {
	if err := client.MigrateIncoming(ctx, path); err != nil {
		return err
	}
	if err := WaitForMigration(ctx, client, wait); err != nil {
		return err
	}
	return client.Cont(ctx)
}

// WaitForMigration polls until QMP migration reaches a terminal state or ctx
// ends.
func WaitForMigration(ctx context.Context, client MigrationMonitor, wait MigrationWait) error {
	if wait.PollDelay <= 0 {
		wait.PollDelay = time.Second
	}
	ticker := time.NewTicker(wait.PollDelay)
	defer ticker.Stop()

	var lastStatus string
	for {
		status, err := client.QueryMigrate(ctx)
		if err != nil {
			return err
		}
		if status != "" {
			lastStatus = status
		}
		switch status {
		case "completed":
			return nil
		case "failed", "cancelled":
			return fmt.Errorf("migration %s", status)
		}

		select {
		case <-ctx.Done():
			if lastStatus == "" {
				lastStatus = "unknown"
			}
			return fmt.Errorf("migration did not complete: %w; last status %q", context.Cause(ctx), lastStatus)
		case <-ticker.C:
		}
	}
}
