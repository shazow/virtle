package qga

import (
	"context"
	"encoding/base64"
	"fmt"
	"time"
)

const (
	// InternalCommandPathEnv is the default PATH used to find Virtle's internal
	// guest commands. It covers common distros such as BusyBox/Alpine,
	// Debian/Ubuntu, and NixOS/Guix, whose service PATH may omit system commands.
	// TODO: Add some way for users to bring their own path, or better yet: preload guest's default PATH and prefix it here.
	InternalCommandPathEnv = "PATH=/bin:/usr/bin:/run/current-system/sw/bin:/run/current-system/profile/bin"

	// execPollDelay is the delay between exit-status polls.
	execPollDelay = 250 * time.Millisecond
)

// ExecWait configures guest command execution. The command deadline is
// carried by ctx; RunCommandStatus polls until the command exits or ctx ends.
type ExecWait struct {
	Name string
	Path string
	Args []string
	// Env replaces the guest agent's inherited environment when non-empty.
	Env           []string
	Subject       string
	CaptureOutput bool

	// pollDelay overrides the exit-status poll pacing; only tests set it.
	pollDelay time.Duration
}

// RunCommandStatus starts a guest command and waits for its exit status.
func RunCommandStatus(ctx context.Context, client ExecRunner, wait ExecWait) (ExecStatus, error) {
	pid, err := client.Exec(ctx, wait.Path, wait.Args, wait.Env, wait.CaptureOutput)
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
