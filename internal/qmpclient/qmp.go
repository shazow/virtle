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
	"github.com/shazow/virtle/internal/qmpwire"
)

// DefaultRPCTimeout bounds a single QMP operation when the dialer does not
// configure one.
const DefaultRPCTimeout = 5 * time.Second

// RawRunner runs raw QMP monitor commands.
type RawRunner interface {
	WithRaw(ctx context.Context, fn func(*rawQMP.Monitor) error) error
	RunRaw(ctx context.Context, command string) error
}

// DeviceController removes QEMU devices and waits for completion.
type DeviceController interface {
	DeviceDelAndWait(ctx context.Context, id string) error
}

// Lifecycle controls the VM run state.
type Lifecycle interface {
	Stop(ctx context.Context) error
	Cont(ctx context.Context) error
	QueryStatus(ctx context.Context) (string, error)
	Quit(ctx context.Context) error
}

// MigrationController runs QMP migration commands and queries their status.
type MigrationController interface {
	MigrateToFile(ctx context.Context, path string) error
	MigrateIncoming(ctx context.Context, path string) error
	QueryMigrate(ctx context.Context) (string, error)
}

// Disconnecter closes an open QMP connection.
type Disconnecter interface {
	Disconnect() error
}

// Client is a full QMP monitor connection. Operation deadlines are carried by
// ctx; each operation is additionally bounded by the transport's RPC timeout
// so a wedged monitor fails fast even when ctx has no deadline.
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
type SocketMonitorDialer struct {
	// RPCTimeout bounds each QMP operation regardless of the caller's ctx
	// deadline. Zero uses DefaultRPCTimeout.
	RPCTimeout time.Duration
}

type socketMonitorClient struct {
	monitor    *deadlineSocketMonitor
	raw        *rawQMP.Monitor
	rpcTimeout time.Duration
	mu         sync.Mutex
}

func (d *SocketMonitorDialer) Dial(ctx context.Context, socketPath string, timeout time.Duration) (Client, error) {
	if ctx.Err() != nil {
		return nil, context.Cause(ctx)
	}

	monitor, err := newDeadlineSocketMonitor(ctx, "unix", socketPath, timeout)
	if err != nil {
		if ctx.Err() != nil {
			return nil, context.Cause(ctx)
		}
		if qmpwire.IsTimeout(err) {
			return nil, fmt.Errorf("qmp connect timed out after %s", timeout)
		}
		return nil, err
	}

	monitor.setDeadline(time.Now().Add(timeout))
	// Cancel a blocked Connect by expiring the socket deadline; the ctx.Err
	// checks below own the case where cancellation raced a successful connect.
	stopInterrupt := context.AfterFunc(ctx, func() { _ = monitor.interrupt() })
	err = monitor.Connect()
	stopInterrupt()
	monitor.setDeadline(time.Time{})
	if err != nil {
		_ = monitor.Disconnect()
		if ctx.Err() != nil {
			return nil, context.Cause(ctx)
		}
		if qmpwire.IsTimeout(err) {
			return nil, fmt.Errorf("qmp connect timed out after %s", timeout)
		}
		return nil, err
	}
	if ctx.Err() != nil {
		_ = monitor.Disconnect()
		return nil, context.Cause(ctx)
	}

	rpcTimeout := d.RPCTimeout
	if rpcTimeout <= 0 {
		rpcTimeout = DefaultRPCTimeout
	}
	return &socketMonitorClient{
		monitor:    monitor,
		raw:        rawQMP.NewMonitor(monitor),
		rpcTimeout: rpcTimeout,
	}, nil
}

func (c *socketMonitorClient) WithRaw(ctx context.Context, fn func(*rawQMP.Monitor) error) error {
	return c.withDeadline(ctx, func() error {
		return fn(c.raw)
	})
}

func (c *socketMonitorClient) RunRaw(ctx context.Context, command string) error {
	err := c.withDeadline(ctx, func() error {
		if !json.Valid([]byte(command)) {
			return fmt.Errorf("invalid qmp json")
		}
		_, err := c.monitor.Run([]byte(command))
		return err
	})
	return c.opError(ctx, "qmp command", err)
}

func (c *socketMonitorClient) DeviceDelAndWait(ctx context.Context, id string) error {
	command, err := json.Marshal(map[string]any{
		"execute":   "device_del",
		"arguments": map[string]any{"id": id},
	})
	if err != nil {
		return err
	}
	err = c.withDeadline(ctx, func() error {
		c.monitor.mu.Lock()
		defer c.monitor.mu.Unlock()
		return c.monitor.withDeadline(func() error {
			if _, err := c.monitor.conn.Write(qmpwire.AppendDelimiter(command)); err != nil {
				return err
			}
			if _, err := c.monitor.readResponseLocked(); err != nil {
				return err
			}
			return c.monitor.waitDeviceDeletedLocked(id)
		})
	})
	return c.opError(ctx, fmt.Sprintf("qmp device_del %q", id), err)
}

func (c *socketMonitorClient) Quit(ctx context.Context) error {
	err := c.WithRaw(ctx, func(monitor *rawQMP.Monitor) error {
		if err := monitor.Quit(); err != nil {
			return fmt.Errorf("qmp quit: %w", err)
		}
		return nil
	})
	return c.opError(ctx, "qmp quit", err)
}

func (c *socketMonitorClient) Stop(ctx context.Context) error {
	err := c.WithRaw(ctx, func(monitor *rawQMP.Monitor) error {
		if err := monitor.Stop(); err != nil {
			return fmt.Errorf("qmp stop: %w", err)
		}
		return nil
	})
	return c.opError(ctx, "qmp stop", err)
}

func (c *socketMonitorClient) Cont(ctx context.Context) error {
	err := c.WithRaw(ctx, func(monitor *rawQMP.Monitor) error {
		if err := monitor.Cont(); err != nil {
			return fmt.Errorf("qmp cont: %w", err)
		}
		return nil
	})
	return c.opError(ctx, "qmp cont", err)
}

func (c *socketMonitorClient) QueryStatus(ctx context.Context) (string, error) {
	var status string
	err := c.WithRaw(ctx, func(monitor *rawQMP.Monitor) error {
		info, err := monitor.QueryStatus()
		if err != nil {
			return fmt.Errorf("qmp query-status: %w", err)
		}
		status = info.Status.String()
		return nil
	})
	if err != nil {
		return "", c.opError(ctx, "qmp query-status", err)
	}
	return status, nil
}

func (c *socketMonitorClient) MigrateToFile(ctx context.Context, path string) error {
	uri := "file:" + path
	err := c.WithRaw(ctx, func(monitor *rawQMP.Monitor) error {
		if err := monitor.Migrate(uri, nil, nil, nil); err != nil {
			return fmt.Errorf("qmp migrate %q: %w", uri, err)
		}
		return nil
	})
	return c.opError(ctx, fmt.Sprintf("qmp migrate %q", uri), err)
}

func (c *socketMonitorClient) MigrateIncoming(ctx context.Context, path string) error {
	uri := "file:" + path
	err := c.WithRaw(ctx, func(monitor *rawQMP.Monitor) error {
		if err := monitor.MigrateIncoming(uri); err != nil {
			return fmt.Errorf("qmp migrate-incoming %q: %w", uri, err)
		}
		return nil
	})
	return c.opError(ctx, fmt.Sprintf("qmp migrate-incoming %q", uri), err)
}

func (c *socketMonitorClient) QueryMigrate(ctx context.Context) (string, error) {
	var status string
	err := c.WithRaw(ctx, func(monitor *rawQMP.Monitor) error {
		info, err := monitor.QueryMigrate()
		if err != nil {
			return fmt.Errorf("qmp query-migrate: %w", err)
		}
		if info.Status != nil {
			status = info.Status.String()
		}
		return nil
	})
	if err != nil {
		return "", c.opError(ctx, "qmp query-migrate", err)
	}
	return status, nil
}

func (c *socketMonitorClient) Disconnect() error {
	if c == nil || c.monitor == nil {
		return nil
	}
	return c.monitor.Disconnect()
}

// withDeadline runs one QMP operation. The connection deadline is the earlier
// of the RPC timeout and the ctx deadline; ctx cancellation interrupts an
// in-flight read by yanking the deadline.
func (c *socketMonitorClient) withDeadline(ctx context.Context, fn func() error) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	deadline := time.Now().Add(c.rpcTimeout)
	if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
		deadline = d
	}
	c.monitor.setDeadline(deadline)
	defer c.monitor.setDeadline(time.Time{})

	stopc := make(chan struct{})
	stop := context.AfterFunc(ctx, func() {
		_ = c.monitor.interrupt()
		close(stopc)
	})
	// Wait for a fired AfterFunc before releasing the mutex so it cannot
	// clobber the deadline set by the next operation.
	defer func() {
		if !stop() {
			<-stopc
		}
	}()

	return fn()
}

// opError maps an operation failure to its cause: the caller's ctx ending,
// the RPC liveness bound expiring, or the raw error.
func (c *socketMonitorClient) opError(ctx context.Context, name string, err error) error {
	if err == nil {
		return nil
	}
	if ctx.Err() != nil {
		return fmt.Errorf("%s: %w", name, context.Cause(ctx))
	}
	if qmpwire.IsTimeout(err) {
		return fmt.Errorf("%s timed out after %s", name, c.rpcTimeout)
	}
	return err
}

type deadlineSocketMonitor struct {
	conn     net.Conn
	decoder  *json.Decoder
	deadline time.Time
	mu       sync.Mutex
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
		if _, err := m.conn.Write(qmpwire.AppendDelimiter(payload)); err != nil {
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
		if _, err := m.conn.Write(qmpwire.AppendDelimiter(command)); err != nil {
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

// setDeadline stores the absolute deadline applied to subsequent operations;
// the raw go-qemu monitor calls Run without a ctx, so the deadline travels
// through this field. Zero clears it.
func (m *deadlineSocketMonitor) setDeadline(deadline time.Time) {
	m.deadline = deadline
}

func (m *deadlineSocketMonitor) interrupt() error {
	if m == nil || m.conn == nil {
		return nil
	}
	return m.conn.SetDeadline(time.Now())
}

func (m *deadlineSocketMonitor) withDeadline(fn func() error) error {
	if !m.deadline.IsZero() {
		if err := m.conn.SetDeadline(m.deadline); err != nil {
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
