package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"testing"
	"testing/synctest"

	"github.com/shazow/virtle/internal/control"
)

func TestStartControlPublishesBeforeServing(t *testing.T) {
	r := New(Config{})
	server, err := r.startControl(control.Handlers{}, func(server *control.Server) error {
		if r.control != server {
			t.Error("runtime must own the server before serving can accept a request")
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	defer server.Close()
}

func TestStartControlEarlyLifecycleCompletesLauncherShutdown(t *testing.T) {
	for _, method := range []string{"kill", "shutdown"} {
		t.Run(method, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				r := New(Config{})
				server, err := r.startControl(control.Handlers{}, func(server *control.Server) error {
					listener := &pipeListener{connections: make(chan net.Conn), closed: make(chan struct{})}
					if err := serveControl(context.Background(), listener, server, nil); err != nil {
						return err
					}
					// Finish an actual lifecycle RPC before startup can return.
					// Publication after this callback is deterministically too late.
					peer, client := net.Pipe()
					defer peer.Close()
					defer client.Close()
					listener.connections <- peer
					if err := json.NewEncoder(client).Encode(map[string]any{"id": 1, "method": method}); err != nil {
						return err
					}
					var response struct {
						ID    int               `json:"id"`
						Error *control.RPCError `json:"error"`
					}
					if err := json.NewDecoder(client).Decode(&response); err != nil {
						return err
					}
					if response.ID != 1 || response.Error != nil {
						t.Errorf("lifecycle response: %+v", response)
					}
					return nil
				})
				if err != nil {
					t.Fatal(err)
				}
				defer server.Close()
				completed := make(chan error, 1)
				go func() { completed <- r.Shutdown(context.Background()) }()
				synctest.Wait()
				select {
				case err := <-completed:
					if err != nil {
						t.Fatal(err)
					}
				default:
					t.Fatal("launcher shutdown hung after early lifecycle RPC completed")
				}
			})
		})
	}
}

func TestStartControlFailureClosesPublishedServer(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		r := New(Config{})
		listener := &pipeListener{connections: make(chan net.Conn), closed: make(chan struct{})}
		defer listener.Close()
		failure := errors.New("serve startup failed")
		var published *control.Server
		server, err := r.startControl(control.Handlers{}, func(server *control.Server) error {
			published = server
			if err := serveControl(context.Background(), listener, server, nil); err != nil {
				t.Fatal(err)
			}
			return failure
		})
		if !errors.Is(err, failure) || server != nil {
			t.Errorf("startup = (%v, %v), want (nil, %v)", server, err, failure)
		}
		if r.control != published {
			t.Error("failed startup must retain the published server for lifecycle draining")
		}
		select {
		case <-listener.closed:
		default:
			t.Error("failed startup left the listener open")
		}
		if err := r.Shutdown(context.Background()); err != nil {
			t.Fatal(err)
		}
	})
}

func TestServeControlClosedBeforeStartup(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		router, err := control.NewRouter(control.Handlers{Core: fakeRuntimeHandler{}})
		if err != nil {
			t.Fatal(err)
		}
		server, err := control.NewServer(router)
		if err != nil {
			t.Fatal(err)
		}
		_ = server.Close()
		listener := &pipeListener{connections: make(chan net.Conn), closed: make(chan struct{})}
		completed := make(chan error, 1)
		go func() { completed <- serveControl(context.Background(), listener, server, nil) }()
		synctest.Wait()
		select {
		case err := <-completed:
			if !errors.Is(err, net.ErrClosed) {
				t.Fatalf("startup error = %v, want net.ErrClosed", err)
			}
		default:
			t.Fatal("startup hung waiting for Started after Serve returned")
		}
		select {
		case <-listener.closed:
		default:
			t.Fatal("aborted startup left listener open")
		}
	})
}
