package qmpwire

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestAppendDelimiter(t *testing.T) {
	if got := string(AppendDelimiter([]byte(`{"execute":"x"}`))); got != "{\"execute\":\"x\"}\n" {
		t.Fatalf("unexpected delimited command: %q", got)
	}
	if got := string(AppendDelimiter([]byte("{}\n"))); got != "{}\n" {
		t.Fatalf("expected already-delimited command unchanged, got %q", got)
	}
}

func TestDialWithRetryProbesAndCloses(t *testing.T) {
	dials := 0
	probes := 0
	closes := 0
	client, err := DialWithRetry(context.Background(), Retry[int]{
		Dial: func(context.Context) (int, error) {
			dials++
			return dials, nil
		},
		Probe: func(int) error {
			probes++
			if probes < 3 {
				return errors.New("not ready")
			}
			return nil
		},
		Close: func(int) { closes++ },
		Delay: time.Millisecond,
	})
	if err != nil {
		t.Fatalf("dial with retry: %v", err)
	}
	if client != 3 || dials != 3 || closes != 2 {
		t.Fatalf("unexpected retry accounting: client=%d dials=%d closes=%d", client, dials, closes)
	}
}

func TestDialWithRetryCheckAborts(t *testing.T) {
	watcherErr := errors.New("watched process exited")
	_, err := DialWithRetry(context.Background(), Retry[int]{
		Dial:  func(context.Context) (int, error) { return 0, errors.New("refused") },
		Check: func() error { return watcherErr },
		Delay: time.Millisecond,
	})
	if !errors.Is(err, watcherErr) {
		t.Fatalf("expected check error, got %v", err)
	}
}

func TestDialWithRetryHonorsContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := DialWithRetry(ctx, Retry[int]{
		Dial:  func(context.Context) (int, error) { return 0, errors.New("refused") },
		Delay: time.Millisecond,
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context error, got %v", err)
	}
}
