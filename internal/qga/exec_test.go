package qga

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestRunCommandStatusWaitsForExit(t *testing.T) {
	client := &execClient{
		pid: 7,
		statuses: []ExecStatus{
			{Exited: false},
			{Exited: true, ExitCode: 3, OutData: "b2s="},
		},
	}
	status, err := RunCommandStatus(context.Background(), client, ExecWait{
		Timeout:       time.Second,
		PollDelay:     time.Millisecond,
		Name:          "test",
		Path:          "/bin/test",
		Args:          []string{"-e", "/tmp/file"},
		Subject:       "/tmp/file",
		CaptureOutput: true,
	})
	if err != nil {
		t.Fatalf("run command status: %v", err)
	}
	if status.ExitCode != 3 || status.OutData != "b2s=" {
		t.Fatalf("unexpected status: %+v", status)
	}
	if client.execPath != "/bin/test" || len(client.execArgs) != 2 || !client.capture {
		t.Fatalf("unexpected exec call: path=%q args=%v capture=%v", client.execPath, client.execArgs, client.capture)
	}
	if client.statusCalls != 2 {
		t.Fatalf("status calls: got %d want 2", client.statusCalls)
	}
}

func TestRunCommandStatusWrapsExecStatusError(t *testing.T) {
	wantErr := errors.New("status failed")
	client := &execClient{pid: 7, statusErr: wantErr}
	_, err := RunCommandStatus(context.Background(), client, ExecWait{
		Timeout:   time.Second,
		PollDelay: time.Millisecond,
		Name:      "chmod",
		Path:      "/bin/chmod",
		Subject:   "/tmp/file",
	})
	if !errors.Is(err, wantErr) || !strings.Contains(err.Error(), `chmod "/tmp/file"`) {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestRunCommandStatusReturnsContextCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	client := &execClient{pid: 7, statuses: []ExecStatus{{Exited: false}}}
	cancel()
	_, err := RunCommandStatus(ctx, client, ExecWait{
		Timeout:   time.Second,
		PollDelay: time.Hour,
		Name:      "test",
		Path:      "/bin/test",
		Subject:   "/tmp/file",
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("cancel error: got %v want %v", err, context.Canceled)
	}
}

func TestExecOutputSuffix(t *testing.T) {
	status := ExecStatus{OutData: "b3V0", ErrData: "ZXJy"}
	if got, want := ExecOutputSuffix(status), `: stdout="out" stderr="err"`; got != want {
		t.Fatalf("suffix: got %q want %q", got, want)
	}
	status = ExecStatus{ErrData: "not-base64"}
	if got, want := ExecOutputSuffix(status), `: stderr="not-base64"`; got != want {
		t.Fatalf("suffix: got %q want %q", got, want)
	}
}

func TestDecodeExecDataFallsBackToRawData(t *testing.T) {
	if got, want := DecodeExecData("b2s="), "ok"; got != want {
		t.Fatalf("decoded: got %q want %q", got, want)
	}
	if got, want := DecodeExecData("not-base64"), "not-base64"; got != want {
		t.Fatalf("fallback: got %q want %q", got, want)
	}
}

type execClient struct {
	pid         int
	execPath    string
	execArgs    []string
	capture     bool
	execErr     error
	statuses    []ExecStatus
	statusErr   error
	statusCalls int
}

func (c *execClient) Exec(_ time.Duration, path string, args []string, captureOutput bool) (int, error) {
	c.execPath = path
	c.execArgs = append([]string(nil), args...)
	c.capture = captureOutput
	return c.pid, c.execErr
}
func (c *execClient) ExecStatus(time.Duration, int) (ExecStatus, error) {
	c.statusCalls++
	if c.statusErr != nil {
		return ExecStatus{}, c.statusErr
	}
	if len(c.statuses) == 0 {
		return ExecStatus{Exited: true}, nil
	}
	status := c.statuses[0]
	if len(c.statuses) > 1 {
		c.statuses = c.statuses[1:]
	}
	return status, nil
}
func (c *execClient) Disconnect() error { return nil }
