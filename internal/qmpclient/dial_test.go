package qmpclient

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestDialWithRetryReturnsClientAfterRetry(t *testing.T) {
	want := &dialClient{}
	dialer := &retryDialer{failures: 1, client: want}
	client, err := DialWithRetry(context.Background(), dialer, DialRetry{
		SocketPath: "qmp.sock",
		Timeout:    10 * time.Millisecond,
		RetryDelay: time.Millisecond,
	})
	if err != nil {
		t.Fatalf("dial retry: %v", err)
	}
	if client != want {
		t.Fatalf("client: got %#v want %#v", client, want)
	}
	if got, want := dialer.calls, 2; got != want {
		t.Fatalf("dial calls: got %d want %d", got, want)
	}
	if got, want := dialer.socketPath, "qmp.sock"; got != want {
		t.Fatalf("socket path: got %q want %q", got, want)
	}
	if got, want := dialer.timeout, 10*time.Millisecond; got != want {
		t.Fatalf("timeout: got %s want %s", got, want)
	}
}

func TestDialWithRetryReturnsCheckError(t *testing.T) {
	wantErr := errors.New("qemu exited")
	_, err := DialWithRetry(context.Background(), &retryDialer{}, DialRetry{
		RetryDelay: time.Millisecond,
		Check: func() error {
			return wantErr
		},
	})
	if !errors.Is(err, wantErr) {
		t.Fatalf("check error: got %v want %v", err, wantErr)
	}
}

func TestDialWithRetryReturnsContextCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	dialer := &retryDialer{err: errors.New("not ready"), afterDial: cancel}
	_, err := DialWithRetry(ctx, dialer, DialRetry{RetryDelay: time.Hour})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("cancel error: got %v want %v", err, context.Canceled)
	}
}

type retryDialer struct {
	failures   int
	calls      int
	socketPath string
	timeout    time.Duration
	client     Client
	err        error
	afterDial  func()
}

type dialClient struct {
	Client
}

func (d *retryDialer) Dial(ctx context.Context, socketPath string, timeout time.Duration) (Client, error) {
	d.calls++
	d.socketPath = socketPath
	d.timeout = timeout
	if d.afterDial != nil {
		d.afterDial()
	}
	if d.failures > 0 {
		d.failures--
		return nil, errors.New("not ready")
	}
	if d.err != nil {
		return nil, d.err
	}
	return d.client, nil
}
