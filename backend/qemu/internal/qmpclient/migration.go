package qmpclient

import (
	"context"
	"fmt"
	"time"
)

// migrationPollDelay is the fixed delay between query-migrate polls while
// waiting for a migration to finish. The overall migration deadline is
// carried by ctx.
const migrationPollDelay = 250 * time.Millisecond

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
func SaveToFile(ctx context.Context, client SaveClient, path string) error {
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
	return WaitForMigration(ctx, client)
}

// RestoreFromFile restores VM state from path through QMP migration.
func RestoreFromFile(ctx context.Context, client RestoreClient, path string) error {
	if err := client.MigrateIncoming(ctx, path); err != nil {
		return err
	}
	if err := WaitForMigration(ctx, client); err != nil {
		return err
	}
	return client.Cont(ctx)
}

// WaitForMigration polls until QMP migration reaches a terminal state or ctx
// ends.
func WaitForMigration(ctx context.Context, client MigrationMonitor) error {
	ticker := time.NewTicker(migrationPollDelay)
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
