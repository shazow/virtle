package balloon

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	doQMP "github.com/digitalocean/go-qemu/qmp"
	rawQMP "github.com/digitalocean/go-qemu/qmp/raw"
)

func TestQMPSessionQueryBalloon(t *testing.T) {
	session, commands := newTestQMPSession(t, func(message map[string]any) map[string]any {
		return map[string]any{
			"return": map[string]any{
				"actual": int64(512) * bytesPerMiB,
			},
		}
	})

	info, err := session.QueryBalloon(time.Second)
	if err != nil {
		t.Fatalf("query balloon: %v", err)
	}
	if got, want := info.ActualBytes, int64(512)*bytesPerMiB; got != want {
		t.Fatalf("unexpected balloon actual: got %d want %d", got, want)
	}

	assertQMPCommand(t, commands, "query-balloon")
}

func TestQMPSessionSetBalloonLogicalSize(t *testing.T) {
	session, commands := newTestQMPSession(t, func(message map[string]any) map[string]any {
		return map[string]any{"return": map[string]any{}}
	})

	if err := session.SetBalloonLogicalSize(time.Second, int64(640)*bytesPerMiB); err != nil {
		t.Fatalf("set balloon logical size: %v", err)
	}

	command := assertQMPCommand(t, commands, "balloon")
	args := commandArguments(t, command)
	if got, want := int64(args["value"].(float64)), int64(640)*bytesPerMiB; got != want {
		t.Fatalf("unexpected balloon value: got %d want %d", got, want)
	}
}

func TestQMPSessionEnableBalloonStatsPolling(t *testing.T) {
	session, commands := newTestQMPSession(t, func(message map[string]any) map[string]any {
		return map[string]any{"return": map[string]any{}}
	})

	if err := session.EnableBalloonStatsPolling(time.Second, "/machine/peripheral/balloon0", 5); err != nil {
		t.Fatalf("enable balloon stats polling: %v", err)
	}

	command := assertQMPCommand(t, commands, "qom-set")
	args := commandArguments(t, command)
	if got, want := args["path"], "/machine/peripheral/balloon0"; got != want {
		t.Fatalf("unexpected qom-set path: got %v want %v", got, want)
	}
	if got, want := args["property"], "guest-stats-polling-interval"; got != want {
		t.Fatalf("unexpected qom-set property: got %v want %v", got, want)
	}
	if got, want := int64(args["value"].(float64)), int64(5); got != want {
		t.Fatalf("unexpected qom-set value: got %d want %d", got, want)
	}
}

func TestQMPSessionReadBalloonStats(t *testing.T) {
	session, commands := newTestQMPSession(t, func(message map[string]any) map[string]any {
		return map[string]any{
			"return": map[string]any{
				"stats": map[string]any{
					"stat-available-memory": int64(123) * bytesPerMiB,
					"stat-free-memory":      int64(45) * bytesPerMiB,
				},
				"last-update": 1_700_000_000,
			},
		}
	})

	stats, err := session.ReadBalloonStats(time.Second, "/machine/peripheral/balloon0")
	if err != nil {
		t.Fatalf("read balloon stats: %v", err)
	}
	if got, want := stats.Stats["stat-available-memory"], int64(123)*bytesPerMiB; got != want {
		t.Fatalf("unexpected stat-available-memory: got %d want %d", got, want)
	}
	if got, want := stats.LastUpdate.Unix(), int64(1_700_000_000); got != want {
		t.Fatalf("unexpected last-update: got %d want %d", got, want)
	}

	command := assertQMPCommand(t, commands, "qom-get")
	args := commandArguments(t, command)
	if got, want := args["path"], "/machine/peripheral/balloon0"; got != want {
		t.Fatalf("unexpected qom-get path: got %v want %v", got, want)
	}
	if got, want := args["property"], "guest-stats"; got != want {
		t.Fatalf("unexpected qom-get property: got %v want %v", got, want)
	}
}

func newTestQMPSession(t *testing.T, handler func(message map[string]any) map[string]any) (session, <-chan map[string]any) {
	t.Helper()

	monitor := &fakeMonitor{
		handler:  handler,
		commands: make(chan map[string]any, 8),
	}
	rawMonitor := rawQMP.NewMonitor(monitor)
	return newQMPSession(&fakeRawSession{monitor: rawMonitor}), monitor.commands
}

type fakeRawSession struct {
	monitor *rawQMP.Monitor
}

func (s *fakeRawSession) WithRaw(timeout time.Duration, fn func(*rawQMP.Monitor) error) error {
	return fn(s.monitor)
}

type fakeMonitor struct {
	handler  func(message map[string]any) map[string]any
	commands chan map[string]any
}

func (m *fakeMonitor) Connect() error {
	return nil
}

func (m *fakeMonitor) Disconnect() error {
	return nil
}

func (m *fakeMonitor) Run(command []byte) ([]byte, error) {
	var message map[string]any
	if err := json.Unmarshal(command, &message); err != nil {
		return nil, err
	}
	m.commands <- message

	response := map[string]any{"return": map[string]any{}}
	if m.handler != nil {
		response = m.handler(message)
	}
	return json.Marshal(response)
}

func (m *fakeMonitor) Events(context.Context) (<-chan doQMP.Event, error) {
	return nil, doQMP.ErrEventsNotSupported
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

func commandArguments(t *testing.T, message map[string]any) map[string]any {
	t.Helper()

	args, ok := message["arguments"].(map[string]any)
	if !ok {
		t.Fatalf("expected command arguments, got %#v", message["arguments"])
	}
	return args
}
