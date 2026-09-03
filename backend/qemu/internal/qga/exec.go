package qga

import (
	"context"
	"encoding/base64"
	"fmt"
	"time"

	shellquote "github.com/kballard/go-shellquote"
)

const (
	// InternalCommandPathEnv is the PATH virtle supplies for its own guest
	// commands (chmod, chown, install, mount, ps, ...). QGA resolves a relative
	// command against the PATH in the supplied environment, falling back to the
	// agent's own PATH only when the environment carries none, and the agent's
	// service PATH often omits system directories. The list covers
	// BusyBox/Alpine, Debian/Ubuntu, and NixOS/Guix layouts. User commands
	// choose their own PATH through vm.GuestCmd.Env.
	InternalCommandPathEnv = "PATH=/bin:/usr/bin:/run/current-system/sw/bin:/run/current-system/profile/bin"

	// execPollDelay is the delay between exit-status polls.
	execPollDelay = 250 * time.Millisecond
)

// ShellCommand lowers a working directory and environment onto a /bin/sh
// wrapper: guest-exec has no working directory, and its env parameter
// replaces rather than augments the inherited environment. Without dir or
// env it returns path and args unchanged.
func ShellCommand(path string, args, env []string, dir string) (string, []string) {
	if dir == "" && len(env) == 0 {
		return path, args
	}
	script := "exec " + shellquote.Join(append([]string{path}, args...)...)
	if dir != "" {
		script = "cd " + shellquote.Join(dir) + " && " + script
	}
	if len(env) > 0 {
		script = "export " + shellquote.Join(env...) + " && " + script
	}
	return "/bin/sh", []string{"-c", script}
}

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
