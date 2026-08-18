package launch

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/shazow/virtle/internal/executor"
	"github.com/shazow/virtle/internal/executor/executortest"
)

func TestWaitForSSHReadyReadsToken(t *testing.T) {
	dialer := &fakeSSHReadyDialer{data: SSHReadyToken + "\n"}
	err := WaitForSSHReady(context.Background(), SSHReadyWait{
		SocketPath:   "ready.sock",
		Timeout:      time.Second,
		PollDelay:    time.Millisecond,
		SocketWaiter: &fakeSSHReadySocketWaiter{},
		Dialer:       dialer,
	})
	if err != nil {
		t.Fatalf("wait for ssh ready: %v", err)
	}
	if dialer.socketPath != "ready.sock" {
		t.Fatalf("dial socket: got %q want ready.sock", dialer.socketPath)
	}
}

func TestWaitForSSHReadyWrapsUnexpectedToken(t *testing.T) {
	err := WaitForSSHReady(context.Background(), SSHReadyWait{
		SocketPath:   "ready.sock",
		Timeout:      time.Second,
		PollDelay:    time.Millisecond,
		SocketWaiter: &fakeSSHReadySocketWaiter{},
		Dialer:       &fakeSSHReadyDialer{data: "NOT_READY\n"},
	})
	if err == nil || !strings.Contains(err.Error(), "vm startup") || !strings.Contains(err.Error(), "unexpected readiness token") {
		t.Fatalf("unexpected token error: %v", err)
	}
}

func TestWaitForSSHReadyWrapsCanceledWait(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := WaitForSSHReady(ctx, SSHReadyWait{
		SocketPath:   "ready.sock",
		Timeout:      time.Second,
		PollDelay:    time.Millisecond,
		SocketWaiter: &fakeSSHReadySocketWaiter{block: true},
		Dialer:       &fakeSSHReadyDialer{},
	})
	if err == nil || !strings.Contains(err.Error(), "wait for ssh readiness") || !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled readiness error: %v", err)
	}
}

func TestWaitForSSHReadyChecksForUnexpectedExit(t *testing.T) {
	process := &executortest.Process{OverrideName: "qemu", WaitErr: errors.New("qemu failed")}
	process.Complete(process.WaitErr)
	err := WaitForSSHReady(context.Background(), SSHReadyWait{
		SocketPath:   "ready.sock",
		Timeout:      time.Second,
		PollDelay:    time.Millisecond,
		SocketWaiter: &fakeSSHReadySocketWaiter{},
		Dialer:       &fakeSSHReadyDialer{block: true},
		Watchers:     executor.NewGroup(process.Process()),
	})
	if !errors.Is(err, process.WaitErr) {
		t.Fatalf("check error: got %v want %v", err, process.WaitErr)
	}
}

type fakeSSHReadySocketWaiter struct {
	block bool
	err   error
}

func (w *fakeSSHReadySocketWaiter) Wait(ctx context.Context, paths []string) error {
	if w.block {
		<-ctx.Done()
		return ctx.Err()
	}
	return w.err
}

type fakeSSHReadyDialer struct {
	socketPath string
	data       string
	err        error
	block      bool
}

func (d *fakeSSHReadyDialer) Dial(ctx context.Context, socketPath string, timeout time.Duration) (io.ReadCloser, error) {
	d.socketPath = socketPath
	if d.err != nil {
		return nil, d.err
	}
	if d.block {
		reader, _ := io.Pipe()
		return reader, nil
	}
	return io.NopCloser(strings.NewReader(d.data)), nil
}
