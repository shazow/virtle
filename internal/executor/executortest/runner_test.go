package executortest

import (
	"errors"
	"os"
	"os/exec"
	"reflect"
	"syscall"
	"testing"

	"github.com/shazow/virtle/internal/executor"
)

func TestRunnerRecordsCommandMetadata(t *testing.T) {
	runner := &Runner{}
	cmd := executor.Command("/tmp/bin/worker", []string{"--flag"}, []string{"EXTRA=1"})
	cmd.Dir = "/tmp/work"
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	process, err := runner.Start(cmd)
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	if process.Name() != "worker" {
		t.Fatalf("process name: got %q want worker", process.Name())
	}

	starts := runner.Starts()
	if len(starts) != 1 {
		t.Fatalf("starts: got %d want 1", len(starts))
	}
	start := starts[0]
	if start.Name != "worker" {
		t.Fatalf("name: got %q want worker", start.Name)
	}
	if got, want := start.Args, []string{"--flag"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("args: got %v want %v", got, want)
	}
	if got, want := start.EnvAdditions, []string{"EXTRA=1"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("env additions: got %v want %v", got, want)
	}
	if start.Dir != "/tmp/work" {
		t.Fatalf("dir: got %q want /tmp/work", start.Dir)
	}
	if !start.ProcessGroup {
		t.Fatal("expected process group")
	}
	if start.Cmd != cmd {
		t.Fatal("expected original command pointer")
	}
}

func TestRunnerReturnsStartErrors(t *testing.T) {
	runner := &Runner{StartErrors: map[string]error{"worker": errors.New("boom")}}

	_, err := runner.Start(exec.Command("/tmp/bin/worker"))
	if err == nil || err.Error() != "boom" {
		t.Fatalf("start error: got %v want boom", err)
	}
}

func TestRunnerRecordsProcessSignals(t *testing.T) {
	runner := &Runner{}
	process, err := runner.Start(exec.Command("/tmp/bin/worker"))
	if err != nil {
		t.Fatalf("start: %v", err)
	}

	if err := process.Signal(syscall.SIGTERM); err != nil {
		t.Fatalf("signal: %v", err)
	}
	signals := runner.ProcessSignals()
	if got, want := len(signals), 1; got != want {
		t.Fatalf("signals: got %d want %d", got, want)
	}
	if signals[0].Name != "worker" || signals[0].Signal != syscall.SIGTERM {
		t.Fatalf("unexpected signal: %#v", signals[0])
	}
}

func TestRunnerOnStartCanCustomizeProcess(t *testing.T) {
	runner := &Runner{}
	runner.OnStart = func(start Start) (*Process, error) {
		process := ProcessFor(start.Name)
		process.IgnoreSignals = true
		process.Complete(os.ErrClosed)
		return process, nil
	}

	process, err := runner.Start(exec.Command("/tmp/bin/worker"))
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	if err := process.Wait(); !errors.Is(err, os.ErrClosed) {
		t.Fatalf("wait: got %v want %v", err, os.ErrClosed)
	}
	if got, want := len(runner.Processes("worker")), 1; got != want {
		t.Fatalf("tracked processes: got %d want %d", got, want)
	}
	if err := process.Kill(); err != nil {
		t.Fatalf("kill: %v", err)
	}
	if got, want := len(runner.ProcessSignals()), 1; got != want {
		t.Fatalf("process signals: got %d want %d", got, want)
	}
}
