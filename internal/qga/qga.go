package qga

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"sync"
	"time"
)

// Pinger checks whether the guest agent is accepting commands.
type Pinger interface {
	Ping(timeout time.Duration) error
}

// FileWriter is the subset of guest-agent file operations needed to write a file.
type FileWriter interface {
	OpenFile(timeout time.Duration, path string) (int, error)
	WriteFile(timeout time.Duration, handle int, payloadBase64 string) error
	CloseFile(timeout time.Duration, handle int) error
}

// FileReader is the subset of guest-agent file operations needed to read a file.
type FileReader interface {
	OpenFileRead(timeout time.Duration, path string) (int, error)
	ReadFile(timeout time.Duration, handle int, count int) (string, bool, error)
	CloseFile(timeout time.Duration, handle int) error
}

// ExecRunner starts a guest process and polls its exit status.
type ExecRunner interface {
	Exec(timeout time.Duration, path string, args []string, captureOutput bool) (int, error)
	ExecStatus(timeout time.Duration, pid int) (ExecStatus, error)
}

// Disconnecter closes an open guest-agent connection.
type Disconnecter interface {
	Disconnect() error
}

// Client is a full guest-agent connection.
type Client interface {
	Pinger
	FileWriter
	FileReader
	ExecRunner
	Disconnecter
}

// Dialer opens guest-agent connections.
type Dialer interface {
	Dial(ctx context.Context, socketPath string, timeout time.Duration) (Client, error)
}

// SocketDialer opens guest-agent connections over Unix sockets.
type SocketDialer struct{}

type socketClient struct {
	conn    net.Conn
	decoder *json.Decoder
	mu      sync.Mutex
}

// ExecStatus is the guest-agent status response for an executed process.
type ExecStatus struct {
	Exited   bool
	ExitCode int
	OutData  string
	ErrData  string
}

func (d *SocketDialer) Dial(ctx context.Context, socketPath string, timeout time.Duration) (Client, error) {
	dialer := net.Dialer{Timeout: timeout}
	conn, err := dialer.DialContext(ctx, "unix", socketPath)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		if isTimeoutError(err) {
			return nil, fmt.Errorf("guest agent connect timed out after %s", timeout)
		}
		return nil, err
	}
	if ctx.Err() != nil {
		_ = conn.Close()
		return nil, ctx.Err()
	}

	return &socketClient{
		conn:    conn,
		decoder: json.NewDecoder(conn),
	}, nil
}

func (c *socketClient) Ping(timeout time.Duration) error {
	_, err := c.run(timeout, "guest-ping", nil)
	if err != nil {
		return fmt.Errorf("guest agent ping: %w", err)
	}
	return nil
}

func (c *socketClient) OpenFile(timeout time.Duration, path string) (int, error) {
	return c.openFile(timeout, path, "w")
}

func (c *socketClient) OpenFileRead(timeout time.Duration, path string) (int, error) {
	return c.openFile(timeout, path, "r")
}

func (c *socketClient) openFile(timeout time.Duration, path string, mode string) (int, error) {
	response, err := c.run(timeout, "guest-file-open", map[string]any{
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

func (c *socketClient) ReadFile(timeout time.Duration, handle int, count int) (string, bool, error) {
	response, err := c.run(timeout, "guest-file-read", map[string]any{
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

func (c *socketClient) WriteFile(timeout time.Duration, handle int, payloadBase64 string) error {
	_, err := c.run(timeout, "guest-file-write", map[string]any{
		"handle":  handle,
		"buf-b64": payloadBase64,
	})
	if err != nil {
		return fmt.Errorf("guest agent write handle %d: %w", handle, err)
	}
	return nil
}

func (c *socketClient) CloseFile(timeout time.Duration, handle int) error {
	_, err := c.run(timeout, "guest-file-close", map[string]any{
		"handle": handle,
	})
	if err != nil {
		return fmt.Errorf("guest agent close handle %d: %w", handle, err)
	}
	return nil
}

func (c *socketClient) Exec(timeout time.Duration, path string, args []string, captureOutput bool) (int, error) {
	response, err := c.run(timeout, "guest-exec", map[string]any{
		"path":           path,
		"arg":            args,
		"capture-output": captureOutput,
	})
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

func (c *socketClient) ExecStatus(timeout time.Duration, pid int) (ExecStatus, error) {
	response, err := c.run(timeout, "guest-exec-status", map[string]any{
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

func (c *socketClient) Disconnect() error {
	if c == nil || c.conn == nil {
		return nil
	}
	return c.conn.Close()
}

func (c *socketClient) run(timeout time.Duration, execute string, arguments map[string]any) (json.RawMessage, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if timeout > 0 {
		if err := c.conn.SetDeadline(time.Now().Add(timeout)); err != nil {
			return nil, err
		}
		defer c.conn.SetDeadline(time.Time{})
	}

	command := map[string]any{"execute": execute}
	if arguments != nil {
		command["arguments"] = arguments
	}
	payload, err := json.Marshal(command)
	if err != nil {
		return nil, err
	}
	if _, err := c.conn.Write(appendDelimiter(payload)); err != nil {
		return nil, err
	}

	for {
		var envelope struct {
			Return json.RawMessage `json:"return"`
			Event  string          `json:"event,omitempty"`
			Error  *struct {
				Class string `json:"class"`
				Desc  string `json:"desc"`
			} `json:"error,omitempty"`
		}
		if err := c.decoder.Decode(&envelope); err != nil {
			return nil, err
		}
		if envelope.Event != "" {
			continue
		}
		if envelope.Error != nil {
			if envelope.Error.Desc != "" {
				return nil, errors.New(envelope.Error.Desc)
			}
			return nil, fmt.Errorf("guest agent command %q failed with %s", execute, envelope.Error.Class)
		}
		return envelope.Return, nil
	}
}

func appendDelimiter(command []byte) []byte {
	if len(command) > 0 && command[len(command)-1] == '\n' {
		return command
	}
	return append(append([]byte(nil), command...), '\n')
}

func isTimeoutError(err error) bool {
	var netErr net.Error
	return errors.As(err, &netErr) && netErr.Timeout()
}
