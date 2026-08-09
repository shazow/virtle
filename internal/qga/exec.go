package qga

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"time"
)

// execPollDelay is the delay between exit-status polls.
const execPollDelay = 250 * time.Millisecond

// ExecWait configures guest command execution. The command deadline is
// carried by ctx; RunCommandStatus polls until the command exits or ctx ends.
type ExecWait struct {
	Name          string
	Path          string
	Args          []string
	Subject       string
	CaptureOutput bool

	// Env holds "key=value" entries added to the guest environment, and
	// InputData is fed to the command's standard input. Both need a client
	// that implements OptionsExecRunner.
	Env       []string
	InputData []byte

	// pollDelay overrides the exit-status poll pacing; only tests set it.
	pollDelay time.Duration
}

// ExecOptions carries the guest-exec fields beyond the command itself.
type ExecOptions struct {
	Env           []string
	InputData     []byte
	CaptureOutput bool
}

// OptionsExecRunner is implemented by clients that can pass an environment and
// standard input along with a guest command.
type OptionsExecRunner interface {
	ExecWithOptions(ctx context.Context, path string, args []string, opts ExecOptions) (int, error)
}

// startExec launches the command, using the richer guest-exec form only when
// the wait needs it, so clients that implement just ExecRunner keep working.
func startExec(ctx context.Context, client ExecRunner, wait ExecWait) (int, error) {
	if len(wait.Env) == 0 && len(wait.InputData) == 0 {
		return client.Exec(ctx, wait.Path, wait.Args, wait.CaptureOutput)
	}
	runner, ok := client.(OptionsExecRunner)
	if !ok {
		return 0, fmt.Errorf("guest agent client does not support exec environment or input: %w", errors.ErrUnsupported)
	}
	return runner.ExecWithOptions(ctx, wait.Path, wait.Args, ExecOptions{
		Env:           wait.Env,
		InputData:     wait.InputData,
		CaptureOutput: wait.CaptureOutput,
	})
}

// RunCommandStatus starts a guest command and waits for its exit status.
func RunCommandStatus(ctx context.Context, client ExecRunner, wait ExecWait) (ExecStatus, error) {
	pid, err := startExec(ctx, client, wait)
	if err != nil {
		return ExecStatus{}, fmt.Errorf("%s %q: %w", wait.Name, wait.Subject, err)
	}

	pollDelay := wait.pollDelay
	if pollDelay <= 0 {
		pollDelay = execPollDelay
	}
	ticker := time.NewTicker(pollDelay)
	defer ticker.Stop()

	for {
		status, err := client.ExecStatus(ctx, pid)
		if err != nil {
			return ExecStatus{}, fmt.Errorf("%s %q: %w", wait.Name, wait.Subject, err)
		}
		if status.Exited {
			return status, nil
		}

		select {
		case <-ctx.Done():
			return ExecStatus{}, fmt.Errorf("%s %q: %w", wait.Name, wait.Subject, context.Cause(ctx))
		case <-ticker.C:
		}
	}
}

// ExecOutputSuffix formats decoded stdout and stderr for error messages.
func ExecOutputSuffix(status ExecStatus) string {
	stdout := DecodeExecData(status.OutData)
	stderr := DecodeExecData(status.ErrData)
	switch {
	case stdout != "" && stderr != "":
		return fmt.Sprintf(": stdout=%q stderr=%q", stdout, stderr)
	case stdout != "":
		return fmt.Sprintf(": stdout=%q", stdout)
	case stderr != "":
		return fmt.Sprintf(": stderr=%q", stderr)
	default:
		return ""
	}
}

// DecodeExecData decodes guest-agent base64 output, falling back to raw data.
func DecodeExecData(data string) string {
	if data == "" {
		return ""
	}
	decoded, err := base64.StdEncoding.DecodeString(data)
	if err != nil {
		return data
	}
	return string(decoded)
}
