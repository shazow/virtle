package qga

import (
	"encoding/json"
	"net"
	"testing"
	"time"
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

	handle, err := client.OpenFile(time.Second, "/tmp/file")
	if err != nil {
		t.Fatalf("open file: %v", err)
	}
	if handle != 42 {
		t.Fatalf("unexpected handle: got %d want 42", handle)
	}
	if err := client.WriteFile(time.Second, handle, "aGVsbG8="); err != nil {
		t.Fatalf("write file: %v", err)
	}
	payload, eof, err := client.ReadFile(time.Second, handle, 1024)
	if err != nil {
		t.Fatalf("read file: %v", err)
	}
	if payload != "aGVsbG8=" || !eof {
		t.Fatalf("unexpected read: payload=%q eof=%v", payload, eof)
	}
	pid, err := client.Exec(time.Second, "/bin/true", []string{"--flag"}, true)
	if err != nil {
		t.Fatalf("exec: %v", err)
	}
	if pid != 7 {
		t.Fatalf("unexpected pid: got %d want 7", pid)
	}
	status, err := client.ExecStatus(time.Second, pid)
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

	if err := client.Ping(time.Second); err != nil {
		t.Fatalf("ping: %v", err)
	}
	assertCommand(t, commands, "guest-ping")
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

	client := &socketClient{
		conn:    clientConn,
		decoder: json.NewDecoder(clientConn),
	}
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
