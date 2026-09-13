//go:build linux

package vmmhost

import (
	"context"
	"encoding/json"
	"net"
	"os"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/shazow/virtle/backend"
	"github.com/shazow/virtle/internal/control"
	"github.com/shazow/virtle/internal/executor/executortest"
)

const testTimeout = 5 * time.Second

// A pipe keeps the response write blocked until the client consumes it.
// Completing the process only after Write starts forces the shutdown race.
func TestCompletionWaitsForControlResponse(t *testing.T) {
	synctest.Test(t, testCompletionWaitsForControlResponse)
}

func testCompletionWaitsForControlResponse(t *testing.T) {
	process := &executortest.Process{}
	lock, err := os.CreateTemp(t.TempDir(), "lock")
	if err != nil {
		t.Fatal(err)
	}
	m := &Machine{process: process.Process(), done: make(chan struct{}), stopped: make(chan struct{}), lock: lock, runtimeDir: t.TempDir()}
	router, err := control.NewMachineRouter(responseMachine{Machine: m})
	if err != nil {
		t.Fatal(err)
	}
	m.control, err = control.NewServer(router)
	if err != nil {
		t.Fatal(err)
	}
	serverConn, client := net.Pipe()
	defer client.Close()
	defer serverConn.Close()
	writing := make(chan struct{})
	listener := &pipeListener{connections: make(chan net.Conn, 1), closed: make(chan struct{})}
	listener.connections <- &responseConn{Conn: serverConn, writing: writing}
	served := make(chan error, 1)
	go func() { served <- m.control.Serve(listener) }()
	<-m.control.Started()
	go m.reap()
	if err := json.NewEncoder(client).Encode(map[string]any{"id": 1, "method": "shutdown"}); err != nil {
		t.Fatal(err)
	}
	<-writing
	process.Complete(nil)
	// Wait until the reaper has either finished (the bug) or is blocked
	// draining the pipe response. No scheduler timing or sleeps are involved.
	synctest.Wait()
	select {
	case <-m.Done():
		t.Fatal("launcher completion preceded the shutdown RPC response write")
	default:
	}
	if err := client.SetReadDeadline(time.Now().Add(testTimeout)); err != nil {
		t.Fatal(err)
	}
	var response struct {
		ID    int               `json:"id"`
		Error *control.RPCError `json:"error"`
	}
	if err := json.NewDecoder(client).Decode(&response); err != nil {
		t.Fatal(err)
	}
	if response.ID != 1 || response.Error != nil {
		t.Fatalf("shutdown response: %+v", response)
	}
	select {
	case <-m.Done():
	case <-time.After(testTimeout):
		t.Fatal("completion blocked after response delivery")
	}
	if err := <-served; err != nil {
		t.Fatal(err)
	}
}

// The handler has completed shutdown; the regression concerns the remaining
// transport write and reaping, independently of any VMM's API.
type responseMachine struct{ backend.Machine }

func (responseMachine) Shutdown(context.Context) error { return nil }

type responseConn struct {
	net.Conn
	writing chan struct{}
}

func (c *responseConn) Write(p []byte) (int, error) {
	close(c.writing)
	return c.Conn.Write(p)
}

type pipeListener struct {
	connections chan net.Conn
	closed      chan struct{}
	once        sync.Once
}

func (l *pipeListener) Accept() (net.Conn, error) {
	select {
	case conn := <-l.connections:
		return conn, nil
	case <-l.closed:
		return nil, net.ErrClosed
	}
}
func (l *pipeListener) Close() error { l.once.Do(func() { close(l.closed) }); return nil }
func (*pipeListener) Addr() net.Addr { return &net.UnixAddr{Net: "unix", Name: "control"} }

func TestConcurrentControlLifecycleResponses(t *testing.T) {
	for _, lastMethod := range []string{"shutdown", "kill"} {
		t.Run(lastMethod, func(t *testing.T) {
			process := &executortest.Process{}
			lock, err := os.CreateTemp(t.TempDir(), "lock")
			if err != nil {
				t.Fatal(err)
			}
			m := &Machine{process: process.Process(), done: make(chan struct{}), stopped: make(chan struct{}),
				lock: lock, runtimeDir: t.TempDir(), shutdownDone: make(chan struct{}), shutdownTimeout: testTimeout}
			release := make(chan struct{})
			m.graceful = func(ctx context.Context, _ string) error {
				select {
				case <-release:
				case <-ctx.Done():
					return ctx.Err()
				}
				process.Complete(nil)
				return nil
			}
			router, err := control.NewMachineRouter(controlMachine{m})
			if err != nil {
				t.Fatal(err)
			}
			m.control, err = control.NewServer(router)
			if err != nil {
				t.Fatal(err)
			}
			listener := &pipeListener{connections: make(chan net.Conn, 1), closed: make(chan struct{})}
			served := make(chan error, 1)
			go func() { served <- m.control.Serve(listener) }()
			<-m.control.Started()
			go m.reap()
			var clients []net.Conn
			for _, method := range []string{"wait", "shutdown", "shutdown", lastMethod} {
				server, client := net.Pipe()
				t.Cleanup(func() { _ = client.Close(); _ = server.Close() })
				_ = client.SetDeadline(time.Now().Add(testTimeout))
				listener.connections <- server
				// Pipe writes ensure each request was accepted before exit starts.
				if err := json.NewEncoder(client).Encode(map[string]any{"id": 1, "method": method}); err != nil {
					t.Fatal(err)
				}
				clients = append(clients, client)
			}
			close(release)
			for _, client := range clients {
				var response struct {
					Error *control.RPCError `json:"error"`
				}
				if err := json.NewDecoder(client).Decode(&response); err != nil {
					t.Fatal(err)
				}
				if response.Error != nil {
					t.Fatal(response.Error)
				}
			}
			ctx, cancel := context.WithTimeout(t.Context(), testTimeout)
			defer cancel()
			if err := m.Wait(ctx); err != nil {
				t.Fatal(err)
			}
			if err := m.Shutdown(ctx); err != nil {
				t.Fatal(err)
			}
			if err := m.Kill(); err != nil {
				t.Fatal(err)
			}
			if err := <-served; err != nil {
				t.Fatal(err)
			}
		})
	}
}
