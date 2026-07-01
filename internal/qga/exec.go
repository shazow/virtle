package qga

import (
	"context"
	"encoding/base64"
	"fmt"
	"time"
)

// ExecWait configures guest command execution and polling.
type ExecWait struct {
	Timeout       time.Duration
	PollDelay     time.Duration
	Name          string
	Path          string
	Args          []string
	Subject       string
	CaptureOutput bool
}

// RunCommandStatus starts a guest command and waits for its exit status.
func RunCommandStatus(ctx context.Context, client ExecRunner, wait ExecWait) (ExecStatus, error) {
	pid, err := client.Exec(wait.Timeout, wait.Path, wait.Args, wait.CaptureOutput)
	if err != nil {
		return ExecStatus{}, fmt.Errorf("%s %q: %w", wait.Name, wait.Subject, err)
	}

	deadline := time.Now().Add(wait.Timeout)
	for {
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return ExecStatus{}, fmt.Errorf("%s %q timed out after %s", wait.Name, wait.Subject, wait.Timeout)
		}

		status, err := client.ExecStatus(minDuration(wait.Timeout, remaining), pid)
		if err != nil {
			return ExecStatus{}, fmt.Errorf("%s %q: %w", wait.Name, wait.Subject, err)
		}
		if status.Exited {
			return status, nil
		}

		sleep := minDuration(wait.PollDelay, time.Until(deadline))
		if sleep <= 0 {
			continue
		}
		timer := time.NewTimer(sleep)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ExecStatus{}, ctx.Err()
		case <-timer.C:
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

func minDuration(a time.Duration, b time.Duration) time.Duration {
	if a < b {
		return a
	}
	return b
}
