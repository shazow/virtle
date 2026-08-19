package qga

import (
	"context"
	"encoding/base64"
	"fmt"
	"time"
)

// execPollDelay is the delay between exit-status polls.
const execPollDelay = 250 * time.Millisecond

// ExecStartError reports that the guest agent refused to start a guest
// program, most often because the guest has no such program or because it is
// not on the PATH the guest agent inherited from whatever started it. The
// program never ran, so nothing in the guest changed and the caller is free to
// retry with a different program path.
type ExecStartError struct {
	Path string
	Err  error
}

func (e *ExecStartError) Error() string {
	return fmt.Sprintf("guest agent exec %q: %v", e.Path, e.Err)
}

func (e *ExecStartError) Unwrap() error {
	return e.Err
}

// ExecWait configures guest command execution. The command deadline is
// carried by ctx; RunCommandStatus polls until the command exits or ctx ends.
type ExecWait struct {
	Name          string
	Path          string
	Args          []string
	Subject       string
	CaptureOutput bool

	// pollDelay overrides the exit-status poll pacing; only tests set it.
	pollDelay time.Duration
}

// RunCommandStatus starts a guest command and waits for its exit status.
func RunCommandStatus(ctx context.Context, client ExecRunner, wait ExecWait) (ExecStatus, error) {
	pid, err := client.Exec(ctx, wait.Path, wait.Args, wait.CaptureOutput)
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
