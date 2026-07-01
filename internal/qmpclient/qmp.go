package qmpclient

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"sync"
	"time"

	doQMP "github.com/digitalocean/go-qemu/qmp"
	rawQMP "github.com/digitalocean/go-qemu/qmp/raw"
)

const (
	defaultQMPRetryDelay       = 200 * time.Millisecond
	defaultQMPConnectTimeout   = 500 * time.Millisecond
	defaultQMPQuitTimeout      = 500 * time.Millisecond
	defaultQMPMigrationTimeout = 30 * time.Second
)

// RawRunner runs raw QMP monitor commands.
type RawRunner interface {
	WithRaw(timeout time.Duration, fn func(*rawQMP.Monitor) error) error
	RunRaw(timeout time.Duration, command string) error
}

// DeviceController removes QEMU devices and waits for completion.
type DeviceController interface {
	DeviceDelAndWait(timeout time.Duration, id string) error
}

// Lifecycle controls the VM run state.
type Lifecycle interface {
	Stop(timeout time.Duration) error
	Cont(timeout time.Duration) error
	QueryStatus(timeout time.Duration) (string, error)
	Quit(timeout time.Duration) error
}

// MigrationController runs QMP migration commands and queries their status.
type MigrationController interface {
	MigrateToFile(timeout time.Duration, path string) error
	MigrateIncoming(timeout time.Duration, path string) error
	QueryMigrate(timeout time.Duration) (string, error)
}

// Disconnecter closes an open QMP connection.
type Disconnecter interface {
	Disconnect() error
}

// Client is a full QMP monitor connection.
type Client interface {
	RawRunner
	DeviceController
	Lifecycle
	MigrationController
	Disconnecter
}

// Dialer opens QMP monitor connections.
type Dialer interface {
	Dial(ctx context.Context, socketPath string, timeout time.Duration) (Client, error)
}

// SocketMonitorDialer opens QMP monitor connections over Unix sockets.
type SocketMonitorDialer struct{}

type socketMonitorClient struct {
	monitor *deadlineSocketMonitor
	raw     *rawQMP.Monitor
	mu      sync.Mutex
}

func (d *SocketMonitorDialer) Dial(ctx context.Context, socketPath string, timeout time.Duration) (Client, error) {
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}

	monitor, err := newDeadlineSocketMonitor(ctx, "unix", socketPath, timeout)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		if isTimeoutError(err) {
			return nil, fmt.Errorf("qmp connect timed out after %s", timeout)
		}
		return nil, err
	}

	monitor.setTimeout(timeout)
	interruptDone := make(chan struct{})
	stopInterrupt := make(chan struct{})
	go func() {
		defer close(interruptDone)
		select {
		case <-ctx.Done():
			_ = monitor.interrupt()
		case <-stopInterrupt:
		}
	}()

	err = monitor.Connect()
	close(stopInterrupt)
	<-interruptDone
	if err != nil {
		_ = monitor.Disconnect()
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		if isTimeoutError(err) {
			return nil, fmt.Errorf("qmp connect timed out after %s", timeout)
		}
		return nil, err
	}
	if ctx.Err() != nil {
		_ = monitor.Disconnect()
		return nil, ctx.Err()
	}

	return &socketMonitorClient{
		monitor: monitor,
		raw:     rawQMP.NewMonitor(monitor),
	}, nil
}

func (c *socketMonitorClient) WithRaw(timeout time.Duration, fn func(*rawQMP.Monitor) error) error {
	return c.withTimeout(timeout, func() error {
		return fn(c.raw)
	})
}

func (c *socketMonitorClient) RunRaw(timeout time.Duration, command string) error {
	err := c.withTimeout(timeout, func() error {
		if !json.Valid([]byte(command)) {
			return fmt.Errorf("invalid qmp json")
		}
		_, err := c.monitor.Run([]byte(command))
		return err
	})
	if isTimeoutError(err) {
		return fmt.Errorf("qmp command timed out after %s", timeout)
	}
	return err
}

func (c *socketMonitorClient) DeviceDelAndWait(timeout time.Duration, id string) error {
	command, err := json.Marshal(map[string]any{
		"execute":   "device_del",
		"arguments": map[string]any{"id": id},
	})
	if err != nil {
		return err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.monitor.setTimeout(timeout)
	defer c.monitor.setTimeout(0)

	c.monitor.mu.Lock()
	defer c.monitor.mu.Unlock()
	err = c.monitor.withDeadline(func() error {
		if _, err := c.monitor.conn.Write(appendQMPDelimiter(command)); err != nil {
			return err
		}
		if _, err := c.monitor.readResponseLocked(); err != nil {
			return err
		}
		return c.monitor.waitDeviceDeletedLocked(id)
	})
	if isTimeoutError(err) {
		return fmt.Errorf("qmp device_del %q timed out after %s", id, timeout)
	}
	return err
}

func (c *socketMonitorClient) Quit(timeout time.Duration) error {
	err := c.WithRaw(timeout, func(monitor *rawQMP.Monitor) error {
		if err := monitor.Quit(); err != nil {
			return fmt.Errorf("qmp quit: %w", err)
		}
		return nil
	})
	if isTimeoutError(err) {
		return fmt.Errorf("qmp quit timed out after %s", timeout)
	}
	return err
}

func (c *socketMonitorClient) Stop(timeout time.Duration) error {
	err := c.WithRaw(timeout, func(monitor *rawQMP.Monitor) error {
		if err := monitor.Stop(); err != nil {
			return fmt.Errorf("qmp stop: %w", err)
		}
		return nil
	})
	if isTimeoutError(err) {
		return fmt.Errorf("qmp stop timed out after %s", timeout)
	}
	return err
}

func (c *socketMonitorClient) Cont(timeout time.Duration) error {
	err := c.WithRaw(timeout, func(monitor *rawQMP.Monitor) error {
		if err := monitor.Cont(); err != nil {
			return fmt.Errorf("qmp cont: %w", err)
		}
		return nil
	})
	if isTimeoutError(err) {
		return fmt.Errorf("qmp cont timed out after %s", timeout)
	}
	return err
}

func (c *socketMonitorClient) QueryStatus(timeout time.Duration) (string, error) {
	var status string
	err := c.WithRaw(timeout, func(monitor *rawQMP.Monitor) error {
		info, err := monitor.QueryStatus()
		if err != nil {
			return fmt.Errorf("qmp query-status: %w", err)
		}
		status = info.Status.String()
		return nil
	})
	if isTimeoutError(err) {
		return "", fmt.Errorf("qmp query-status timed out after %s", timeout)
	}
	return status, err
}

func (c *socketMonitorClient) MigrateToFile(timeout time.Duration, path string) error {
	uri := "file:" + path
	err := c.WithRaw(timeout, func(monitor *rawQMP.Monitor) error {
		if err := monitor.Migrate(uri, nil, nil, nil); err != nil {
			return fmt.Errorf("qmp migrate %q: %w", uri, err)
		}
		return nil
	})
	if isTimeoutError(err) {
		return fmt.Errorf("qmp migrate %q timed out after %s", uri, timeout)
	}
	return err
}

func (c *socketMonitorClient) MigrateIncoming(timeout time.Duration, path string) error {
	uri := "file:" + path
	err := c.WithRaw(timeout, func(monitor *rawQMP.Monitor) error {
		if err := monitor.MigrateIncoming(uri); err != nil {
			return fmt.Errorf("qmp migrate-incoming %q: %w", uri, err)
		}
		return nil
	})
	if isTimeoutError(err) {
		return fmt.Errorf("qmp migrate-incoming %q timed out after %s", uri, timeout)
	}
	return err
}

func (c *socketMonitorClient) QueryMigrate(timeout time.Duration) (string, error) {
	var status string
	err := c.WithRaw(timeout, func(monitor *rawQMP.Monitor) error {
		info, err := monitor.QueryMigrate()
		if err != nil {
			return fmt.Errorf("qmp query-migrate: %w", err)
		}
		if info.Status != nil {
			status = info.Status.String()
		}
		return nil
	})
	if isTimeoutError(err) {
		return "", fmt.Errorf("qmp query-migrate timed out after %s", timeout)
	}
	return status, err
}

func (c *socketMonitorClient) Disconnect() error {
	if c == nil || c.monitor == nil {
		return nil
	}
	return c.monitor.Disconnect()
}

func (c *socketMonitorClient) withTimeout(timeout time.Duration, fn func() error) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.monitor.setTimeout(timeout)
	defer c.monitor.setTimeout(0)

	return fn()
}

type deadlineSocketMonitor struct {
	conn    net.Conn
	decoder *json.Decoder
	timeout time.Duration
	mu      sync.Mutex
}

func newDeadlineSocketMonitor(ctx context.Context, network string, addr string, timeout time.Duration) (*deadlineSocketMonitor, error) {
	dialer := net.Dialer{Timeout: timeout}
	conn, err := dialer.DialContext(ctx, network, addr)
	if err != nil {
		return nil, err
	}

	return &deadlineSocketMonitor{
		conn:    conn,
		decoder: json.NewDecoder(conn),
		timeout: timeout,
	}, nil
}

func (m *deadlineSocketMonitor) Connect() error {
	m.mu.Lock()
	defer m.mu.Unlock()

	return m.withDeadline(func() error {
		var banner struct {
			QMP struct {
				Version      doQMP.Version `json:"version"`
				Capabilities []string      `json:"capabilities"`
			} `json:"QMP"`
		}
		if err := m.decoder.Decode(&banner); err != nil {
			return err
		}

		payload, err := json.Marshal(doQMP.Command{Execute: "qmp_capabilities"})
		if err != nil {
			return err
		}
		if _, err := m.conn.Write(appendQMPDelimiter(payload)); err != nil {
			return err
		}

		_, err = m.readResponseLocked()
		return err
	})
}

func (m *deadlineSocketMonitor) Disconnect() error {
	if m == nil || m.conn == nil {
		return nil
	}
	return m.conn.Close()
}

func (m *deadlineSocketMonitor) Run(command []byte) ([]byte, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	var response []byte
	err := m.withDeadline(func() error {
		if _, err := m.conn.Write(appendQMPDelimiter(command)); err != nil {
			return err
		}

		var err error
		response, err = m.readResponseLocked()
		return err
	})
	if err != nil {
		return nil, err
	}
	return response, nil
}

func (m *deadlineSocketMonitor) Events(context.Context) (<-chan doQMP.Event, error) {
	return nil, doQMP.ErrEventsNotSupported
}

func (m *deadlineSocketMonitor) setTimeout(timeout time.Duration) {
	m.timeout = timeout
}

func (m *deadlineSocketMonitor) interrupt() error {
	if m == nil || m.conn == nil {
		return nil
	}
	return m.conn.SetDeadline(time.Now())
}

func (m *deadlineSocketMonitor) withDeadline(fn func() error) error {
	if m.timeout > 0 {
		if err := m.conn.SetDeadline(time.Now().Add(m.timeout)); err != nil {
			return err
		}
		defer m.conn.SetDeadline(time.Time{})
	}
	return fn()
}

func (m *deadlineSocketMonitor) readResponseLocked() ([]byte, error) {
	for {
		var message json.RawMessage
		if err := m.decoder.Decode(&message); err != nil {
			return nil, err
		}

		var envelope struct {
			Event string `json:"event,omitempty"`
			Error *struct {
				Class string `json:"class"`
				Desc  string `json:"desc"`
			} `json:"error,omitempty"`
		}
		if err := json.Unmarshal(message, &envelope); err != nil {
			return nil, err
		}
		if envelope.Event != "" {
			continue
		}
		if envelope.Error != nil && envelope.Error.Desc != "" {
			return nil, errors.New(envelope.Error.Desc)
		}
		return message, nil
	}
}

func (m *deadlineSocketMonitor) waitDeviceDeletedLocked(id string) error {
	for {
		var message json.RawMessage
		if err := m.decoder.Decode(&message); err != nil {
			return err
		}

		var envelope struct {
			Event string `json:"event,omitempty"`
			Data  struct {
				Device string `json:"device,omitempty"`
			} `json:"data,omitempty"`
			Error *struct {
				Desc string `json:"desc"`
			} `json:"error,omitempty"`
		}
		if err := json.Unmarshal(message, &envelope); err != nil {
			return err
		}
		if envelope.Error != nil && envelope.Error.Desc != "" {
			return errors.New(envelope.Error.Desc)
		}
		if envelope.Event == "DEVICE_DELETED" && envelope.Data.Device == id {
			return nil
		}
	}
}

func appendQMPDelimiter(command []byte) []byte {
	if len(command) > 0 && command[len(command)-1] == '\n' {
		return command
	}
	return append(append([]byte(nil), command...), '\n')
}

func isTimeoutError(err error) bool {
	var netErr net.Error
	return errors.As(err, &netErr) && netErr.Timeout()
}
