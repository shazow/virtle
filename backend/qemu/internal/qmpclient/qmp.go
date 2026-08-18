package qmpclient

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"time"

	doQMP "github.com/digitalocean/go-qemu/qmp"
	rawQMP "github.com/digitalocean/go-qemu/qmp/raw"
	"github.com/shazow/virtle/backend/qemu/internal/qmpwire"
)

// DefaultRPCTimeout bounds a single QMP operation when the dialer does not
// configure one.
const DefaultRPCTimeout = qmpwire.DefaultRPCTimeout

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
	monitor *socketMonitor
	raw     *rawQMP.Monitor
	session *qmpwire.Session
}

func (d *SocketMonitorDialer) Dial(ctx context.Context, socketPath string, timeout time.Duration) (Client, error) {
	conn, err := qmpwire.DialUnix(ctx, socketPath, timeout, "qmp")
	if err != nil {
		return nil, err
	}
	monitor := &socketMonitor{
		conn:    conn,
		decoder: json.NewDecoder(conn),
	}

	// The handshake runs through a session bounded by the connect timeout so
	// ctx cancellation can interrupt a blocked banner read.
	session := &qmpwire.Session{Conn: conn, RPCTimeout: timeout}
	if err := session.Do(ctx, monitor.Connect); err != nil {
		_ = monitor.Disconnect()
		return nil, qmpwire.DialError(ctx, err, "qmp", timeout)
	}
	if ctx.Err() != nil {
		_ = monitor.Disconnect()
		return nil, context.Cause(ctx)
	}

	session.RPCTimeout = d.RPCTimeout
	if session.RPCTimeout <= 0 {
		session.RPCTimeout = DefaultRPCTimeout
	}
	return &socketMonitorClient{
		monitor: monitor,
		raw:     rawQMP.NewMonitor(monitor),
		session: session,
	}, nil
}

func (c *socketMonitorClient) WithRaw(ctx context.Context, fn func(*rawQMP.Monitor) error) error {
	return c.session.Do(ctx, func() error {
		return fn(c.raw)
	})
}

func (c *socketMonitorClient) RunRaw(ctx context.Context, command string) error {
	err := c.session.Do(ctx, func() error {
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
	err = c.session.Do(ctx, func() error {
		if _, err := c.monitor.conn.Write(qmpwire.AppendDelimiter(command)); err != nil {
			return &qmpwire.WireError{Err: err}
		}
		if _, err := c.monitor.readResponse(); err != nil {
			return err
		}
		return c.monitor.waitDeviceDeleted(id)
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
	// Deviation from the per-RPC liveness bound: QMP can hold the reply to
	// migrate until the file-backed state transfer finishes, which takes
	// longer than an ordinary round trip. The caller's ctx (the migration
	// timeout) is the only bound here.
	err := c.session.DoSlow(ctx, func() error {
		if err := c.raw.Migrate(uri, nil, nil, nil); err != nil {
			return fmt.Errorf("qmp migrate %q: %w", uri, err)
		}
		return nil
	})
	return c.opError(ctx, fmt.Sprintf("qmp migrate %q", uri), err)
}

func (c *socketMonitorClient) MigrateIncoming(ctx context.Context, path string) error {
	uri := "file:" + path
	// Deviation from the per-RPC liveness bound: like migrate, the reply can
	// lag behind reading the saved state, so only ctx bounds this operation.
	err := c.session.DoSlow(ctx, func() error {
		if err := c.raw.MigrateIncoming(uri); err != nil {
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

// opError maps an operation failure to its cause: the caller's ctx ending,
// the RPC liveness bound expiring, or the raw error.
func (c *socketMonitorClient) opError(ctx context.Context, name string, err error) error {
	if err == nil {
		return nil
	}
	if ctx.Err() != nil && !errors.Is(err, qmpwire.ErrBroken) {
		return fmt.Errorf("%s: %w", name, context.Cause(ctx))
	}
	if qmpwire.IsTimeout(err) {
		return fmt.Errorf("%s timed out after %s", name, c.session.RPCTimeout)
	}
	return err
}

// socketMonitor adapts the shared unix socket to go-qemu's raw monitor
// interface. It only frames commands and decodes replies: deadlines,
// cancellation, and serialization are owned by the client's Session, so every
// method must be called inside Session.Do/DoSlow.
type socketMonitor struct {
	conn    net.Conn
	decoder *json.Decoder
}

func (m *socketMonitor) Connect() error {
	var banner struct {
		QMP struct {
			Version      doQMP.Version `json:"version"`
			Capabilities []string      `json:"capabilities"`
		} `json:"QMP"`
	}
	if err := m.decoder.Decode(&banner); err != nil {
		return &qmpwire.WireError{Err: err}
	}

	payload, err := json.Marshal(doQMP.Command{Execute: "qmp_capabilities"})
	if err != nil {
		return err
	}
	if _, err := m.conn.Write(qmpwire.AppendDelimiter(payload)); err != nil {
		return &qmpwire.WireError{Err: err}
	}

	_, err = m.readResponse()
	return err
}

func (m *socketMonitor) Disconnect() error {
	if m == nil || m.conn == nil {
		return nil
	}
	return m.conn.Close()
}

func (m *socketMonitor) Run(command []byte) ([]byte, error) {
	if _, err := m.conn.Write(qmpwire.AppendDelimiter(command)); err != nil {
		return nil, &qmpwire.WireError{Err: err}
	}
	return m.readResponse()
}

func (m *socketMonitor) Events(context.Context) (<-chan doQMP.Event, error) {
	return nil, doQMP.ErrEventsNotSupported
}

func (m *socketMonitor) readResponse() ([]byte, error) {
	for {
		envelope, err := qmpwire.DecodeRawEnvelope(m.decoder)
		if err != nil {
			return nil, err
		}
		if envelope.Event != "" {
			continue
		}
		if envelope.Error != nil && envelope.Error.Desc != "" {
			return nil, envelope.Error
		}
		return envelope.Raw, nil
	}
}

func (m *socketMonitor) waitDeviceDeleted(id string) error {
	for {
		envelope, err := qmpwire.DecodeEnvelope(m.decoder)
		if err != nil {
			return err
		}
		if envelope.Error != nil && envelope.Error.Desc != "" {
			return envelope.Error
		}
		if envelope.Event != "DEVICE_DELETED" {
			continue
		}
		var data struct {
			Device string `json:"device,omitempty"`
		}
		if err := json.Unmarshal(envelope.Data, &data); err != nil {
			return &qmpwire.WireError{Err: err}
		}
		if data.Device == id {
			return nil
		}
	}
}
