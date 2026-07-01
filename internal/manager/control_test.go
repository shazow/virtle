package manager

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/shazow/virtle/internal/manager/control"
)

type fakeControlCore struct{}

func (fakeControlCore) Status(context.Context, control.StatusRequest) (control.StatusResponse, error) {
	return control.StatusResponse{State: control.RuntimeReady}, nil
}

func startTestControlServerAt(t *testing.T, path string, runtime any) {
	t.Helper()
	core, ok := runtime.(control.RuntimeCore)
	if !ok {
		t.Fatalf("runtime core handler is required")
	}
	handlers := control.Handlers{Core: core}
	if guest, ok := runtime.(control.RuntimeGuest); ok {
		handlers.Guest = guest
	}
	if suspend, ok := runtime.(control.RuntimeSuspend); ok {
		handlers.Suspend = suspend
	}
	if hotplug, ok := runtime.(control.RuntimeHotplug); ok {
		handlers.Hotplug = hotplug
	}
	if balloon, ok := runtime.(control.RuntimeBalloon); ok {
		handlers.Balloon = balloon
	}
	router, err := control.NewRouter(handlers)
	if err != nil {
		t.Fatalf("router: %v", err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("create control socket directory: %v", err)
	}
	listener, err := control.Listen(path)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	server, err := control.NewServer(router)
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
