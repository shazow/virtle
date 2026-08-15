package launch

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/shazow/virtle/backend/qemu/internal/qmpclient"
	"github.com/shazow/virtle/internal/executor"
	"github.com/shazow/virtle/internal/executor/executortest"
)

func TestWaitForQMPWaitsForSocketThenDials(t *testing.T) {
	waiter := &fakeQMPSocketWaiter{}
	dialer := &fakeQMPDialer{}

	client, err := WaitForQMP(context.Background(), QMPWait{
		SocketPath:     "qmp.sock",
		SocketWaiter:   waiter,
		Dialer:         dialer,
		ConnectTimeout: 10 * time.Millisecond,
		RetryDelay:     time.Millisecond,
		PollDelay:      time.Millisecond,
	})
	if err != nil {
		t.Fatalf("wait for qmp: %v", err)
	}
	if client != nil {
		t.Fatalf("expected nil fake client, got %#v", client)
	}
	if !waiter.called {
		t.Fatalf("expected socket waiter to be called")
	}
	if got, want := waiter.paths, []string{"qmp.sock"}; len(got) != len(want) || got[0] != want[0] {
		t.Fatalf("socket paths: got %v want %v", got, want)
	}
	if got, want := dialer.socketPath, "qmp.sock"; got != want {
		t.Fatalf("dial socket path: got %q want %q", got, want)
	}
	if got, want := dialer.timeout, 10*time.Millisecond; got != want {
		t.Fatalf("dial timeout: got %s want %s", got, want)
	}
}

func TestWaitForQMPWrapsSocketWaitError(t *testing.T) {
	waitErr := errors.New("socket failed")
	_, err := WaitForQMP(context.Background(), QMPWait{
		Stage:        "vm startup",
		SocketPath:   "qmp.sock",
		SocketWaiter: &fakeQMPSocketWaiter{err: waitErr},
		Dialer:       &fakeQMPDialer{},
		PollDelay:    time.Millisecond,
	})
	var stageErr *StageError
	if !errors.As(err, &stageErr) || stageErr.Stage != "vm startup" || !errors.Is(err, waitErr) {
		t.Fatalf("wrapped err: got %v", err)
	}
}

func TestWaitForQMPChecksWatcherExitWhileDialing(t *testing.T) {
	checkErr := errors.New("qemu exited")
	process := &executortest.Process{OverrideName: "qemu"}
	dialer := &fakeQMPDialer{
		err: errors.New("not ready"),
		afterDial: func() {
			process.Complete(checkErr)
		},
	}
	_, err := WaitForQMP(context.Background(), QMPWait{
		SocketPath:   "qmp.sock",
		SocketWaiter: &fakeQMPSocketWaiter{},
		Dialer:       dialer,
		RetryDelay:   time.Millisecond,
		PollDelay:    time.Millisecond,
		Watchers:     executor.NewGroup(process.Process()),
	})
	if !errors.Is(err, checkErr) {
		t.Fatalf("watcher exit err: got %v want %v", err, checkErr)
	}
	if dialer.calls != 1 {
		t.Fatalf("dialer calls: got %d want 1", dialer.calls)
	}
}

func TestWaitForQMPWrapsDialCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := WaitForQMP(ctx, QMPWait{
		SocketPath:   "qmp.sock",
		SocketWaiter: &fakeQMPSocketWaiter{},
		Dialer:       &fakeQMPDialer{},
		RetryDelay:   time.Millisecond,
		PollDelay:    time.Millisecond,
	})
	var stageErr *StageError
	if !errors.As(err, &stageErr) || stageErr.Stage != "vm startup" || !errors.Is(err, context.Canceled) {
		t.Fatalf("cancel err: got %v", err)
	}
}

type fakeQMPSocketWaiter struct {
	called bool
	paths  []string
	err    error
}

func (w *fakeQMPSocketWaiter) Wait(ctx context.Context, socketPaths []string) error {
	w.called = true
	w.paths = append([]string(nil), socketPaths...)
	return w.err
}

type fakeQMPDialer struct {
	calls      int
	socketPath string
	timeout    time.Duration
	err        error
	afterDial  func()
}

func (d *fakeQMPDialer) Dial(ctx context.Context, socketPath string, timeout time.Duration) (qmpclient.Client, error) {
	d.calls++
	d.socketPath = socketPath
	d.timeout = timeout
	if d.afterDial != nil {
		d.afterDial()
	}
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	return nil, d.err
}
