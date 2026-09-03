package qmpclient

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"path/filepath"
	"strings"
	"testing"
	"time"

	rawQMP "github.com/digitalocean/go-qemu/qmp/raw"
	"github.com/shazow/virtle/backend/qemu/internal/qmpwire"
	"github.com/shazow/virtle/backend/qemu/limits"
)

func TestQMPClientQuit(t *testing.T) {
	client, commands, cleanup := newTestQMPClient(t, func(message map[string]any) map[string]any {
		return map[string]any{"return": map[string]any{}}
	})
	defer cleanup()

	if err := client.Quit(context.Background()); err != nil {
		t.Fatalf("quit: %v", err)
	}

	assertHandshakeCommand(t, commands)
	assertQMPCommand(t, commands, "quit")
}

func TestQMPClientWithRawRunsGenericQMPCommand(t *testing.T) {
	client, commands, cleanup := newTestQMPClient(t, func(message map[string]any) map[string]any {
		return map[string]any{
			"return": map[string]any{
				"running":    true,
				"singlestep": false,
				"status":     "running",
			},
		}
	})
	defer cleanup()

	err := client.WithRaw(context.Background(), func(monitor *rawQMP.Monitor) error {
		_, err := monitor.QueryStatus()
		return err
	})
	if err != nil {
		t.Fatalf("with raw query-status: %v", err)
	}

	assertHandshakeCommand(t, commands)
	assertQMPCommand(t, commands, "query-status")
}

func TestQMPClientStopContAndQueryStatus(t *testing.T) {
	status := "running"
	client, commands, cleanup := newTestQMPClient(t, func(message map[string]any) map[string]any {
		switch message["execute"] {
		case "query-status":
			return map[string]any{
				"return": map[string]any{
					"running":    status == "running",
					"singlestep": false,
					"status":     status,
				},
			}
		case "stop":
			status = "paused"
			return map[string]any{"return": map[string]any{}}
		case "cont":
			status = "running"
			return map[string]any{"return": map[string]any{}}
		default:
			return map[string]any{"return": map[string]any{}}
		}
	})
	defer cleanup()

	gotStatus, err := client.QueryStatus(context.Background())
	if err != nil {
		t.Fatalf("query status: %v", err)
	}
	if gotStatus != "running" {
		t.Fatalf("unexpected status: got %q want running", gotStatus)
	}
	if err := client.Stop(context.Background()); err != nil {
		t.Fatalf("stop: %v", err)
	}
	gotStatus, err = client.QueryStatus(context.Background())
	if err != nil {
		t.Fatalf("query status after stop: %v", err)
	}
	if gotStatus != "paused" {
		t.Fatalf("unexpected status after stop: got %q want paused", gotStatus)
	}
	if err := client.Cont(context.Background()); err != nil {
		t.Fatalf("cont: %v", err)
	}

	assertHandshakeCommand(t, commands)
	assertQMPCommand(t, commands, "query-status")
	assertQMPCommand(t, commands, "stop")
	assertQMPCommand(t, commands, "query-status")
	assertQMPCommand(t, commands, "cont")
}

func TestQMPClientMigrationCommands(t *testing.T) {
	client, commands, cleanup := newTestQMPClient(t, func(message map[string]any) map[string]any {
		switch message["execute"] {
		case "query-migrate":
			return map[string]any{"return": map[string]any{"status": "completed"}}
		default:
			return map[string]any{"return": map[string]any{}}
		}
	})
	defer cleanup()

	if err := client.MigrateToFile(context.Background(), "/tmp/vm.state"); err != nil {
		t.Fatalf("migrate to file: %v", err)
	}
	status, err := client.QueryMigrate(context.Background())
	if err != nil {
		t.Fatalf("query migrate: %v", err)
	}
	if status != "completed" {
		t.Fatalf("unexpected migration status: got %q want completed", status)
	}
	if err := client.MigrateIncoming(context.Background(), "/tmp/vm.state"); err != nil {
		t.Fatalf("migrate incoming: %v", err)
	}

	assertHandshakeCommand(t, commands)
	assertQMPCommand(t, commands, "migrate")
	assertQMPCommand(t, commands, "query-migrate")
	assertQMPCommand(t, commands, "migrate-incoming")
}

// migrateTestRPCTimeout and migrateTestReplyDelay pin the invariant the
// not-capped test depends on: the migrate reply arrives only after the RPC
// liveness bound has expired. A timer is the only way to observe "the bound
// elapsed", so this is the sanctioned sleep with shared constants.
const (
	migrateTestRPCTimeout = 10 * time.Millisecond
	migrateTestReplyDelay = 10 * migrateTestRPCTimeout
)

func TestQMPClientMigrateNotCappedByRPCTimeout(t *testing.T) {
	client, commands, cleanup := newTestQMPClient(t, func(message map[string]any) map[string]any {
		if message["execute"] == "migrate" {
			time.Sleep(migrateTestReplyDelay)
		}
		return map[string]any{"return": map[string]any{}}
	})
	defer cleanup()
	client.session.RPCTimeout = migrateTestRPCTimeout

	if err := client.MigrateToFile(context.Background(), "/tmp/vm.state"); err != nil {
		t.Fatalf("migrate held its reply past the rpc timeout and should still succeed: %v", err)
	}

	assertHandshakeCommand(t, commands)
	assertQMPCommand(t, commands, "migrate")
}

func TestQMPClientMigrateHonorsContextDeadline(t *testing.T) {
	// Hold the migrate reply until the test ends so the ctx deadline is the
	// only thing that can end the call.
	release := make(chan struct{})
	client, _, cleanup := newTestQMPClient(t, func(message map[string]any) map[string]any {
		if message["execute"] == "migrate" {
			<-release
		}
		return map[string]any{"return": map[string]any{}}
	})
	defer cleanup()
	defer close(release)

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	err := client.MigrateToFile(ctx, "/tmp/vm.state")
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("expected ctx deadline to bound migrate, got %v", err)
	}
}

func TestQMPClientFailsFastAfterTimedOutOperation(t *testing.T) {
	release := make(chan struct{})
	client, _, cleanup := newTestQMPClient(t, func(message map[string]any) map[string]any {
		if message["execute"] == "stop" {
			// Hold the reply past the RPC bound so it is still in flight when
			// the next operation would run.
			<-release
		}
		return map[string]any{"return": map[string]any{}}
	})
	defer cleanup()
	client.session.RPCTimeout = 10 * time.Millisecond

	if err := client.Stop(context.Background()); err == nil {
		t.Fatal("expected stop to time out")
	}
	close(release)

	err := client.Cont(context.Background())
	if !errors.Is(err, qmpwire.ErrBroken) {
		t.Fatalf("expected broken-connection error instead of reading the stale reply, got %v", err)
	}
}

func TestQMPClientRejectsOversizedWireFrame(t *testing.T) {
	serverConn, clientConn := net.Pipe()
	defer serverConn.Close()
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = serverConn.Write([]byte(`{"return":"` + strings.Repeat("x", 128) + `"}` + "\n"))
	}()
	monitor := &socketMonitor{
		conn:    clientConn,
		decoder: qmpwire.NewDecoder(clientConn, 32),
	}
	_, err := monitor.readResponse()
	if !errors.Is(err, limits.ErrExceeded) || !qmpwire.IsWireError(err) {
		t.Fatalf("expected wire-level frame limit error, got %v", err)
	}
	_ = monitor.Disconnect()
	<-done
}

func TestQMPDialContextCancelsDuringHandshake(t *testing.T) {
	socketPath := filepath.Join(t.TempDir(), "qmp.sock")
	listener, err := net.Listen("unix", socketPath)
	if err != nil {
		t.Fatalf("listen unix socket: %v", err)
	}
	defer listener.Close()

	accepted := make(chan net.Conn, 1)
	serverDone := make(chan struct{})
	go func() {
		defer close(serverDone)
		conn, err := listener.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		accepted <- conn
		var buf [1]byte
		_, _ = conn.Read(buf[:])
	}()

	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(50*time.Millisecond, cancel)

	start := time.Now()
	_, err = (&SocketMonitorDialer{}).Dial(ctx, socketPath, 5*time.Second)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context cancellation, got %v", err)
	}
	if elapsed := time.Since(start); elapsed >= time.Second {
		t.Fatalf("expected handshake cancellation to return promptly, took %s", elapsed)
	}

	select {
	case conn := <-accepted:
		_ = conn.Close()
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for qmp client to connect")
	}

	select {
	case <-serverDone:
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for qmp test server to exit")
	}
}

func newTestQMPClient(t *testing.T, handler func(message map[string]any) map[string]any) (*socketMonitorClient, <-chan map[string]any, func()) {
	t.Helper()

	serverConn, clientConn := net.Pipe()
	commands := make(chan map[string]any, 8)
	done := make(chan struct{})

	go func() {
		defer close(done)
		defer close(commands)
		defer serverConn.Close()

		encoder := json.NewEncoder(serverConn)
		decoder := json.NewDecoder(serverConn)

		if err := encoder.Encode(map[string]any{
			"QMP": map[string]any{
				"version": map[string]any{
					"qemu": map[string]any{
						"major": 8,
						"minor": 2,
						"micro": 0,
					},
					"package": "",
				},
				"capabilities": []string{},
			},
		}); err != nil {
			return
		}

		var handshake map[string]any
		if err := decoder.Decode(&handshake); err != nil {
			return
		}
		commands <- handshake
		if err := encoder.Encode(map[string]any{"return": map[string]any{}}); err != nil {
			return
		}

		for {
			var message map[string]any
			if err := decoder.Decode(&message); err != nil {
				return
			}
			commands <- message

			response := handler(message)
			if response == nil {
				response = map[string]any{"return": map[string]any{}}
			}
			if err := encoder.Encode(response); err != nil {
				return
			}
		}
	}()

	monitor := &socketMonitor{
		conn:    clientConn,
		decoder: qmpwire.NewDecoder(clientConn, 0),
	}
	if err := monitor.Connect(); err != nil {
		t.Fatalf("connect qmp test monitor: %v", err)
	}

	client := &socketMonitorClient{
		monitor: monitor,
		raw:     rawQMP.NewMonitor(monitor),
		session: &qmpwire.Session{Conn: clientConn, RPCTimeout: time.Second},
	}

	cleanup := func() {
		_ = client.Disconnect()
		select {
		case <-done:
		case <-time.After(time.Second):
			t.Fatal("timed out waiting for qmp test server to exit")
		}
	}

	return client, commands, cleanup
}

func assertHandshakeCommand(t *testing.T, commands <-chan map[string]any) {
	t.Helper()
	assertQMPCommand(t, commands, "qmp_capabilities")
}

func assertQMPCommand(t *testing.T, commands <-chan map[string]any, want string) map[string]any {
	t.Helper()

	select {
	case message := <-commands:
		if got := message["execute"]; got != want {
			t.Fatalf("unexpected qmp command: got %v want %v", got, want)
		}
		return message
	case <-time.After(time.Second):
		t.Fatalf("timed out waiting for qmp command %q", want)
	}
	return nil
}

func TestWithRawReportsContextCancellation(t *testing.T) {
	release := make(chan struct{})
	client, commands, cleanup := newTestQMPClient(t, func(map[string]any) map[string]any {
		<-release
		return nil
	})
	defer cleanup()
	assertHandshakeCommand(t, commands)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- client.WithRaw(ctx, func(monitor *rawQMP.Monitor) error { return monitor.Stop() })
	}()
	assertQMPCommand(t, commands, "stop")
	cancel()
	err := <-done
	close(release)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("WithRaw after cancellation: got %v, want %v", err, context.Canceled)
	}
}
