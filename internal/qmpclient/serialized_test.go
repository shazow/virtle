package qmpclient

import (
	"sync"
	"testing"
	"time"

	rawQMP "github.com/digitalocean/go-qemu/qmp/raw"
)

func TestSerializedClientSerializesCalls(t *testing.T) {
	client := &fakeSerializedClient{release: make(chan struct{})}
	serialized := Serialized(client)

	started := make(chan struct{})
	go func() {
		close(started)
		_ = serialized.Stop(time.Second)
	}()
	<-started
	client.waitForActive(t)

	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = serialized.QueryStatus(time.Second)
	}()

	select {
	case <-done:
		t.Fatal("second qmp call completed before first call released")
	case <-time.After(20 * time.Millisecond):
	}

	close(client.release)
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for serialized qmp call")
	}
}

func TestSerializedClientIsIdempotent(t *testing.T) {
	client := &fakeSerializedClient{release: make(chan struct{})}
	serialized := Serialized(client)
	if got := Serialized(serialized); got != serialized {
		t.Fatalf("serialized client was wrapped again: got %#v want %#v", got, serialized)
	}
	if got := Serialized(nil); got != nil {
		t.Fatalf("nil client: got %#v want nil", got)
	}
}

type fakeSerializedClient struct {
	mu      sync.Mutex
	active  bool
	release chan struct{}
}

func (c *fakeSerializedClient) waitForActive(t *testing.T) {
	t.Helper()

	deadline := time.After(time.Second)
	ticker := time.NewTicker(time.Millisecond)
	defer ticker.Stop()
	for {
		c.mu.Lock()
		active := c.active
		c.mu.Unlock()
		if active {
			return
		}
		select {
		case <-deadline:
			t.Fatal("timed out waiting for active qmp call")
		case <-ticker.C:
		}
	}
}

func (c *fakeSerializedClient) block() {
	c.mu.Lock()
	c.active = true
	c.mu.Unlock()
	<-c.release
}

func (c *fakeSerializedClient) WithRaw(time.Duration, func(*rawQMP.Monitor) error) error {
	c.block()
	return nil
}

func (c *fakeSerializedClient) RunRaw(time.Duration, string) error {
	c.block()
	return nil
}

func (c *fakeSerializedClient) DeviceDelAndWait(time.Duration, string) error {
	c.block()
	return nil
}

func (c *fakeSerializedClient) Stop(time.Duration) error {
	c.block()
	return nil
}

func (c *fakeSerializedClient) Cont(time.Duration) error {
	c.block()
	return nil
}

func (c *fakeSerializedClient) QueryStatus(time.Duration) (string, error) {
	c.block()
	return "running", nil
}

func (c *fakeSerializedClient) MigrateToFile(time.Duration, string) error {
	c.block()
	return nil
}

func (c *fakeSerializedClient) MigrateIncoming(time.Duration, string) error {
	c.block()
	return nil
}

func (c *fakeSerializedClient) QueryMigrate(time.Duration) (string, error) {
	c.block()
	return "completed", nil
}

func (c *fakeSerializedClient) Quit(time.Duration) error {
	c.block()
	return nil
}

func (c *fakeSerializedClient) Disconnect() error {
	c.block()
	return nil
}
