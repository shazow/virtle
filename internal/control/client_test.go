package control

import (
	"bytes"
	"context"
	"errors"
	"testing"

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
