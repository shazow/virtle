package qemu

import (
	"context"
	"encoding/base64"
	"fmt"
	"sync"
	"time"

	rawQMP "github.com/digitalocean/go-qemu/qmp/raw"
	"github.com/shazow/virtle/internal/executor/executortest"
	"github.com/shazow/virtle/internal/qga"
	"github.com/shazow/virtle/internal/qmpclient"
)

// testBackend returns a backend whose host interactions — starting processes,
// waiting for sockets, dialing QEMU — are stubbed, so a launch can be exercised
// without QEMU or a guest.
func testBackend(config Config) (*qemuBackend, *executortest.Runner, *fakeQMP, *fakeGuestAgent) {
	runner := &executortest.Runner{}
	monitor := &fakeQMP{status: "running"}
	agent := newFakeGuestAgent()

	b := newBackend(config)
	b.guestControl = (*instance).qgaGuestControl
	b.runner = runner
	b.socketWaiter = readySocketWaiter{}
	b.cidChecker = freeCIDChecker{}
	b.qmpDialer = fakeQMPDialer{client: monitor}
	b.qgaDialer = fakeGuestAgentDialer{client: agent}
	return b, runner, monitor, agent
}

type readySocketWaiter struct{}

func (readySocketWaiter) Wait(context.Context, []string) error { return nil }

type freeCIDChecker struct{}

func (freeCIDChecker) Available(int) (bool, error) { return true, nil }

type fakeQMPDialer struct{ client *fakeQMP }

func (d fakeQMPDialer) Dial(context.Context, string, time.Duration) (qmpclient.Client, error) {
	return d.client, nil
}

// fakeQMP records the monitor commands a launch issues.
type fakeQMP struct {
	mu           sync.Mutex
	calls        []string
	status       string
	disconnected bool
}

func (q *fakeQMP) record(call string) {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.calls = append(q.calls, call)
}

func (q *fakeQMP) Calls() []string {
	q.mu.Lock()
	defer q.mu.Unlock()
	return append([]string(nil), q.calls...)
}

// WithRaw is unreachable in tests: the one raw-monitor caller in this package
// is the balloon resize, which tests replace through the backend's
// setBalloonSize seam.
func (q *fakeQMP) WithRaw(context.Context, func(*rawQMP.Monitor) error) error {
	q.record("with-raw")
	return fmt.Errorf("fake qmp has no raw monitor")
}

func (q *fakeQMP) RunRaw(_ context.Context, command string) error {
	q.record("run-raw:" + command)
	return nil
}

func (q *fakeQMP) DeviceDelAndWait(_ context.Context, id string) error {
	q.record("device-del:" + id)
	return nil
}

func (q *fakeQMP) Stop(context.Context) error {
	q.record("stop")
	q.mu.Lock()
	q.status = "paused"
	q.mu.Unlock()
	return nil
}

func (q *fakeQMP) Cont(context.Context) error {
	q.record("cont")
	q.mu.Lock()
	q.status = "running"
	q.mu.Unlock()
	return nil
}

func (q *fakeQMP) QueryStatus(context.Context) (string, error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.status, nil
}

func (q *fakeQMP) Quit(context.Context) error {
	q.record("quit")
	return nil
}

func (q *fakeQMP) MigrateToFile(_ context.Context, path string) error {
	q.record("migrate-to-file:" + path)
	return nil
}

func (q *fakeQMP) MigrateIncoming(_ context.Context, path string) error {
	q.record("migrate-incoming:" + path)
	return nil
}

func (q *fakeQMP) QueryMigrate(context.Context) (string, error) {
	return "completed", nil
}

func (q *fakeQMP) Disconnect() error {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.disconnected = true
	return nil
}

type fakeGuestAgentDialer struct{ client *fakeGuestAgent }

func (d fakeGuestAgentDialer) Dial(context.Context, string, time.Duration) (qga.Client, error) {
	return d.client, nil
}

// fakeGuestAgent is an in-memory guest: files land in a map, commands report a
// canned result.
type fakeGuestAgent struct {
	mu       sync.Mutex
	files    map[string][]byte
	handles  map[int]string
	nextID   int
	commands []string
	exit     int
	stdout   string
	shutdown bool
	closed   bool

	// onShutdown runs when the guest is asked to power down, so tests can
	// end the fake QEMU process the way a real power-down would.
	onShutdown func()
}

func newFakeGuestAgent() *fakeGuestAgent {
	return &fakeGuestAgent{files: map[string][]byte{}, handles: map[int]string{}}
}

func (a *fakeGuestAgent) File(name string) (string, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	data, ok := a.files[name]
	return string(data), ok
}

func (a *fakeGuestAgent) Commands() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]string(nil), a.commands...)
}

func (a *fakeGuestAgent) Ping(context.Context) error { return nil }

func (a *fakeGuestAgent) OpenFile(_ context.Context, path string) (int, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.nextID++
	a.handles[a.nextID] = path
	a.files[path] = nil
	return a.nextID, nil
}

func (a *fakeGuestAgent) OpenFileRead(_ context.Context, path string) (int, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if _, ok := a.files[path]; !ok {
		return 0, fmt.Errorf("guest file %q does not exist", path)
	}
	a.nextID++
	a.handles[a.nextID] = path
	return a.nextID, nil
}

func (a *fakeGuestAgent) WriteFile(_ context.Context, handle int, payloadBase64 string) error {
	chunk, err := base64.StdEncoding.DecodeString(payloadBase64)
	if err != nil {
		return err
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	path, ok := a.handles[handle]
	if !ok {
		return fmt.Errorf("unknown guest file handle %d", handle)
	}
	a.files[path] = append(a.files[path], chunk...)
	return nil
}

func (a *fakeGuestAgent) ReadFile(_ context.Context, handle int, count int) (string, bool, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	path, ok := a.handles[handle]
	if !ok {
		return "", false, fmt.Errorf("unknown guest file handle %d", handle)
	}
	data := a.files[path]
	if len(data) > count {
		a.files[path] = data[count:]
		return base64.StdEncoding.EncodeToString(data[:count]), false, nil
	}
	a.files[path] = nil
	return base64.StdEncoding.EncodeToString(data), true, nil
}

func (a *fakeGuestAgent) CloseFile(_ context.Context, handle int) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	delete(a.handles, handle)
	return nil
}

func (a *fakeGuestAgent) Exec(ctx context.Context, path string, args []string, captureOutput bool) (int, error) {
	return a.ExecWithOptions(ctx, path, args, qga.ExecOptions{CaptureOutput: captureOutput})
}

func (a *fakeGuestAgent) ExecWithOptions(_ context.Context, path string, args []string, opts qga.ExecOptions) (int, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	command := path
	for _, arg := range args {
		command += " " + arg
	}
	for _, env := range opts.Env {
		command += " env:" + env
	}
	if len(opts.InputData) > 0 {
		command += " stdin:" + string(opts.InputData)
	}
	a.commands = append(a.commands, command)
	return len(a.commands), nil
}

func (a *fakeGuestAgent) ExecStatus(context.Context, int) (qga.ExecStatus, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return qga.ExecStatus{
		Exited:   true,
		ExitCode: a.exit,
		OutData:  base64.StdEncoding.EncodeToString([]byte(a.stdout)),
	}, nil
}

func (a *fakeGuestAgent) Shutdown(context.Context) error {
	a.mu.Lock()
	a.shutdown = true
	onShutdown := a.onShutdown
	a.mu.Unlock()
	if onShutdown != nil {
		onShutdown()
	}
	return nil
}

func (a *fakeGuestAgent) Disconnect() error {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.closed = true
	return nil
}
