package qmpclient

import (
	"context"
	"fmt"
	"time"
)

// MigrationWait configures polling for QMP migration completion.
type MigrationWait struct {
	Timeout        time.Duration
	CommandTimeout time.Duration
	PollDelay      time.Duration
}

// RestoreWait configures VM state restore timeouts.
type RestoreWait struct {
	MigrationTimeout time.Duration
	CommandTimeout   time.Duration
	PollDelay        time.Duration
}

// SaveWait configures VM state save timeouts.
type SaveWait struct {
	MigrationTimeout time.Duration
	CommandTimeout   time.Duration
	PollDelay        time.Duration
}

// SaveClient is the QMP capability set needed to save VM state to a file.
type SaveClient interface {
	QueryStatus(timeout time.Duration) (string, error)
	Stop(timeout time.Duration) error
	MigrateToFile(timeout time.Duration, path string) error
	QueryMigrate(timeout time.Duration) (string, error)
}

// RestoreClient is the QMP capability set needed to restore VM state from a file.
type RestoreClient interface {
	MigrateIncoming(timeout time.Duration, path string) error
	QueryMigrate(timeout time.Duration) (string, error)
	Cont(timeout time.Duration) error
}

// MigrationMonitor queries migration progress.
type MigrationMonitor interface {
	QueryMigrate(timeout time.Duration) (string, error)
}

// SaveToFile saves VM state to path through QMP migration.
func SaveToFile(ctx context.Context, client SaveClient, path string, wait SaveWait) error {
	status, err := client.QueryStatus(wait.CommandTimeout)
	if err != nil {
		return err
	}
	switch status {
	case "paused":
	case "running":
		if err := client.Stop(wait.CommandTimeout); err != nil {
			return err
		}
	default:
		return fmt.Errorf("cannot save VM while QMP status is %q", status)
	}
	if err := client.MigrateToFile(wait.MigrationTimeout, path); err != nil {
		return err
	}
	return WaitForMigration(ctx, client, MigrationWait{
		Timeout:        wait.MigrationTimeout,
		CommandTimeout: wait.CommandTimeout,
		PollDelay:      wait.PollDelay,
	})
}

// RestoreFromFile restores VM state from path through QMP migration.
func RestoreFromFile(ctx context.Context, client RestoreClient, path string, wait RestoreWait) error {
	if err := client.MigrateIncoming(wait.MigrationTimeout, path); err != nil {
		return err
	}
	if err := WaitForMigration(ctx, client, MigrationWait{
		Timeout:        wait.MigrationTimeout,
		CommandTimeout: wait.CommandTimeout,
		PollDelay:      wait.PollDelay,
	}); err != nil {
		return err
	}
	return client.Cont(wait.CommandTimeout)
}

// WaitForMigration polls until QMP migration reaches a terminal state.
func WaitForMigration(ctx context.Context, client MigrationMonitor, wait MigrationWait) error {
	if wait.PollDelay <= 0 {
		wait.PollDelay = time.Second
	}
	deadline := time.NewTimer(wait.Timeout)
	defer deadline.Stop()
	ticker := time.NewTicker(wait.PollDelay)
	defer ticker.Stop()

	var lastStatus string
	for {
		status, err := client.QueryMigrate(wait.CommandTimeout)
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
			return ctx.Err()
		case <-deadline.C:
			if lastStatus == "" {
				lastStatus = "unknown"
			}
			return fmt.Errorf("migration did not complete within %s; last status %q", wait.Timeout, lastStatus)
		case <-ticker.C:
		}
	}
}
