package launch

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/shazow/virtle/backend/qemu/internal/qga"
	"github.com/shazow/virtle/internal/executor"
	"github.com/shazow/virtle/internal/executor/executortest"
)

func TestWaitForGuestAgentWaitsForSocketThenDials(t *testing.T) {
	waiter := &fakeGuestAgentSocketWaiter{}
	client := &fakeGuestAgentClient{}
	dialer := &fakeGuestAgentDialer{client: client}

	got, err := WaitForGuestAgent(context.Background(), GuestAgentWait{
		SocketPath:     "qga.sock",
		SocketWaiter:   waiter,
		Dialer:         dialer,
		ConnectTimeout: 10 * time.Millisecond,
		RetryDelay:     time.Millisecond,
		PollDelay:      time.Millisecond,
	})
	if err != nil {
		t.Fatalf("wait for guest agent: %v", err)
	}
	if got != client {
		t.Fatalf("client: got %#v want %#v", got, client)
	}
	if !waiter.called {
		t.Fatal("expected socket waiter to be called")
	}
	if got, want := dialer.socketPath, "qga.sock"; got != want {
		t.Fatalf("dial socket: got %q want %q", got, want)
	}
	if got, want := dialer.timeout, 10*time.Millisecond; got != want {
		t.Fatalf("dial timeout: got %s want %s", got, want)
	}
	if !client.pinged {
		t.Fatal("expected readiness ping")
	}
}

func TestWaitForGuestAgentWrapsSocketWaitError(t *testing.T) {
	waitErr := errors.New("socket failed")
	_, err := WaitForGuestAgent(context.Background(), GuestAgentWait{
		Stage:        "guest agent",
		SocketWaiter: &fakeGuestAgentSocketWaiter{err: waitErr},
		Dialer:       &fakeGuestAgentDialer{client: &fakeGuestAgentClient{}},
		PollDelay:    time.Millisecond,
	})
	var stageErr *StageError
	if !errors.As(err, &stageErr) || stageErr.Stage != "guest agent" || !errors.Is(err, waitErr) {
		t.Fatalf("wrapped err: got %v", err)
	}
}

func TestWaitForGuestAgentChecksWatcherExitWhileDialing(t *testing.T) {
	checkErr := errors.New("qemu exited")
	process := &executortest.Process{OverrideName: "qemu"}
	dialer := &fakeGuestAgentDialer{
		err: errors.New("not ready"),
		afterDial: func() {
			process.Complete(checkErr)
		},
	}
	_, err := WaitForGuestAgent(context.Background(), GuestAgentWait{
		SocketWaiter: &fakeGuestAgentSocketWaiter{},
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

func TestWaitForGuestAgentWrapsDialCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := WaitForGuestAgent(ctx, GuestAgentWait{
		SocketWaiter: &fakeGuestAgentSocketWaiter{},
		Dialer:       &fakeGuestAgentDialer{client: &fakeGuestAgentClient{}},
		RetryDelay:   time.Millisecond,
		PollDelay:    time.Millisecond,
	})
	var stageErr *StageError
	if !errors.As(err, &stageErr) || stageErr.Stage != "guest agent" || !errors.Is(err, context.Canceled) {
		t.Fatalf("cancel err: got %v", err)
	}
}

type fakeGuestAgentSocketWaiter struct {
	called bool
	paths  []string
	err    error
}

func (w *fakeGuestAgentSocketWaiter) Wait(ctx context.Context, socketPaths []string) error {
	w.called = true
	w.paths = append([]string(nil), socketPaths...)
	return w.err
}

type fakeGuestAgentDialer struct {
	calls      int
	socketPath string
	timeout    time.Duration
	client     qga.Client
	err        error
	afterDial  func()
}

func (d *fakeGuestAgentDialer) Dial(ctx context.Context, socketPath string, timeout time.Duration) (qga.Client, error) {
	d.calls++
	d.socketPath = socketPath
	d.timeout = timeout
	if d.afterDial != nil {
		d.afterDial()
	}
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	if d.err != nil {
		return nil, d.err
	}
	return d.client, nil
}

type fakeGuestAgentClient struct {
	pinged bool
}

func (c *fakeGuestAgentClient) Ping(ctx context.Context) error {
	c.pinged = true
	return nil
}

func (c *fakeGuestAgentClient) OpenFile(context.Context, string) (int, error) { return 0, nil }
func (c *fakeGuestAgentClient) OpenFileRead(context.Context, string) (int, error) {
	return 0, nil
}
func (c *fakeGuestAgentClient) ReadFile(context.Context, int, int) (string, bool, error) {
	return "", false, nil
}
func (c *fakeGuestAgentClient) WriteFile(context.Context, int, string) error { return nil }
func (c *fakeGuestAgentClient) CloseFile(context.Context, int) error         { return nil }
func (c *fakeGuestAgentClient) Exec(context.Context, string, []string, bool) (int, error) {
	return 0, nil
}
func (c *fakeGuestAgentClient) ExecStatus(context.Context, int) (qga.ExecStatus, error) {
	return qga.ExecStatus{}, nil
}
func (c *fakeGuestAgentClient) Shutdown(context.Context) error { return nil }
func (c *fakeGuestAgentClient) Disconnect() error              { return nil }
