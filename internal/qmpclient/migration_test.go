package qmpclient

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestWaitForMigrationCompletes(t *testing.T) {
	client := &migrationClient{statuses: []string{"active", "completed"}}
	if err := WaitForMigration(context.Background(), client, MigrationWait{PollDelay: time.Millisecond}); err != nil {
		t.Fatalf("wait migration: %v", err)
	}
}

func TestWaitForMigrationReturnsTerminalFailure(t *testing.T) {
	client := &migrationClient{statuses: []string{"active", "failed"}}
	err := WaitForMigration(context.Background(), client, MigrationWait{PollDelay: time.Millisecond})
	if err == nil || !strings.Contains(err.Error(), "migration failed") {
		t.Fatalf("expected failed migration error, got %v", err)
	}
}

func TestWaitForMigrationDeadlineReportsLastStatus(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Millisecond)
	defer cancel()
	client := &migrationClient{statuses: []string{"setup"}}
	err := WaitForMigration(ctx, client, MigrationWait{PollDelay: time.Millisecond})
	if err == nil || !strings.Contains(err.Error(), `last status "setup"`) {
		t.Fatalf("expected deadline error with last status, got %v", err)
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("expected deadline cause, got %v", err)
	}
}

func TestWaitForMigrationReturnsQueryError(t *testing.T) {
	wantErr := errors.New("query migrate failed")
	client := &migrationClient{err: wantErr}
	err := WaitForMigration(context.Background(), client, MigrationWait{PollDelay: time.Millisecond})
	if !errors.Is(err, wantErr) {
		t.Fatalf("query error: got %v want %v", err, wantErr)
	}
}

func TestWaitForMigrationReturnsContextCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	client := &migrationClient{afterQuery: cancel}
	err := WaitForMigration(ctx, client, MigrationWait{PollDelay: time.Hour})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("cancel error: got %v want %v", err, context.Canceled)
	}
}

func TestRestoreFromFileMigratesWaitsAndContinues(t *testing.T) {
	client := &migrationClient{statuses: []string{"active", "completed"}}
	if err := RestoreFromFile(context.Background(), client, "/tmp/vmstate", MigrationWait{PollDelay: time.Millisecond}); err != nil {
		t.Fatalf("restore from file: %v", err)
	}
	assertCalls(t, client.calls, []string{"migrate-incoming:/tmp/vmstate", "query-migrate", "query-migrate", "cont"})
}

func TestRestoreFromFileReturnsMigrateIncomingError(t *testing.T) {
	wantErr := errors.New("restore failed")
	client := &migrationClient{migrateIncomingErr: wantErr}
	err := RestoreFromFile(context.Background(), client, "/tmp/vmstate", MigrationWait{PollDelay: time.Millisecond})
	if !errors.Is(err, wantErr) {
		t.Fatalf("restore error: got %v want %v", err, wantErr)
	}
}

func TestSaveToFileStopsRunningVMAndMigrates(t *testing.T) {
	client := &migrationClient{vmStatus: "running", statuses: []string{"active", "completed"}}
	if err := SaveToFile(context.Background(), client, "/tmp/vmstate", MigrationWait{PollDelay: time.Millisecond}); err != nil {
		t.Fatalf("save to file: %v", err)
	}
	assertCalls(t, client.calls, []string{"query-status", "stop", "migrate:/tmp/vmstate", "query-migrate", "query-migrate"})
}

func TestSaveToFileMigratesPausedVMWithoutStop(t *testing.T) {
	client := &migrationClient{vmStatus: "paused", statuses: []string{"completed"}}
	if err := SaveToFile(context.Background(), client, "/tmp/vmstate", MigrationWait{PollDelay: time.Millisecond}); err != nil {
		t.Fatalf("save paused vm: %v", err)
	}
	assertCalls(t, client.calls, []string{"query-status", "migrate:/tmp/vmstate", "query-migrate"})
}

func TestSaveToFileRejectsInvalidVMStatus(t *testing.T) {
	client := &migrationClient{vmStatus: "shutdown"}
	err := SaveToFile(context.Background(), client, "/tmp/vmstate", MigrationWait{PollDelay: time.Millisecond})
	if err == nil || !strings.Contains(err.Error(), `cannot save VM while QMP status is "shutdown"`) {
		t.Fatalf("expected invalid status error, got %v", err)
	}
}

func assertCalls(t *testing.T, got []string, want []string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("calls: got %#v want %#v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("calls: got %#v want %#v", got, want)
		}
	}
}

type migrationClient struct {
	statuses           []string
	vmStatus           string
	err                error
	migrateIncomingErr error
	calls              []string
	afterQuery         func()
}

func (c *migrationClient) QueryStatus(ctx context.Context) (string, error) {
	c.calls = append(c.calls, "query-status")
	return c.vmStatus, nil
}

func (c *migrationClient) Stop(ctx context.Context) error {
	c.calls = append(c.calls, "stop")
	c.vmStatus = "paused"
	return nil
}

func (c *migrationClient) MigrateToFile(ctx context.Context, path string) error {
	c.calls = append(c.calls, "migrate:"+path)
	return nil
}

func (c *migrationClient) MigrateIncoming(ctx context.Context, path string) error {
	c.calls = append(c.calls, "migrate-incoming:"+path)
	return c.migrateIncomingErr
}

func (c *migrationClient) Cont(ctx context.Context) error {
	c.calls = append(c.calls, "cont")
	return nil
}

func (c *migrationClient) QueryMigrate(ctx context.Context) (string, error) {
	c.calls = append(c.calls, "query-migrate")
	if c.err != nil {
		return "", c.err
	}
	if c.afterQuery != nil {
		c.afterQuery()
	}
	if len(c.statuses) == 0 {
		return "", nil
	}
	status := c.statuses[0]
	if len(c.statuses) > 1 {
		c.statuses = c.statuses[1:]
	}
	return status, nil
}
