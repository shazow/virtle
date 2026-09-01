package runtime

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/shazow/virtle/backend"
	"github.com/shazow/virtle/internal/control"
)

func TestStartControlServesRuntimeHandler(t *testing.T) {
	socketPath := filepath.Join(t.TempDir(), "virtle.sock")
	handler := fakeRuntimeHandler{}
	router, err := control.NewRouter(control.Handlers{Core: handler})
	if err != nil {
		t.Fatalf("router: %v", err)
	}
	server, err := StartControl(context.Background(), socketPath, router, nil)
	if err != nil {
		t.Fatalf("start control: %v", err)
	}
	defer server.Close()

	machine, err := control.Dial(context.Background(), socketPath)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	resp, err := machine.(backend.StatusReporter).Status(context.Background())
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	if resp.State != control.RuntimeReady || resp.CID != 7 {
		t.Fatalf("unexpected status response: %#v", resp)
	}
}

func TestStartControlEmptySocketPath(t *testing.T) {
	router, err := control.NewRouter(control.Handlers{Core: fakeRuntimeHandler{}})
	if err != nil {
		t.Fatalf("router: %v", err)
	}
	server, err := StartControl(context.Background(), "", router, nil)
	if err != nil {
		t.Fatalf("empty start control: %v", err)
	}
	if server != nil {
		t.Fatalf("expected nil server for empty socket path, got %#v", server)
	}
}

type fakeRuntimeHandler struct{}

func (fakeRuntimeHandler) Wait(context.Context, control.WaitRequest) (control.WaitResponse, error) {
	return control.WaitResponse{}, nil
}

func (fakeRuntimeHandler) Status(context.Context, control.StatusRequest) (control.StatusResponse, error) {
	return control.StatusResponse{State: control.RuntimeReady, CID: 7}, nil
}
