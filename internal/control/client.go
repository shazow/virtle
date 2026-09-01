package control

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"sync"
	"time"

	"github.com/shazow/virtle/backend"
	"github.com/shazow/virtle/units"
	"github.com/shazow/virtle/vm"
)

// Dial connects to a control socket and returns its remote machine. Dial
// discovers server capabilities before returning and begins observing machine
// exit immediately.
func Dial(ctx context.Context, path string) (backend.Machine, error) {
	c := &client{dial: func(ctx context.Context) (net.Conn, error) {
		var d net.Dialer
		return d.DialContext(ctx, "unix", path)
	}}
	methods, err := callTyped[MethodsRequest, MethodsResponse](c, ctx, rpcMethods, MethodsRequest{})
	if err != nil {
		return nil, err
	}
	m := &machine{client: c, methods: make(map[rpcMethod]bool, len(methods.Methods)), done: make(chan struct{})}
	for _, method := range methods.Methods {
		m.methods[rpcMethod(method)] = true
	}
	go m.observeExit()
	return m, nil
}

// Raw sends a debugging request without interpreting its result.
func Raw(ctx context.Context, path, method string, params json.RawMessage) (json.RawMessage, error) {
	c := &client{dial: func(ctx context.Context) (net.Conn, error) {
		var d net.Dialer
		return d.DialContext(ctx, "unix", path)
	}}
	var resp json.RawMessage
	err := c.call(ctx, rpcMethod(method), params, &resp)
	return resp, err
}

type machine struct {
	client  *client
	methods map[rpcMethod]bool
	done    chan struct{}
	once    sync.Once
	mu      sync.Mutex
	err     error
}

func (m *machine) supports(method rpcMethod) error {
	if m.methods[method] {
		return nil
	}
	return fmt.Errorf("control method %q: %w", method, errors.ErrUnsupported)
}

func (m *machine) finish(err error) {
	m.once.Do(func() {
		m.mu.Lock()
		m.err = err
		m.mu.Unlock()
		close(m.done)
	})
}

func (m *machine) observeExit() {
	if m.methods[rpcWait] {
		_, err := callTyped[WaitRequest, WaitResponse](m.client, context.Background(), rpcWait, WaitRequest{})
		m.finish(err)
		return
	}

	ticker := time.NewTicker(250 * time.Millisecond)
	defer ticker.Stop()
	for {
		status, err := callTyped[StatusRequest, StatusResponse](m.client, context.Background(), rpcStatus, StatusRequest{})
		if err != nil {
			m.finish(fmt.Errorf("machine exited without status: %w", err))
			return
		}
		if status.State == backend.StateStopped {
			m.finish(nil)
			return
		}
		<-ticker.C
	}
}

func (m *machine) Done() <-chan struct{} { return m.done }

func (m *machine) Err() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.err
}

func (m *machine) Wait(ctx context.Context) error {
	select {
	case <-m.done:
		return m.Err()
	case <-ctx.Done():
		return context.Cause(ctx)
	}
}

func (m *machine) Kill() error {
	if err := m.supports(rpcKill); err != nil {
		return err
	}
	_, err := callTyped[KillRequest, KillResponse](m.client, context.Background(), rpcKill, KillRequest{})
	return err
}

func (m *machine) Shutdown(ctx context.Context) error {
	if m.methods[rpcShutdown] {
		_, err := callTyped[ShutdownRequest, ShutdownResponse](m.client, ctx, rpcShutdown, ShutdownRequest{})
		return err
	}
	g, err := m.RemoteControl()
	if err != nil {
		return m.Kill()
	}
	if err := g.Shutdown(ctx); err != nil {
		return m.Kill()
	}
	if err := m.Wait(ctx); err != nil && ctx.Err() != nil {
		return errors.Join(err, m.Kill())
	} else {
		return err
	}
}

func (m *machine) Suspend(ctx context.Context) error {
	if err := m.supports(rpcSuspend); err != nil {
		return err
	}
	_, err := callTyped[SuspendRequest, SuspendResponse](m.client, ctx, rpcSuspend, SuspendRequest{})
	return err
}

func (m *machine) ResizeMemory(ctx context.Context, size units.Bytes) error {
	if err := m.supports(rpcBalloon); err != nil {
		return err
	}
	_, err := callTyped[BalloonRequest, BalloonResponse](m.client, ctx, rpcBalloon, BalloonRequest{TargetBytes: size.Int64()})
	return err
}

func deviceRequest(dev vm.Device) (*DeviceRequest, error) {
	switch d := dev.(type) {
	case vm.Share:
		return &DeviceRequest{Share: &d}, nil
	case vm.Disk:
		return &DeviceRequest{Disk: &d}, nil
	case vm.Forward:
		return &DeviceRequest{Forward: &d}, nil
	default:
		return nil, fmt.Errorf("unsupported device type %T", dev)
	}
}

func (m *machine) changeDevice(ctx context.Context, dev vm.Device, detach bool) error {
	if err := m.supports(rpcHotplug); err != nil {
		return err
	}
	req, err := deviceRequest(dev)
	if err != nil {
		return err
	}
	_, err = callTyped[HotplugRequest, HotplugResponse](m.client, ctx, rpcHotplug, HotplugRequest{Detach: detach, Device: req})
	return err
}

func (m *machine) Attach(ctx context.Context, dev vm.Device) error {
	return m.changeDevice(ctx, dev, false)
}

func (m *machine) Detach(ctx context.Context, dev vm.Device) error {
	return m.changeDevice(ctx, dev, true)
}

func (m *machine) Status(ctx context.Context) (backend.Status, error) {
	if err := m.supports(rpcStatus); err != nil {
		return backend.Status{}, err
	}
	return callTyped[StatusRequest, StatusResponse](m.client, ctx, rpcStatus, StatusRequest{})
}

func (m *machine) RemoteControl() (vm.Guest, error) {
	for _, method := range []rpcMethod{rpcGuestExec, rpcGuestRead, rpcGuestWrite} {
		if err := m.supports(method); err != nil {
			return nil, err
		}
	}
	return &guest{machine: m}, nil
}

type guest struct{ machine *machine }

func (g *guest) Run(ctx context.Context, cmd *vm.GuestCmd) error {
	if cmd == nil || cmd.Path == "" {
		return fmt.Errorf("guest command path is required")
	}
	if cmd.Stdin != nil {
		return fmt.Errorf("guest command stdin over control socket: %w", errors.ErrUnsupported)
	}
	req := GuestExecRequest{Path: cmd.Path, Args: cmd.Args, Env: cmd.Env, Dir: cmd.Dir, CaptureOutput: true}
	if deadline, ok := ctx.Deadline(); ok {
		req.Timeout = units.Duration(time.Until(deadline))
	}
	resp, err := callTyped[GuestExecRequest, GuestExecResponse](g.machine.client, ctx, rpcGuestExec, req)
	if err != nil {
		return err
	}
	stdout, err := base64.StdEncoding.DecodeString(resp.OutData)
	if err != nil {
		return fmt.Errorf("decode guest stdout: %w", err)
	}
	stderr, err := base64.StdEncoding.DecodeString(resp.ErrData)
	if err != nil {
		return fmt.Errorf("decode guest stderr: %w", err)
	}
	if cmd.Stdout != nil {
		if _, err := cmd.Stdout.Write(stdout); err != nil {
			return err
		}
	}
	if cmd.Stderr != nil {
		if _, err := cmd.Stderr.Write(stderr); err != nil {
			return err
		}
	}
	if resp.ExitCode != 0 {
		exitErr := &vm.ExitError{Code: resp.ExitCode}
		if cmd.Stderr == nil {
			exitErr.Stderr = stderr
		}
		return exitErr
	}
	return nil
}

func (g *guest) Open(ctx context.Context, name string) (io.ReadCloser, error) {
	resp, err := callTyped[GuestReadRequest, GuestReadResponse](g.machine.client, ctx, rpcGuestRead, GuestReadRequest{Path: name})
	if err != nil {
		return nil, err
	}
	data, err := base64.StdEncoding.DecodeString(resp.DataBase64)
	if err != nil {
		return nil, fmt.Errorf("decode guest file: %w", err)
	}
	return io.NopCloser(bytes.NewReader(data)), nil
}

func (g *guest) Create(ctx context.Context, name string, mode fs.FileMode) (io.WriteCloser, error) {
	return &guestWriter{ctx: ctx, guest: g, name: name, mode: mode}, nil
}

type guestWriter struct {
	bytes.Buffer
	ctx   context.Context
	guest *guest
	name  string
	mode  fs.FileMode
}

func (w *guestWriter) Close() error {
	_, err := callTyped[GuestWriteRequest, GuestWriteResponse](w.guest.machine.client, w.ctx, rpcGuestWrite, GuestWriteRequest{
		Path: w.name, DataBase64: base64.StdEncoding.EncodeToString(w.Bytes()), Mode: uint32(w.mode.Perm()),
	})
	return err
}

func (g *guest) Shutdown(ctx context.Context) error {
	if err := g.machine.supports(rpcGuestShutdown); err != nil {
		return err
	}
	_, err := callTyped[GuestShutdownRequest, GuestShutdownResponse](g.machine.client, ctx, rpcGuestShutdown, GuestShutdownRequest{})
	return err
}

func (*guest) Close() error { return nil }

var (
	_ backend.Machine        = (*machine)(nil)
	_ backend.Suspender      = (*machine)(nil)
	_ backend.MemoryResizer  = (*machine)(nil)
	_ backend.DeviceAttacher = (*machine)(nil)
	_ backend.StatusReporter = (*machine)(nil)
	_ vm.Guest               = (*guest)(nil)
)

type client struct {
	dial func(context.Context) (net.Conn, error)
}

func callTyped[Req any, Resp any](c *client, ctx context.Context, method rpcMethod, req Req) (Resp, error) {
	var resp Resp
	err := c.call(ctx, method, req, &resp)
	return resp, err
}

func (c *client) call(ctx context.Context, method rpcMethod, params any, result any) error {
	conn, err := c.dial(ctx)
	if err != nil {
		return fmt.Errorf("control dial: %w", err)
	}
	defer conn.Close()
	if deadline, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(deadline)
	}
	payload, err := json.Marshal(params)
	if err != nil {
		return err
	}
	if err := json.NewEncoder(conn).Encode(requestEnvelope{ID: 1, Method: method, Params: payload}); err != nil {
		return fmt.Errorf("control request: %w", err)
	}
	line, err := bufio.NewReader(conn).ReadBytes('\n')
	if err != nil {
		return fmt.Errorf("control response: %w", err)
	}
	var resp responseEnvelope
	if err := json.Unmarshal(line, &resp); err != nil {
		return fmt.Errorf("control response: %w", err)
	}
	if resp.Error != nil {
		return resp.Error
	}
	if result == nil {
		return nil
	}
	if err := json.Unmarshal(resp.Result, result); err != nil {
		return fmt.Errorf("control result: %w", err)
	}
	return nil
}
