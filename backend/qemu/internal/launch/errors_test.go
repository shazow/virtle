package launch

import (
	"errors"
	"testing"

	"github.com/shazow/virtle/internal/executor"
	"github.com/shazow/virtle/internal/executor/executortest"
)

func TestWrapStagePreservesStageAndCause(t *testing.T) {
	cause := errors.New("failed")
	err := wrapStage("vm startup", cause)
	if !errors.Is(err, cause) {
		t.Fatalf("wrapped error does not preserve cause: %v", err)
	}
	var stageErr *StageError
	if !errors.As(err, &stageErr) {
		t.Fatalf("error type: got %T", err)
	}
	if stageErr.Stage != "vm startup" {
		t.Fatalf("stage: got %q want vm startup", stageErr.Stage)
	}
	if got, want := err.Error(), "vm startup: failed"; got != want {
		t.Fatalf("error string: got %q want %q", got, want)
	}
}

func TestWrapFixedStage(t *testing.T) {
	cause := errors.New("failed")
	err := WrapFixedStage("restore")(cause)
	var stageErr *StageError
	if !errors.As(err, &stageErr) {
		t.Fatalf("error type: got %T", err)
	}
	if stageErr.Stage != "restore" || !errors.Is(err, cause) {
		t.Fatalf("wrapped error: %#v", err)
	}
}

func TestWrapCommandError(t *testing.T) {
	cause := errors.New("failed")
	err := wrapCommandError("active session", "ssh", cause)
	if !errors.Is(err, cause) {
		t.Fatalf("wrapped error does not preserve cause: %v", err)
	}
	var commandErr *CommandError
	if !errors.As(err, &commandErr) {
		t.Fatalf("error type: got %T", err)
	}
	if commandErr.Stage != "active session" || commandErr.Command != "ssh" || commandErr.ExitCode != -1 {
		t.Fatalf("command error: %#v", commandErr)
	}
	if got, want := err.Error(), "active session: ssh failed: failed"; got != want {
		t.Fatalf("error string: got %q want %q", got, want)
	}
}

func TestWrapCommandErrorNoopsWithoutError(t *testing.T) {
	if err := wrapCommandError("active session", "ssh", nil); err != nil {
		t.Fatalf("error: got %v want nil", err)
	}
}

func TestWrapHotplugErrorClassifiesStages(t *testing.T) {
	tests := []struct {
		name  string
		err   error
		stage string
	}{
		{name: "guest", err: errors.New("guest command failed"), stage: "hotplug guest"},
		{name: "qmp", err: errors.New("qmp command failed"), stage: "hotplug qmp"},
		{name: "device del", err: errors.New("device_del failed"), stage: "hotplug qmp"},
		{name: "state", err: errors.New("read state failed"), stage: "hotplug state"},
		{name: "default", err: errors.New("missing id"), stage: "hotplug"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := WrapHotplugError(tt.err)
			if !errors.Is(err, tt.err) {
				t.Fatalf("cause: got %v want %v", err, tt.err)
			}
			var stageErr *StageError
			if !errors.As(err, &stageErr) || stageErr.Stage != tt.stage {
				t.Fatalf("stage err: got %v want stage %q", err, tt.stage)
			}
		})
	}
}

func TestWrapHotplugErrorNoopsWithoutError(t *testing.T) {
	if err := WrapHotplugError(nil); err != nil {
		t.Fatalf("error: got %v want nil", err)
	}
}

func TestFirstUnexpectedExitNoopsWithoutExitedProcess(t *testing.T) {
	group := executor.NewGroup((&executortest.Process{OverrideName: "qemu"}).Process())
	if err := firstUnexpectedExit("vm startup", group); err != nil {
		t.Fatalf("unexpected exit: %v", err)
	}
}

func TestFirstUnexpectedExitWrapsCleanExit(t *testing.T) {
	process := &executortest.Process{OverrideName: "qemu"}
	wrapped := process.Process()
	process.Complete(nil)
	<-wrapped.Done()
	group := executor.NewGroup(wrapped)

	err := firstUnexpectedExit("vm startup", group)
	var stageErr *StageError
	if !errors.As(err, &stageErr) {
		t.Fatalf("error type: got %T", err)
	}
	if stageErr.Stage != "vm startup" {
		t.Fatalf("stage: got %q want vm startup", stageErr.Stage)
	}
	if got, want := err.Error(), "vm startup: qemu exited unexpectedly"; got != want {
		t.Fatalf("error string: got %q want %q", got, want)
	}
}

func TestFirstUnexpectedExitWrapsProcessError(t *testing.T) {
	waitErr := errors.New("wait failed")
	process := &executortest.Process{OverrideName: "virtiofsd"}
	wrapped := process.Process()
	process.Complete(waitErr)
	<-wrapped.Done()
	group := executor.NewGroup(wrapped)

	err := firstUnexpectedExit("virtiofs startup", group)
	if !errors.Is(err, waitErr) {
		t.Fatalf("error cause: got %v want %v", err, waitErr)
	}
	var commandErr *CommandError
	if !errors.As(err, &commandErr) {
		t.Fatalf("error type: got %T", err)
	}
	if commandErr.Stage != "virtiofs startup" || commandErr.Command != "virtiofsd" {
		t.Fatalf("command error: %#v", commandErr)
	}
}
