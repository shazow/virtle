package control

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

type fakeControlCore struct {
	status StatusResponse
}

func (h *fakeControlCore) Status(context.Context, StatusRequest) (StatusResponse, error) {
	return h.status, nil
}

type fakeControlGuest struct {
	psResp    GuestPSResponse
	execReq   GuestExecRequest
	readReq   GuestReadRequest
	readResp  GuestReadResponse
	writeReq  GuestWriteRequest
	writeResp GuestWriteResponse
}

func (h *fakeControlGuest) GuestPS(context.Context, GuestPSRequest) (GuestPSResponse, error) {
	return h.psResp, nil
}

func (h *fakeControlGuest) GuestExec(ctx context.Context, req GuestExecRequest) (GuestExecResponse, error) {
	h.execReq = req
	return GuestExecResponse{Exited: true, ExitCode: 7, OutData: "b3V0Cg==", ErrData: "ZXJyCg=="}, nil
}

func (h *fakeControlGuest) GuestRead(ctx context.Context, req GuestReadRequest) (GuestReadResponse, error) {
	h.readReq = req
	return h.readResp, nil
}

func (h *fakeControlGuest) GuestWrite(ctx context.Context, req GuestWriteRequest) (GuestWriteResponse, error) {
	h.writeReq = req
	return h.writeResp, nil
}

type fakeControlHandler struct {
	fakeControlCore
	fakeControlGuest
	suspendCalls int
	hotplugReq   HotplugRequest
	balloonReq   BalloonRequest
}

func (h *fakeControlHandler) Suspend(context.Context, SuspendRequest) (SuspendResponse, error) {
	h.suspendCalls++
	return SuspendResponse{Saved: true, VMStatePath: "/tmp/vm-state"}, nil
}

func (h *fakeControlHandler) Hotplug(ctx context.Context, req HotplugRequest) (HotplugResponse, error) {
	h.hotplugReq = req
	return HotplugResponse{ID: req.ID, Detach: req.Detach}, nil
}

func (h *fakeControlHandler) Balloon(ctx context.Context, req BalloonRequest) (BalloonResponse, error) {
	h.balloonReq = req
	return BalloonResponse{ActualBytes: 512, TargetBytes: req.TargetBytes}, nil
}

func TestControlClientServerTypedCalls(t *testing.T) {
	handler := &fakeControlHandler{
		fakeControlCore: fakeControlCore{
			status: StatusResponse{State: RuntimeReady, CID: 7},
		},
		fakeControlGuest: fakeControlGuest{
			psResp:    GuestPSResponse{ProcessList: "USER COMMAND\nroot init"},
			readResp:  GuestReadResponse{Path: "/tmp/message", DataBase64: "aGVsbG8="},
			writeResp: GuestWriteResponse{Path: "/tmp/message"},
		},
	}
	path := startTestControlServer(t, handler)
	client := Dial(path)

	status, err := client.Status(context.Background(), StatusRequest{})
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	if status.State != RuntimeReady || status.CID != 7 {
		t.Fatalf("unexpected status: %#v", status)
	}

	ps, err := client.GuestPS(context.Background(), GuestPSRequest{})
	if err != nil {
		t.Fatalf("guest ps: %v", err)
	}
	if ps.ProcessList != "USER COMMAND\nroot init" {
		t.Fatalf("unexpected guest ps: %#v", ps)
	}

	execResp, err := client.GuestExec(context.Background(), GuestExecRequest{
		Path:          "/bin/echo",
		Args:          []string{"hello"},
		CaptureOutput: true,
	})
	if err != nil {
		t.Fatalf("guest exec: %v", err)
	}
	if handler.execReq.Path != "/bin/echo" || !reflect.DeepEqual(handler.execReq.Args, []string{"hello"}) || !handler.execReq.CaptureOutput {
		t.Fatalf("unexpected guest exec request: %#v", handler.execReq)
	}
	if !execResp.Exited || execResp.ExitCode != 7 || execResp.OutData != "b3V0Cg==" || execResp.ErrData != "ZXJyCg==" {
		t.Fatalf("unexpected guest exec response: %#v", execResp)
	}

	readResp, err := client.GuestRead(context.Background(), GuestReadRequest{Path: "/tmp/message"})
	if err != nil {
		t.Fatalf("guest read: %v", err)
	}
	if handler.readReq.Path != "/tmp/message" {
		t.Fatalf("unexpected guest read request: %#v", handler.readReq)
	}
	if readResp.Path != "/tmp/message" || readResp.DataBase64 != "aGVsbG8=" {
		t.Fatalf("unexpected guest read response: %#v", readResp)
	}

	writeResp, err := client.GuestWrite(context.Background(), GuestWriteRequest{Path: "/tmp/message", DataBase64: "dXBkYXRlZA=="})
	if err != nil {
		t.Fatalf("guest write: %v", err)
	}
	if handler.writeReq.Path != "/tmp/message" || handler.writeReq.DataBase64 != "dXBkYXRlZA==" {
		t.Fatalf("unexpected guest write request: %#v", handler.writeReq)
	}
	if writeResp.Path != "/tmp/message" {
		t.Fatalf("unexpected guest write response: %#v", writeResp)
	}

	suspend, err := client.Suspend(context.Background(), SuspendRequest{})
	if err != nil {
		t.Fatalf("suspend: %v", err)
	}
	if !suspend.Saved || suspend.VMStatePath != "/tmp/vm-state" {
		t.Fatalf("unexpected suspend response: %#v", suspend)
	}

	hotplug, err := client.Hotplug(context.Background(), HotplugRequest{ID: "disk0", Detach: true})
	if err != nil {
		t.Fatalf("hotplug: %v", err)
	}
	if hotplug.ID != "disk0" || !hotplug.Detach || handler.hotplugReq.ID != "disk0" {
		t.Fatalf("unexpected hotplug response=%#v req=%#v", hotplug, handler.hotplugReq)
	}

	balloon, err := client.Balloon(context.Background(), BalloonRequest{TargetBytes: 1024})
	if err != nil {
		t.Fatalf("balloon: %v", err)
	}
	if balloon.ActualBytes != 512 || balloon.TargetBytes != 1024 || handler.balloonReq.TargetBytes != 1024 {
		t.Fatalf("unexpected balloon response=%#v req=%#v", balloon, handler.balloonReq)
	}

	methods, err := client.Methods(context.Background(), MethodsRequest{})
	if err != nil {
		t.Fatalf("methods: %v", err)
	}
	wantMethods := []string{"status", "methods", "guest-ps", "guest-exec", "guest-read", "guest-write", "suspend", "hotplug", "balloon"}
	if !reflect.DeepEqual(methods.Methods, wantMethods) {
		t.Fatalf("unexpected methods: got %#v want %#v", methods.Methods, wantMethods)
	}
}

func TestControlSocketPermissions(t *testing.T) {
	path := startTestControlServer(t, &fakeControlHandler{})
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat socket: %v", err)
	}
	if got, want := info.Mode().Perm(), os.FileMode(0o600); got != want {
		t.Fatalf("unexpected socket mode: got %o want %o", got, want)
	}
}

func TestControlRouterUnsupportedCapability(t *testing.T) {
	path := startTestControlServer(t, &fakeControlCore{})
	_, err := Dial(path).Hotplug(context.Background(), HotplugRequest{ID: "disk0"})
	var rpcErr *RPCError
	if err == nil || !errors.As(err, &rpcErr) || rpcErr.Code != ErrUnsupported {
		t.Fatalf("expected unsupported rpc error, got %v", err)
	}
}

func TestControlRouterRequiresExplicitHotplugRegistration(t *testing.T) {
	handler := &fakeControlHandler{}
	router, err := NewRouter(Handlers{Core: handler})
	if err != nil {
		t.Fatalf("router: %v", err)
	}
	serverPath := filepath.Join(t.TempDir(), "virtie.sock")
	startTestControlRouterAt(t, serverPath, router)

	_, err = Dial(serverPath).Hotplug(context.Background(), HotplugRequest{ID: "disk0"})
	var rpcErr *RPCError
	if err == nil || !errors.As(err, &rpcErr) || rpcErr.Code != ErrUnsupported {
		t.Fatalf("expected unregistered hotplug to be unsupported, got %v", err)
	}

	router, err = NewRouter(Handlers{Core: handler, Hotplug: handler})
	if err != nil {
		t.Fatalf("router with hotplug: %v", err)
	}
	registeredPath := filepath.Join(t.TempDir(), "virtie.sock")
	startTestControlRouterAt(t, registeredPath, router)

	resp, err := Dial(registeredPath).Hotplug(context.Background(), HotplugRequest{ID: "disk0", Detach: true})
	if err != nil {
		t.Fatalf("registered hotplug: %v", err)
	}
	if resp.ID != "disk0" || !resp.Detach {
		t.Fatalf("unexpected hotplug response: %#v", resp)
	}
}

func TestControlRouterRequiresExplicitGuestRegistration(t *testing.T) {
	handler := &fakeControlHandler{}
	router, err := NewRouter(Handlers{Core: handler})
	if err != nil {
		t.Fatalf("router: %v", err)
	}
	serverPath := filepath.Join(t.TempDir(), "virtie.sock")
	startTestControlRouterAt(t, serverPath, router)

	_, err = Dial(serverPath).GuestPS(context.Background(), GuestPSRequest{})
	var rpcErr *RPCError
	if err == nil || !errors.As(err, &rpcErr) || rpcErr.Code != ErrUnsupported {
		t.Fatalf("expected unregistered guest ps to be unsupported, got %v", err)
	}

	router, err = NewRouter(Handlers{Core: handler, Guest: handler})
	if err != nil {
		t.Fatalf("router with guest: %v", err)
	}
	registeredPath := filepath.Join(t.TempDir(), "virtie.sock")
	startTestControlRouterAt(t, registeredPath, router)

	resp, err := Dial(registeredPath).GuestPS(context.Background(), GuestPSRequest{})
	if err != nil {
		t.Fatalf("registered guest ps: %v", err)
	}
	if resp.ProcessList != "" {
		t.Fatalf("unexpected guest ps response: %#v", resp)
	}
}

func TestControlInvalidJSONAndUnknownMethod(t *testing.T) {
	path := startTestControlServer(t, &fakeControlHandler{})
	conn, err := net.Dial("unix", path)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	if _, err := conn.Write([]byte("{not json}\n")); err != nil {
		t.Fatalf("write invalid json: %v", err)
	}
	var invalid struct {
		Error *RPCError `json:"error,omitempty"`
	}
	if err := json.NewDecoder(conn).Decode(&invalid); err != nil {
		t.Fatalf("decode invalid response: %v", err)
	}
	_ = conn.Close()
	if invalid.Error == nil || invalid.Error.Code != ErrInvalidRequest {
		t.Fatalf("expected invalid request response, got %#v", invalid)
	}

	conn, err = net.Dial("unix", path)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	if err := json.NewEncoder(conn).Encode(struct {
		ID     int             `json:"id"`
		Method string          `json:"method"`
		Params json.RawMessage `json:"params"`
	}{ID: 1, Method: "missing", Params: json.RawMessage("{}")}); err != nil {
		t.Fatalf("write unknown method: %v", err)
	}
	var unknown struct {
		Error *RPCError `json:"error,omitempty"`
	}
	if err := json.NewDecoder(conn).Decode(&unknown); err != nil {
		t.Fatalf("decode unknown response: %v", err)
	}
	_ = conn.Close()
	if unknown.Error == nil || unknown.Error.Code != ErrUnknownMethod {
		t.Fatalf("expected unknown method response, got %#v", unknown)
	}
}

func startTestControlServer(t *testing.T, runtime any) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "virtie.sock")

	core, ok := runtime.(RuntimeCore)
	if !ok {
		t.Fatalf("runtime core handler is required")
	}
	handlers := Handlers{Core: core}
	if guest, ok := runtime.(RuntimeGuest); ok {
		handlers.Guest = guest
	}
	if suspend, ok := runtime.(RuntimeSuspend); ok {
		handlers.Suspend = suspend
	}
	if hotplug, ok := runtime.(RuntimeHotplug); ok {
		handlers.Hotplug = hotplug
	}
	if balloon, ok := runtime.(RuntimeBalloon); ok {
		handlers.Balloon = balloon
	}
	router, err := NewRouter(handlers)
	if err != nil {
		t.Fatalf("router: %v", err)
	}
	startTestControlRouterAt(t, path, router)
	return path
}

func startTestControlRouterAt(t *testing.T, path string, router *Router) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("create control socket directory: %v", err)
	}
	listener, err := Listen(path)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	server, err := NewServer(router)
	if err != nil {
		t.Fatalf("server: %v", err)
	}
	done := make(chan error, 1)
	go func() {
		done <- server.Serve(listener)
	}()
	t.Cleanup(func() {
		if err := listener.Close(); err != nil && !strings.Contains(err.Error(), "use of closed") {
			t.Errorf("close server: %v", err)
		}
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("serve: %v", err)
			}
		case <-time.After(time.Second):
			t.Error("server did not stop")
		}
	})
}
