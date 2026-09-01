package qga

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"time"

	"github.com/shazow/virtle/backend/qemu/internal/qmpwire"
	"github.com/shazow/virtle/backend/qemu/limits"
)

// DefaultRPCTimeout bounds a single guest-agent round trip when the dialer
// does not configure one.
const DefaultRPCTimeout = qmpwire.DefaultRPCTimeout

// Pinger checks whether the guest agent is accepting commands.
type Pinger interface {
	Ping(ctx context.Context) error
}

// FileWriter is the subset of guest-agent file operations needed to write a file.
type FileWriter interface {
	OpenFile(ctx context.Context, path string) (int, error)
	WriteFile(ctx context.Context, handle int, payloadBase64 string) error
	CloseFile(ctx context.Context, handle int) error
}

// FileReader is the subset of guest-agent file operations needed to read a file.
type FileReader interface {
	OpenFileRead(ctx context.Context, path string) (int, error)
	ReadFile(ctx context.Context, handle int, count int) (string, bool, error)
	CloseFile(ctx context.Context, handle int) error
}

// ExecRunner starts a guest process and polls its exit status.
type ExecRunner interface {
	Exec(ctx context.Context, path string, args []string, env []string, captureOutput bool) (int, error)
	ExecStatus(ctx context.Context, pid int) (ExecStatus, error)
}

// Shutdowner asks the guest operating system to power down.
type Shutdowner interface {
	Shutdown(ctx context.Context) error
}

// Disconnecter closes an open guest-agent connection.
type Disconnecter interface {
	Disconnect() error
}

// Client is a full guest-agent connection. Operation deadlines are carried by
// ctx; each individual round trip is additionally bounded by the transport's
// RPC timeout so a dead agent fails fast even when ctx has no deadline.
type Client interface {
	Pinger
	FileWriter
	FileReader
	ExecRunner
	Shutdowner
	Disconnecter
}

// Dialer opens guest-agent connections.
type Dialer interface {
	Dial(ctx context.Context, socketPath string, timeout time.Duration) (Client, error)
}

// SocketDialer opens guest-agent connections over Unix sockets.
type SocketDialer struct {
	// RPCTimeout bounds each guest-agent round trip regardless of the caller's
	// ctx deadline, so a wedged agent fails fast even when the command itself
	// has no time limit. Zero uses DefaultRPCTimeout.
	RPCTimeout time.Duration
	// MaxFrameSize bounds one guest-agent response. Zero uses
	// limits.DefaultMaxFrameSize.
	MaxFrameSize int64
	// MaxCommandOutputSize bounds the combined encoded stdout and stderr in a
	// guest-exec-status response. Zero uses
	// limits.DefaultMaxCommandOutputSize.
	MaxCommandOutputSize int64
}

type socketClient struct {
	conn                 net.Conn
	decoder              *json.Decoder
	session              *qmpwire.Session
	maxCommandOutputSize int64
}

// ExecStatus is the guest-agent status response for an executed process.
type ExecStatus struct {
	Exited   bool
	ExitCode int
	OutData  string
	ErrData  string
}

func (d *SocketDialer) Dial(ctx context.Context, socketPath string, timeout time.Duration) (Client, error) {
	conn, err := qmpwire.DialUnix(ctx, socketPath, timeout, "guest agent")
	if err != nil {
		return nil, err
	}

	rpcTimeout := d.RPCTimeout
	if rpcTimeout <= 0 {
		rpcTimeout = DefaultRPCTimeout
	}
	reader := bufio.NewReader(conn)
	maxFrameSize := d.MaxFrameSize
	if maxFrameSize <= 0 {
		maxFrameSize = limits.DefaultMaxFrameSize
	}
	maxCommandOutputSize := d.MaxCommandOutputSize
	if maxCommandOutputSize <= 0 {
		maxCommandOutputSize = limits.DefaultMaxCommandOutputSize
	}
	client := &socketClient{
		conn:                 conn,
		session:              &qmpwire.Session{Conn: conn, RPCTimeout: rpcTimeout},
		maxCommandOutputSize: maxCommandOutputSize,
	}
	if err := client.synchronize(ctx, reader, maxFrameSize); err != nil {
		return nil, errors.Join(fmt.Errorf("synchronize guest agent: %w", err), conn.Close())
	}
	client.decoder = qmpwire.NewDecoder(reader, maxFrameSize)
	return client, nil
}

// synchronize discards replies left in QGA's virtio-serial stream by an
// earlier host connection. Without guest-sync-delimited, a fresh client can
// mistake a stale empty reply for guest-exec's PID response.
func (c *socketClient) synchronize(ctx context.Context, reader *bufio.Reader, maxFrameSize int64) error {
	const syncID int64 = 0x564952544c45 // "VIRTLE"
	payload, err := json.Marshal(map[string]any{
		"execute":   "guest-sync-delimited",
		"arguments": map[string]any{"id": syncID},
	})
	if err != nil {
		return err
	}

	return c.session.Do(ctx, func() error {
		command := append([]byte{0xff}, qmpwire.AppendDelimiter(payload)...)
		if _, err := c.conn.Write(command); err != nil {
			return &qmpwire.WireError{Err: err}
		}
		for {
			// QGA prefixes every guest-sync-delimited response with 0xff. Scan
			// for that sentinel before parsing JSON so stale or partial data
			// cannot poison the decoder.
			if _, err := readDelimited(reader, 0xff, maxFrameSize, false); err != nil {
				return &qmpwire.WireError{Err: err}
			}
			line, err := readDelimited(reader, '\n', maxFrameSize, true)
			if err != nil {
				return &qmpwire.WireError{Err: err}
			}
			var envelope qmpwire.Envelope
			if json.Unmarshal(line, &envelope) != nil {
				continue
			}
			var returned int64
			if json.Unmarshal(envelope.Return, &returned) == nil && returned == syncID {
				return nil
			}
		}
	})
}

// readDelimited consumes one bounded segment without letting bufio allocate in
// proportion to an untrusted peer's input. The delimiter is not returned.
func readDelimited(reader *bufio.Reader, delimiter byte, limit int64, retain bool) ([]byte, error) {
	var result []byte
	for size := int64(0); ; size++ {
		b, err := reader.ReadByte()
		if err != nil {
			return nil, err
		}
		if b == delimiter {
			return result, nil
		}
		if size >= limit {
			return nil, &limits.Error{Resource: "QEMU control frame", Limit: limit}
		}
		if retain {
			result = append(result, b)
		}
	}
}

func (c *socketClient) Ping(ctx context.Context) error {
	_, err := c.run(ctx, "guest-ping", nil)
	if err != nil {
		return fmt.Errorf("guest agent ping: %w", err)
	}
	return nil
}

func (c *socketClient) OpenFile(ctx context.Context, path string) (int, error) {
	return c.openFile(ctx, path, "w")
}

func (c *socketClient) OpenFileRead(ctx context.Context, path string) (int, error) {
	return c.openFile(ctx, path, "r")
}

func (c *socketClient) openFile(ctx context.Context, path string, mode string) (int, error) {
	response, err := c.run(ctx, "guest-file-open", map[string]any{
		"path": path,
		"mode": mode,
	})
	if err != nil {
		return 0, fmt.Errorf("guest agent open %q: %w", path, err)
	}

	var handle int
	if err := json.Unmarshal(response, &handle); err != nil {
		return 0, fmt.Errorf("guest agent open %q returned invalid handle: %w", path, err)
	}
	return handle, nil
}

func (c *socketClient) ReadFile(ctx context.Context, handle int, count int) (string, bool, error) {
	response, err := c.run(ctx, "guest-file-read", map[string]any{
		"handle": handle,
		"count":  count,
	})
	if err != nil {
		return "", false, fmt.Errorf("guest agent read handle %d: %w", handle, err)
	}

	var result struct {
		BufB64 string `json:"buf-b64"`
		EOF    bool   `json:"eof"`
	}
	if err := json.Unmarshal(response, &result); err != nil {
		return "", false, fmt.Errorf("guest agent read handle %d returned invalid payload: %w", handle, err)
	}
	return result.BufB64, result.EOF, nil
}

func (c *socketClient) WriteFile(ctx context.Context, handle int, payloadBase64 string) error {
	_, err := c.run(ctx, "guest-file-write", map[string]any{
		"handle":  handle,
		"buf-b64": payloadBase64,
	})
	if err != nil {
		return fmt.Errorf("guest agent write handle %d: %w", handle, err)
	}
	return nil
}

func (c *socketClient) CloseFile(ctx context.Context, handle int) error {
	_, err := c.run(ctx, "guest-file-close", map[string]any{
		"handle": handle,
	})
	if err != nil {
		return fmt.Errorf("guest agent close handle %d: %w", handle, err)
	}
	return nil
}

func (c *socketClient) Exec(ctx context.Context, path string, args []string, env []string, captureOutput bool) (int, error) {
	arguments := map[string]any{
		"path":           path,
		"arg":            args,
		"capture-output": captureOutput,
	}
	if len(env) > 0 {
		arguments["env"] = env
	}
	response, err := c.run(ctx, "guest-exec", arguments)
	if err != nil {
		return 0, fmt.Errorf("guest agent exec %q: %w", path, err)
	}

	var result struct {
		PID int `json:"pid"`
	}
	if err := json.Unmarshal(response, &result); err != nil {
		return 0, fmt.Errorf("guest agent exec %q returned invalid pid: %w", path, err)
	}
	if result.PID <= 0 {
		return 0, fmt.Errorf("guest agent exec %q returned invalid pid %d", path, result.PID)
	}
	return result.PID, nil
}

func (c *socketClient) ExecStatus(ctx context.Context, pid int) (ExecStatus, error) {
	response, err := c.run(ctx, "guest-exec-status", map[string]any{
		"pid": pid,
	})
	if err != nil {
		return ExecStatus{}, fmt.Errorf("guest agent exec-status pid %d: %w", pid, err)
	}

	var result struct {
		Exited bool `json:"exited"`
		// Pointer preserves absent exitcode while a guest command is still running.
		ExitCode *int   `json:"exitcode,omitempty"`
		OutData  string `json:"out-data,omitempty"`
		ErrData  string `json:"err-data,omitempty"`
	}
	if err := json.Unmarshal(response, &result); err != nil {
		return ExecStatus{}, fmt.Errorf("guest agent exec-status pid %d returned invalid status: %w", pid, err)
	}
	if int64(len(result.OutData))+int64(len(result.ErrData)) > c.maxCommandOutputSize {
		return ExecStatus{}, &limits.Error{Resource: "guest command output", Limit: c.maxCommandOutputSize}
	}
	status := ExecStatus{
		Exited:  result.Exited,
		OutData: result.OutData,
		ErrData: result.ErrData,
	}
	if result.ExitCode != nil {
		status.ExitCode = *result.ExitCode
	}
	return status, nil
}

// Shutdown asks the guest to power down. The guest often powers off without
// answering, so a missing or truncated response counts as success; callers
// bound the wait through ctx.
func (c *socketClient) Shutdown(ctx context.Context) error {
	_, err := c.run(ctx, "guest-shutdown", map[string]any{"mode": "powerdown"})
	if err == nil || errors.Is(err, context.DeadlineExceeded) || qmpwire.IsTimeout(err) || errors.Is(err, io.EOF) || errors.Is(err, net.ErrClosed) {
		return nil
	}
	return fmt.Errorf("guest agent shutdown: %w", err)
}

func (c *socketClient) Disconnect() error {
	if c == nil || c.conn == nil {
		return nil
	}
	return c.conn.Close()
}

// run issues one guest-agent round trip through the session, which owns
// deadline arbitration, cancellation interrupts, and poisoning the connection
// once its stream can no longer be trusted.
func (c *socketClient) run(ctx context.Context, execute string, arguments map[string]any) (json.RawMessage, error) {
	command := map[string]any{"execute": execute}
	if arguments != nil {
		command["arguments"] = arguments
	}
	payload, err := json.Marshal(command)
	if err != nil {
		return nil, err
	}

	var response json.RawMessage
	err = c.session.Do(ctx, func() error {
		if _, err := c.conn.Write(qmpwire.AppendDelimiter(payload)); err != nil {
			return &qmpwire.WireError{Err: err}
		}
		for {
			envelope, err := qmpwire.DecodeEnvelope(c.decoder)
			if err != nil {
				return err
			}
			if envelope.Event != "" {
				continue
			}
			if envelope.Error != nil {
				if envelope.Error.Desc != "" {
					return envelope.Error
				}
				return fmt.Errorf("guest agent command %q failed with %s", execute, envelope.Error.Class)
			}
			response = envelope.Return
			return nil
		}
	})
	if err != nil {
		return nil, c.wireError(ctx, err)
	}
	return response, nil
}

// wireError maps a failure to its cause: the caller's ctx ending, the RPC
// liveness bound expiring, or the raw error.
func (c *socketClient) wireError(ctx context.Context, err error) error {
	if ctx.Err() != nil && !errors.Is(err, qmpwire.ErrBroken) {
		return context.Cause(ctx)
	}
	if qmpwire.IsTimeout(err) {
		return fmt.Errorf("guest agent unresponsive after %s", c.session.RPCTimeout)
	}
	return err
}
