package launch

import (
	"context"
	"errors"
	"reflect"
	"testing"
)

func TestTaskStopCancelsAndReturnsErrorOnce(t *testing.T) {
	wantErr := errors.New("stopped")
	calls := 0
	task := startTask(context.Background(), func(ctx context.Context) error {
		<-ctx.Done()
		calls++
		return wantErr
	})

	if err := task.Stop(); !errors.Is(err, wantErr) {
		t.Fatalf("unexpected stop error: got %v want %v", err, wantErr)
	}
	if err := task.Stop(); !errors.Is(err, wantErr) {
		t.Fatalf("unexpected second stop error: got %v want %v", err, wantErr)
	}
	if calls != 1 {
		t.Fatalf("task stopped %d times, want 1", calls)
	}
}

func TestTaskGroupStopsInReverseOrder(t *testing.T) {
	var got []string
	var group taskGroup
	group.Add(startTask(context.Background(), func(ctx context.Context) error {
		<-ctx.Done()
		got = append(got, "first")
		return nil
	}))
	group.Add(startTask(context.Background(), func(ctx context.Context) error {
		<-ctx.Done()
		got = append(got, "second")
		return nil
	}))

	if err := group.Stop(); err != nil {
		t.Fatalf("stop group: %v", err)
	}
	if want := []string{"second", "first"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("unexpected stop order: got %#v want %#v", got, want)
	}
}
