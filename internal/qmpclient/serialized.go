package qmpclient

import (
	"sync"
	"time"

	rawQMP "github.com/digitalocean/go-qemu/qmp/raw"
)

type serializedClient struct {
	client Client
	mu     sync.Mutex
}

// Serialized wraps client so all QMP calls are executed serially.
func Serialized(client Client) Client {
	if client == nil {
		return nil
	}
	if _, ok := client.(*serializedClient); ok {
		return client
	}
	return &serializedClient{client: client}
}

func (c *serializedClient) WithRaw(timeout time.Duration, fn func(*rawQMP.Monitor) error) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.client.WithRaw(timeout, fn)
}

func (c *serializedClient) RunRaw(timeout time.Duration, command string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.client.RunRaw(timeout, command)
}

func (c *serializedClient) DeviceDelAndWait(timeout time.Duration, id string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.client.DeviceDelAndWait(timeout, id)
}

func (c *serializedClient) Stop(timeout time.Duration) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.client.Stop(timeout)
}

func (c *serializedClient) Cont(timeout time.Duration) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.client.Cont(timeout)
}

func (c *serializedClient) QueryStatus(timeout time.Duration) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.client.QueryStatus(timeout)
}

func (c *serializedClient) MigrateToFile(timeout time.Duration, path string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.client.MigrateToFile(timeout, path)
}

func (c *serializedClient) MigrateIncoming(timeout time.Duration, path string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.client.MigrateIncoming(timeout, path)
}

func (c *serializedClient) QueryMigrate(timeout time.Duration) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.client.QueryMigrate(timeout)
}

func (c *serializedClient) Quit(timeout time.Duration) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.client.Quit(timeout)
}

func (c *serializedClient) Disconnect() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.client.Disconnect()
}
