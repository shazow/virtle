package runtime

import (
	"context"
	"encoding/json"
	"net"
	"sync"
	"testing"
	"testing/synctest"

	"github.com/shazow/virtle/backend/qemu/internal/launch"
	"github.com/shazow/virtle/internal/control"
	"github.com/shazow/virtle/internal/executor/executortest"
)

func TestShutdownWaitsForControlResponses(t *testing.T) {
	for _, method := range []string{"kill", "shutdown"} {
		t.Run(method, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				processes := launch.NewProcessSet()
				process := (&executortest.Process{}).Process()
				processes.SetQEMU(process)
				release := make(chan struct{})
				var releaseOnce sync.Once
				releaseTeardown := func() { releaseOnce.Do(func() { close(release) }) }
				defer releaseTeardown()
				r := New(Config{
					Processes:       processes,
					SuspendRequests: launch.NewSuspendCoordinator(),
					WriteBack: func(context.Context) error {
						// Keep acceptance open until every lifecycle request is
						// queued, including requests sharing the same closer.
						<-release
						return nil
					},
				})
				hotplug := &heldHotplug{entered: make(chan struct{}), release: make(chan struct{})}
				defer close(hotplug.release)
				router, err := control.NewRouter(control.Handlers{Core: r, Kill: r, Shutdown: r, Suspend: r, Hotplug: hotplug})
				if err != nil {
					t.Fatal(err)
				}
				r.control, err = control.NewServer(router)
				if err != nil {
					t.Fatal(err)
				}
				defer r.control.Close()
				listener := &pipeListener{connections: make(chan net.Conn), closed: make(chan struct{})}
				served := make(chan error, 1)
				go func() { served <- r.control.Serve(listener) }()
				<-r.control.Started()
				server, client := net.Pipe()
				defer server.Close()
				defer client.Close()
				listener.connections <- server
				if err := json.NewEncoder(client).Encode(map[string]any{"method": "hotplug"}); err != nil {
					t.Fatal(err)
				}
				<-hotplug.entered

				// The VM reaper calls Shutdown before publishing launcher
				// completion. Exercise that same boundary after QEMU exits.
				completed := make(chan error, 1)
				go func() {
					<-process.Done()
					completed <- r.Shutdown(context.Background())
				}()
				var clients []net.Conn
				var writes []<-chan struct{}
				for _, request := range []string{"wait", "suspend", method, "kill", "shutdown"} {
					server, client := net.Pipe()
					defer server.Close()
					defer client.Close()
					writing := make(chan struct{})
					listener.connections <- &responseConn{Conn: server, writing: writing}
					if err := json.NewEncoder(client).Encode(map[string]any{"id": len(clients) + 1, "method": request}); err != nil {
						t.Fatal(err)
					}
					clients = append(clients, client)
					writes = append(writes, writing)
				}
				releaseTeardown()
				synctest.Wait()
				for i, writing := range writes {
					select {
					case <-writing:
					default:
						t.Fatalf("lifecycle handler %d blocked before writing its response", i+1)
					}
				}
				select {
				case err := <-completed:
					t.Fatalf("launcher teardown completed before lifecycle RPC responses were delivered: %v", err)
				default:
				}
				for i, client := range clients {
					var response struct {
						ID    int               `json:"id"`
						Error *control.RPCError `json:"error"`
					}
					if err := json.NewDecoder(client).Decode(&response); err != nil {
						t.Fatal(err)
					}
					if response.ID != i+1 {
						t.Fatalf("lifecycle response: %+v", response)
					}
					if i == 1 {
						// No foreground loop remains to service the queued
						// suspend. Teardown must release its handler to drain.
						if response.Error == nil || response.Error.Code != control.ErrFailedPrecondition {
							t.Fatalf("pending suspend response: %+v", response)
						}
					} else if response.Error != nil {
						t.Fatalf("lifecycle response: %+v", response)
					}
					if i < len(clients)-1 {
						synctest.Wait()
						select {
						case err := <-completed:
							t.Fatalf("launcher teardown completed with lifecycle responses still pending: %v", err)
						default:
						}
					}
				}
				synctest.Wait()
				select {
				case err := <-completed:
					if err != nil {
						t.Fatal(err)
					}
				default:
					t.Fatal("launcher teardown is waiting for an unrelated hotplug handler")
				}
				if err := <-served; err != nil {
					t.Fatal(err)
				}
			})
		})
	}
}

func TestShutdownDrainsRequestsClassifiedAfterClose(t *testing.T) {
	for _, method := range []string{"wait", "kill", "shutdown", "suspend", "hotplug"} {
		t.Run(method, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				r := New(Config{SuspendRequests: launch.NewSuspendCoordinator()})
				hotplug := &heldHotplug{entered: make(chan struct{}), release: make(chan struct{})}
				defer close(hotplug.release)
				router, err := control.NewRouter(control.Handlers{Core: r, Kill: r, Shutdown: r, Suspend: r, Hotplug: hotplug})
				if err != nil {
					t.Fatal(err)
				}
				r.control, err = control.NewServer(router)
				if err != nil {
					t.Fatal(err)
				}
				defer r.control.Close()
				listener := &pipeListener{connections: make(chan net.Conn), closed: make(chan struct{})}
				go func() {
					if err := r.control.Serve(listener); err != nil {
						t.Error(err)
					}
				}()
				<-r.control.Started()
				server, client := net.Pipe()
				defer server.Close()
				defer client.Close()
				listener.connections <- server
				// Hold the accepted request before decoding. Multiple launch
				// teardown callers must all include its eventual classification.
				completed := make(chan error, 2)
				for range 2 {
					go func() { completed <- r.Shutdown(context.Background()) }()
				}
				synctest.Wait()
				select {
				case err := <-completed:
					t.Fatalf("teardown missed an unclassified request: %v", err)
				default:
				}
				if err := json.NewEncoder(client).Encode(map[string]any{"id": 1, "method": method}); err != nil {
					t.Fatal(err)
				}
				synctest.Wait()
				if method != "hotplug" {
					select {
					case err := <-completed:
						t.Fatalf("teardown missed a pending lifecycle response: %v", err)
					default:
					}
					var response struct {
						ID int `json:"id"`
					}
					if err := json.NewDecoder(client).Decode(&response); err != nil {
						t.Fatal(err)
					}
					if response.ID != 1 {
						t.Fatalf("unexpected response: %+v", response)
					}
				} else {
					<-hotplug.entered
				}
				synctest.Wait()
				for range 2 {
					select {
					case err := <-completed:
						if err != nil {
							t.Fatal(err)
						}
					default:
						t.Fatal("teardown did not finish after lifecycle responses drained")
					}
				}
			})
		})
	}
}

type heldHotplug struct {
	entered chan struct{}
	release chan struct{}
}

func (h *heldHotplug) Hotplug(context.Context, control.HotplugRequest) (control.HotplugResponse, error) {
	close(h.entered)
	<-h.release
	return control.HotplugResponse{}, nil
}

// net.Pipe holds Write until the client reads, exposing the response-delivery
// window deterministically without a real socket or scheduler-dependent sleeps.
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
