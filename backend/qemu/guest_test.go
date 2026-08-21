package qemu

import (
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"strings"
	"testing"

	"github.com/shazow/virtle/backend/qemu/internal/qga"
	"github.com/shazow/virtle/vm"
)

// fakeGuestHost hands out a shared fake QGA client.
type fakeGuestHost struct {
	client *fakeQGAClient
	downs  int
}

func (h *fakeGuestHost) DialGuestAgent(ctx context.Context) (qga.Client, error) {
	return h.client, nil
}

func (h *fakeGuestHost) ShutdownGuest(ctx context.Context) error {
	h.downs++
	return nil
}

// fakeQGAClient records exec calls and serves an in-memory file store.
type fakeQGAClient struct {
	execPath string
	execArgs []string
	status   qga.ExecStatus

	files       map[string]string // by handle key
	handlePaths map[int]string
	nextHandle  int
}

func newFakeQGAClient() *fakeQGAClient {
	return &fakeQGAClient{files: map[string]string{}, handlePaths: map[int]string{}}
}

func (c *fakeQGAClient) Ping(ctx context.Context) error { return nil }

func (c *fakeQGAClient) OpenFile(ctx context.Context, path string) (int, error) {
	c.nextHandle++
	c.handlePaths[c.nextHandle] = path
	c.files[path] = ""
	return c.nextHandle, nil
}

func (c *fakeQGAClient) WriteFile(ctx context.Context, handle int, payloadBase64 string) error {
	path, ok := c.handlePaths[handle]
	if !ok {
		return fmt.Errorf("bad handle %d", handle)
	}
	data, err := base64.StdEncoding.DecodeString(payloadBase64)
	if err != nil {
		return err
	}
	c.files[path] += string(data)
	return nil
}

func (c *fakeQGAClient) OpenFileRead(ctx context.Context, path string) (int, error) {
	if _, ok := c.files[path]; !ok {
		return 0, fmt.Errorf("no such guest file %q", path)
	}
	c.nextHandle++
	c.handlePaths[c.nextHandle] = path
	return c.nextHandle, nil
}

func (c *fakeQGAClient) ReadFile(ctx context.Context, handle int, count int) (string, bool, error) {
	path, ok := c.handlePaths[handle]
	if !ok {
		return "", false, fmt.Errorf("bad handle %d", handle)
	}
	content := c.files[path]
	// Serve one byte per call to exercise chunked reads.
	if content == "" {
		return "", true, nil
	}
	chunk, rest := content[:1], content[1:]
	c.files[path] = rest
	return base64.StdEncoding.EncodeToString([]byte(chunk)), rest == "", nil
}

func (c *fakeQGAClient) CloseFile(ctx context.Context, handle int) error {
	delete(c.handlePaths, handle)
	return nil
}

func (c *fakeQGAClient) Exec(ctx context.Context, path string, args []string, env []string, captureOutput bool) (int, error) {
	c.execPath = path
	c.execArgs = args
	return 42, nil
}

func (c *fakeQGAClient) ExecStatus(ctx context.Context, pid int) (qga.ExecStatus, error) {
	status := c.status
	status.Exited = true
	return status, nil
}

func (c *fakeQGAClient) Shutdown(ctx context.Context) error { return nil }
func (c *fakeQGAClient) Disconnect() error                  { return nil }

func TestQGAGuestRun(t *testing.T) {
	client := newFakeQGAClient()
	client.status = qga.ExecStatus{
		ExitCode: 3,
		OutData:  base64.StdEncoding.EncodeToString([]byte("out")),
		ErrData:  base64.StdEncoding.EncodeToString([]byte("err")),
	}
	g := &qgaGuest{vm: &fakeGuestHost{client: client}}

	result, err := g.Run(context.Background(), &vm.GuestCmd{Path: "make", Args: []string{"-j2"}})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if client.execPath != "make" || len(client.execArgs) != 1 || client.execArgs[0] != "-j2" {
		t.Errorf("exec = %q %v", client.execPath, client.execArgs)
	}
	if result.ExitCode != 3 || string(result.Stdout) != "out" || string(result.Stderr) != "err" {
		t.Errorf("result = %+v", result)
	}
}

func TestQGAGuestRunDirEnvWrapsShell(t *testing.T) {
	client := newFakeQGAClient()
	g := &qgaGuest{vm: &fakeGuestHost{client: client}}

	if _, err := g.Run(context.Background(), &vm.GuestCmd{
		Path: "make", Dir: "/workspace", Env: []string{"FOO=bar"},
	}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if client.execPath != "/bin/sh" {
		t.Fatalf("exec path = %q, want /bin/sh", client.execPath)
	}
	script := strings.Join(client.execArgs, " ")
	for _, want := range []string{"cd /workspace", "FOO=bar", "exec make"} {
		if !strings.Contains(script, want) {
			t.Errorf("shell script %q missing %q", script, want)
		}
	}
}

func TestQGAGuestRunStdinUnsupported(t *testing.T) {
	g := &qgaGuest{vm: &fakeGuestHost{client: newFakeQGAClient()}}
	if _, err := g.Run(context.Background(), &vm.GuestCmd{Path: "cat", Stdin: strings.NewReader("x")}); err == nil {
		t.Fatal("expected stdin to be unsupported over QGA")
	}
}

func TestQGAGuestCreateThenOpen(t *testing.T) {
	client := newFakeQGAClient()
	g := &qgaGuest{vm: &fakeGuestHost{client: client}}
	ctx := context.Background()

	w, err := g.Create(ctx, "/etc/motd", 0)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if _, err := io.WriteString(w, "hello guest"); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	r, err := g.Open(ctx, "/etc/motd")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	data, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if err := r.Close(); err != nil {
		t.Fatalf("Close reader: %v", err)
	}
	if string(data) != "hello guest" {
		t.Errorf("read back %q, want %q", data, "hello guest")
	}
}

func TestQGAGuestShutdown(t *testing.T) {
	host := &fakeGuestHost{client: newFakeQGAClient()}
	g := &qgaGuest{vm: host}
	if err := g.Shutdown(context.Background()); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
	if host.downs != 1 {
		t.Errorf("shutdown requests = %d, want 1", host.downs)
	}
}
