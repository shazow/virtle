package control

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/shazow/virtle/backend"
	"github.com/shazow/virtle/backend/backendtest"
	"github.com/shazow/virtle/vm"
)

type proxyControlHandler struct {
	fakeControlHandler
	done chan struct{}
}

type proxyBackend struct {
	start func(context.Context) (backend.Machine, error)
}

func (b proxyBackend) Start(ctx context.Context, spec *vm.Spec) (backend.Machine, error) {
	return b.start(ctx)
}

type conformingProxyHandler struct {
	proxyControlHandler
}

func (h *conformingProxyHandler) GuestExec(ctx context.Context, req GuestExecRequest) (GuestExecResponse, error) {
	return GuestExecResponse{Exited: true}, nil
}

func (h *conformingProxyHandler) Kill(ctx context.Context, req KillRequest) (KillResponse, error) {
	h.finish()
	return KillResponse{}, nil
}

func (h *conformingProxyHandler) ShutdownRPC(ctx context.Context, req ShutdownRequest) (ShutdownResponse, error) {
	h.finish()
	return ShutdownResponse{}, nil
}

func (h *conformingProxyHandler) finish() {
	select {
	case <-h.done:
	default:
		close(h.done)
	}
}

func TestMachineProxyConforms(t *testing.T) {
	backendtest.TestBackend(t, func(t *testing.T) (backend.Backend, *vm.Spec) {
		h := &conformingProxyHandler{proxyControlHandler: proxyControlHandler{done: make(chan struct{})}}
		h.status = StatusResponse{State: RuntimeReady}
		path := startTestControlServer(t, h)
		return proxyBackend{start: func(ctx context.Context) (backend.Machine, error) {
			return Dial(ctx, path)
		}}, &vm.Spec{}
	})
}

func (h *proxyControlHandler) Wait(ctx context.Context, req WaitRequest) (WaitResponse, error) {
	select {
	case <-h.done:
		return WaitResponse{}, nil
	case <-ctx.Done():
		return WaitResponse{}, context.Cause(ctx)
	}
}

func TestDialImplementsMachineContract(t *testing.T) {
	h := &proxyControlHandler{done: make(chan struct{})}
	h.status = StatusResponse{State: RuntimeReady, CID: 7, PID: 42}
	path := startTestControlServer(t, h)

	m, err := Dial(context.Background(), path)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	status, err := m.(backend.StatusReporter).Status(context.Background())
	if err != nil || status.CID != 7 || status.PID != 42 {
		t.Fatalf("Status = %#v, %v", status, err)
	}
	if err := m.(backend.Suspender).Suspend(context.Background()); err != nil {
		t.Fatalf("Suspend: %v", err)
	}
	if h.suspendCalls != 1 {
		t.Fatalf("Suspend calls = %d, want 1", h.suspendCalls)
	}

	g, err := m.RemoteControl()
	if err != nil {
		t.Fatalf("RemoteControl: %v", err)
	}
	var stdout, stderr bytes.Buffer
	err = g.Run(context.Background(), &vm.GuestCmd{Path: "/bin/false", Stdout: &stdout, Stderr: &stderr})
	var exitErr *vm.ExitError
	if !errors.As(err, &exitErr) || exitErr.Code != 7 {
		t.Fatalf("Run error = %v, want ExitError code 7", err)
	}
	if stdout.String() != "out\n" || stderr.String() != "err\n" {
		t.Fatalf("Run output = %q, %q", stdout.String(), stderr.String())
	}

	select {
	case <-m.Done():
		t.Fatal("Done closed before wait response")
	default:
	}
	close(h.done)
	if err := m.Wait(context.Background()); err != nil {
		t.Fatalf("Wait: %v", err)
	}
}

func TestDialCapabilitySkewReturnsUnsupported(t *testing.T) {
	path := startTestControlServer(t, &fakeControlCore{})
	m, err := Dial(context.Background(), path)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	err = m.(backend.DeviceAttacher).Attach(context.Background(), vm.Disk{Path: "disk.img"})
	if !errors.Is(err, errors.ErrUnsupported) {
		t.Fatalf("Attach error = %v, want errors.ErrUnsupported", err)
	}
}

func TestLegacyMachineLifecycle(t *testing.T) {
	for _, test := range []struct {
		name    string
		methods []string
		action  rpcMethod
	}{
		{name: "suspend", methods: []string{"suspend"}, action: rpcSuspend},
		{name: "guest shutdown", methods: []string{"guest-exec", "guest-read", "guest-write", "guest-shutdown"}, action: rpcGuestShutdown},
		{name: "kill fallback", methods: []string{"kill"}, action: rpcKill},
	} {
		t.Run(test.name, func(t *testing.T) {
			// The socket's disappearance is the old server's completion record,
			// so this compatibility test needs a real Unix listener.
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			path, stop := legacyControlServer(t, test.methods, func(method rpcMethod, stop func()) (any, error) {
				switch method {
				case rpcStatus:
					return StatusResponse{State: RuntimeReady}, nil
				case test.action:
					stop()
					return SuspendResponse{Saved: true}, nil
				default:
					return nil, &RPCError{Code: ErrUnknownMethod, Message: "unknown method"}
				}
			})
			defer stop()
			m, err := Dial(ctx, path)
			if err != nil {
				t.Fatal(err)
			}
			status, err := m.(backend.StatusReporter).Status(ctx)
			if err != nil || status.State != RuntimeReady {
				t.Fatalf("Status = %+v, %v", status, err)
			}
			select {
			case <-m.Done():
				t.Fatalf("machine reported exit while the old server was ready: %v", m.Err())
			default:
			}
			if test.action == rpcSuspend {
				err = m.(backend.Suspender).Suspend(ctx)
			} else {
				err = m.Shutdown(ctx)
			}
			if err != nil {
				t.Fatalf("%s: %v", test.name, err)
			}
			if err := m.Wait(ctx); err != nil {
				t.Fatalf("wait for old server teardown: %v", err)
			}
		})
	}
}

// legacyControlServer serves the older method set over the real JSON transport.
// The current Router requires wait, which these servers did not implement.
func legacyControlServer(t *testing.T, methods []string, handle func(rpcMethod, func()) (any, error)) (string, func()) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "ctl")
	listener, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	stop := func() { _ = listener.Close() }
	accepted := make(chan struct{})
	var handlers sync.WaitGroup
	t.Cleanup(func() { stop(); <-accepted; handlers.Wait() })
	go func() {
		defer close(accepted)
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			handlers.Add(1)
			go func() {
				defer handlers.Done()
				defer conn.Close()
				var req requestEnvelope
				if err := json.NewDecoder(conn).Decode(&req); err != nil {
					return
				}
				var result any
				var rpcErr error
				if req.Method == rpcMethods {
					result = MethodsResponse{Methods: append([]string{"methods", "status"}, methods...)}
				} else {
					result, rpcErr = handle(req.Method, stop)
				}
				response := responseEnvelope{ID: req.ID}
				if rpcErr != nil {
					_ = errors.As(rpcErr, &response.Error)
				} else {
					response.Result, _ = json.Marshal(result)
				}
				_ = json.NewEncoder(conn).Encode(response)
			}()
		}
	}()
	return path, stop
}

// pipeClient exercises the real control transport without a filesystem socket.
func pipeClient(t *testing.T, handlers Handlers) *client {
	t.Helper()
	router, err := NewRouter(handlers)
	if err != nil {
		t.Fatalf("NewRouter: %v", err)
	}
	server, err := NewServer(router)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	var serving sync.WaitGroup
	t.Cleanup(serving.Wait)
	return &client{dial: func(ctx context.Context) (net.Conn, error) {
		host, peer := net.Pipe()
		serving.Add(1)
		go func() {
			defer serving.Done()
			server.handleConn(peer, nil)
		}()
		return host, nil
	}}
}

func TestGuestRunCancellationReachesServer(t *testing.T) {
	handler := &blockingGuestHandler{entered: make(chan struct{}), canceled: make(chan struct{})}
	c := pipeClient(t, Handlers{Core: &fakeControlCore{}, Guest: handler})
	g := &guest{machine: &machine{client: c}}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- g.Run(ctx, &vm.GuestCmd{Path: "/bin/sleep", Args: []string{"infinity"}}) }()
	<-handler.entered
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("Run after cancellation = %v, want context.Canceled", err)
	}
	<-handler.canceled
}

type cancelShutdownHandler struct {
	entered chan struct{}
	killed  chan struct{}
}

func (h *cancelShutdownHandler) ShutdownRPC(ctx context.Context, _ ShutdownRequest) (ShutdownResponse, error) {
	close(h.entered)
	<-ctx.Done()
	return ShutdownResponse{}, context.Cause(ctx)
}

func (h *cancelShutdownHandler) Kill(context.Context, KillRequest) (KillResponse, error) {
	close(h.killed)
	return KillResponse{}, nil
}

func TestMachineShutdownCancellationFallsBackToKill(t *testing.T) {
	handler := &cancelShutdownHandler{entered: make(chan struct{}), killed: make(chan struct{})}
	c := pipeClient(t, Handlers{Core: &fakeControlCore{}, Shutdown: handler, Kill: handler})
	m := &machine{client: c, methods: map[rpcMethod]bool{rpcShutdown: true, rpcKill: true}, done: make(chan struct{})}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- m.Shutdown(ctx) }()
	<-handler.entered
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("Shutdown after cancellation = %v, want context.Canceled", err)
	}
	<-handler.killed
}
