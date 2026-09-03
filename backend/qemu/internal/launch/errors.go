package launch

import (
	"errors"
	"fmt"
	"os/exec"
	"strings"

	"github.com/shazow/virtle/internal/executor"
)

type CommandError struct {
	Stage    string
	Command  string
	ExitCode int
	Err      error
}

func (e *CommandError) Error() string {
	if e.ExitCode >= 0 {
		return fmt.Sprintf("%s: %s exited with code %d: %v", e.Stage, e.Command, e.ExitCode, e.Err)
	}
	return fmt.Sprintf("%s: %s failed: %v", e.Stage, e.Command, e.Err)
}

func (e *CommandError) Unwrap() error {
	return e.Err
}

type StageError struct {
	Stage string
	Err   error
}

func (e *StageError) Error() string {
	return fmt.Sprintf("%s: %v", e.Stage, e.Err)
}

func (e *StageError) Unwrap() error {
	return e.Err
}

var ErrSavedSuspendExit = errors.New("saved suspend requested")

func IsSavedSuspendExit(err error) bool {
	return errors.Is(err, ErrSavedSuspendExit)
}

// WrapStage attributes err to a launch stage.
func WrapStage(stage string, err error) error {
	return &StageError{Stage: stage, Err: err}
}

func wrapCommandError(stage string, command string, err error) error {
	if err == nil {
		return nil
	}

	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return &CommandError{
			Stage:    stage,
			Command:  command,
			ExitCode: exitErr.ExitCode(),
			Err:      err,
		}
	}

	return &CommandError{
		Stage:    stage,
		Command:  command,
		ExitCode: -1,
		Err:      err,
	}
}

func WrapHotplugError(err error) error {
	if err == nil {
		return nil
	}
	message := err.Error()
	switch {
	case strings.Contains(message, "guest command"):
		return WrapStage("hotplug guest", err)
	case strings.Contains(message, "qmp"), strings.Contains(message, "device_del"), strings.Contains(message, "chardev"), strings.Contains(message, "netdev"), strings.Contains(message, "blockdev"):
		return WrapStage("hotplug qmp", err)
	case strings.Contains(message, "state"):
		return WrapStage("hotplug state", err)
	default:
		return WrapStage("hotplug", err)
	}
}

func firstUnexpectedExit(stage string, watchers executor.Group) error {
	process, err, ok := watchers.FirstExit()
	if !ok {
		return nil
	}
	if err == nil {
		return WrapStage(stage, fmt.Errorf("%s exited unexpectedly", process.Name()))
	}
	return wrapCommandError(stage, process.Name(), err)
}
