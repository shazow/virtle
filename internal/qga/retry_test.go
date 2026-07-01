package qga

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestDialWithRetryReturnsClientAfterRetry(t *testing.T) {
	first := &retryClient{pingErr: errors.New("not ready")}
	want := &retryClient{}
	dialer := &retryDialer{
		clients: []Client{
			first,
			want,
		},
	}
	client, err := DialWithRetry(context.Background(), dialer, DialRetry{
		SocketPath:     "qga.sock",
		ConnectTimeout: 10 * time.Millisecond,
		CommandTimeout: 20 * time.Millisecond,
		RetryDelay:     time.Millisecond,
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
	if got, want := dialer.socketPath, "qga.sock"; got != want {
		t.Fatalf("socket path: got %q want %q", got, want)
	}
	if got, want := dialer.timeout, 10*time.Millisecond; got != want {
		t.Fatalf("timeout: got %s want %s", got, want)
	}
	if !first.disconnected {
		t.Fatalf("expected failed ping client to be disconnected")
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
	calls      int
	socketPath string
	timeout    time.Duration
	clients    []Client
	err        error
	afterDial  func()
}

func (d *retryDialer) Dial(ctx context.Context, socketPath string, timeout time.Duration) (Client, error) {
	d.calls++
	d.socketPath = socketPath
	d.timeout = timeout
	if d.afterDial != nil {
		d.afterDial()
	}
	if d.err != nil {
		return nil, d.err
	}
	if len(d.clients) == 0 {
		return &retryClient{}, nil
	}
	client := d.clients[0]
	d.clients = d.clients[1:]
	return client, nil
}

type retryClient struct {
	pingErr      error
	disconnected bool
}

func (c *retryClient) Ping(time.Duration) error                        { return c.pingErr }
func (c *retryClient) OpenFile(time.Duration, string) (int, error)     { return 0, nil }
func (c *retryClient) OpenFileRead(time.Duration, string) (int, error) { return 0, nil }
func (c *retryClient) ReadFile(time.Duration, int, int) (string, bool, error) {
	return "", false, nil
}
func (c *retryClient) WriteFile(time.Duration, int, string) error              { return nil }
func (c *retryClient) CloseFile(time.Duration, int) error                      { return nil }
func (c *retryClient) Exec(time.Duration, string, []string, bool) (int, error) { return 0, nil }
func (c *retryClient) ExecStatus(time.Duration, int) (ExecStatus, error) {
	return ExecStatus{}, nil
}
func (c *retryClient) Disconnect() error {
	c.disconnected = true
	return nil
}
