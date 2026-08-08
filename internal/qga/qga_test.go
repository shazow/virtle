package qga

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/shazow/virtle/internal/qmpwire"
)

func TestClientFileAndExecCommands(t *testing.T) {
	client, commands, cleanup := newTestClient(t, func(message map[string]any) map[string]any {
		switch message["execute"] {
		case "guest-file-open":
			return map[string]any{"return": 42}
		case "guest-file-read":
			return map[string]any{"return": map[string]any{"buf-b64": "aGVsbG8=", "eof": true}}
		case "guest-exec":
			return map[string]any{"return": map[string]any{"pid": 7}}
		case "guest-exec-status":
			return map[string]any{"return": map[string]any{"exited": true, "exitcode": 0, "out-data": "b2s="}}
		default:
			return map[string]any{"return": map[string]any{}}
		}
	})
	defer cleanup()

	handle, err := client.OpenFile(context.Background(), "/tmp/file")
	if err != nil {
		t.Fatalf("open file: %v", err)
	}
	if handle != 42 {
		t.Fatalf("unexpected handle: got %d want 42", handle)
	}
	if err := client.WriteFile(context.Background(), handle, "aGVsbG8="); err != nil {
		t.Fatalf("write file: %v", err)
	}
	payload, eof, err := client.ReadFile(context.Background(), handle, 1024)
	if err != nil {
		t.Fatalf("read file: %v", err)
	}
	if payload != "aGVsbG8=" || !eof {
		t.Fatalf("unexpected read: payload=%q eof=%v", payload, eof)
	}
	pid, err := client.Exec(context.Background(), "/bin/true", []string{"--flag"}, true)
	if err != nil {
		t.Fatalf("exec: %v", err)
	}
	if pid != 7 {
		t.Fatalf("unexpected pid: got %d want 7", pid)
	}
	status, err := client.ExecStatus(context.Background(), pid)
	if err != nil {
		t.Fatalf("exec status: %v", err)
	}
	if !status.Exited || status.ExitCode != 0 || status.OutData != "b2s=" {
		t.Fatalf("unexpected exec status: %#v", status)
	}

	assertCommand(t, commands, "guest-file-open")
	assertCommand(t, commands, "guest-file-write")
	assertCommand(t, commands, "guest-file-read")
	assertCommand(t, commands, "guest-exec")
	assertCommand(t, commands, "guest-exec-status")
}

func TestClientSkipsEvents(t *testing.T) {
	client, commands, cleanup := newTestClient(t, func(message map[string]any) map[string]any {
		return map[string]any{
			"event":  "GUEST_EVENT",
			"return": map[string]any{},
		}
	})
	defer cleanup()

	if err := client.Ping(context.Background()); err != nil {
		t.Fatalf("ping: %v", err)
	}
	assertCommand(t, commands, "guest-ping")
}

func TestClientCancellationInterruptsBlockedRead(t *testing.T) {
	serverConn, clientConn := net.Pipe()
	defer serverConn.Close()
	go func() {
		// Swallow the request and never respond.
		buf := make([]byte, 1024)
		_, _ = serverConn.Read(buf)
	}()

	client := newSocketClient(clientConn, time.Hour)
	defer client.Disconnect()

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(10 * time.Millisecond)
		cancel()
	}()
	start := time.Now()
	err := client.Ping(ctx)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context cancellation, got %v", err)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("cancellation took %s; read was not interrupted", elapsed)
	}
}

func TestClientFailsFastAfterInterruptedCall(t *testing.T) {
	release := make(chan struct{})
	client, _, cleanup := newTestClient(t, func(message map[string]any) map[string]any {
		// Hold the first reply until after the caller gave up, so the stale
		// response is still in flight when the next call would run.
		<-release
		return map[string]any{"return": map[string]any{}}
	})
	defer cleanup()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	if err := client.Ping(ctx); err == nil {
		t.Fatal("expected interrupted ping to fail")
	}
	close(release)

	err := client.Ping(context.Background())
	if !errors.Is(err, qmpwire.ErrBroken) {
		t.Fatalf("expected broken-connection error instead of reading the stale reply, got %v", err)
	}
}

func TestClientRPCTimeoutBoundsUnresponsiveAgent(t *testing.T) {
	serverConn, clientConn := net.Pipe()
	defer serverConn.Close()
	go func() {
		buf := make([]byte, 1024)
		_, _ = serverConn.Read(buf)
	}()

	client := newSocketClient(clientConn, 20*time.Millisecond)
	defer client.Disconnect()

	err := client.Ping(context.Background())
	if err == nil || !strings.Contains(err.Error(), "guest agent unresponsive after 20ms") {
		t.Fatalf("expected unresponsive-agent error, got %v", err)
	}
}

func newSocketClient(conn net.Conn, rpcTimeout time.Duration) *socketClient {
	return &socketClient{
		conn:    conn,
		decoder: json.NewDecoder(conn),
		session: &qmpwire.Session{Conn: conn, RPCTimeout: rpcTimeout},
	}
}

func newTestClient(t *testing.T, handler func(message map[string]any) map[string]any) (*socketClient, <-chan map[string]any, func()) {
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
			if response["event"] != nil {
				if err := encoder.Encode(map[string]any{"event": response["event"]}); err != nil {
					return
				}
				delete(response, "event")
			}
			if err := encoder.Encode(response); err != nil {
				return
			}
		}
	}()

	client := newSocketClient(clientConn, time.Second)
	cleanup := func() {
		_ = client.Disconnect()
		select {
		case <-done:
		case <-time.After(time.Second):
			t.Fatal("timed out waiting for qga test server to exit")
		}
	}
	return client, commands, cleanup
}

func assertCommand(t *testing.T, commands <-chan map[string]any, execute string) {
	t.Helper()

	select {
	case command := <-commands:
		if command["execute"] != execute {
			t.Fatalf("unexpected command: got %#v want execute=%q", command, execute)
		}
	case <-time.After(time.Second):
		t.Fatalf("timed out waiting for command %q", execute)
	}
}
