package vmm

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os/exec"
	"path/filepath"
	"sort"

	"github.com/shazow/virtle/backend/qemu/internal/launch"
	"github.com/shazow/virtle/internal/executor"
	"github.com/shazow/virtle/internal/manifest"
)

const (
	notifyStateRuntimeSuspend = "runtime:suspend"
	notifyStateRuntimeResume  = "runtime:resume"
)

func notifyRuntimeResume(ctx context.Context, plan *launch.Plan) {
	if plan == nil || plan.Notifier == nil || plan.ResumeState == nil {
		return
	}
	plan.Notifier.Notify(ctx, notifyStateRuntimeResume, "Restored saved VM state", map[string]string{
		"host_name":     plan.Manifest.Identity.HostName,
		"vm_state_path": plan.ResumeState.VMStatePath,
		"cid":           fmt.Sprintf("%d", plan.CID),
	})
}

func notifyRuntimeSuspend(ctx context.Context, notifier launch.NotificationSink, state launch.SuspendState) {
	if notifier == nil {
		return
	}
	notifier.Notify(ctx, notifyStateRuntimeSuspend, "Saved VM suspend state", map[string]string{
		"host_name":       state.HostName,
		"qmp_socket_path": state.QMPSocketPath,
		"vm_state_path":   state.VMStatePath,
		"cid":             fmt.Sprintf("%d", state.CID),
	})
}

type noopNotifier struct{}

func (noopNotifier) Notify(context.Context, string, string, map[string]string) {}

type commandNotifier struct {
	command manifest.Command
	states  map[string]struct{}
	dir     string
	logger  *slog.Logger
	output  io.Writer
}

func newCommandNotifier(manifest *manifest.Manifest, logger *slog.Logger, output io.Writer) launch.NotificationSink {
	if manifest == nil {
		return noopNotifier{}
	}
	notifications := manifest.Notifications
	if notifications.Command.IsZero() || notifications.Command.Path == "" {
		return noopNotifier{}
	}
	// Normalize only the working directory; the configured command path is
	// resolved by the executor environment exactly as provided.
	dir := manifest.Paths.WorkingDir
	if !filepath.IsAbs(dir) {
		if absDir, err := filepath.Abs(dir); err == nil {
			dir = absDir
		}
	}
	command := notifications.Command
	command.Args = append([]string(nil), command.Args...)
	command.Env = append([]string(nil), command.Env...)

	var states map[string]struct{}
	if len(notifications.States) > 0 {
		states = make(map[string]struct{}, len(notifications.States))
		for _, state := range notifications.States {
			states[state] = struct{}{}
		}
	}

	return &commandNotifier{
		command: command,
		states:  states,
		dir:     dir,
		logger:  logger,
		output:  output,
	}
}

func (n *commandNotifier) Notify(ctx context.Context, state string, message string, values map[string]string) {
	if n == nil || !n.enabled(state) {
		return
	}
	renderer, err := manifest.NewTemplateRenderer(manifest.NotificationTemplateProvider{
		State:   state,
		Message: message,
		Values:  values,
	})
	if err != nil {
		if n.logger != nil {
			n.logger.Warn("notification hook template failed", "state", state, "err", err)
		}
		return
	}
	command, err := manifest.RenderCommand(n.command, renderer)
	if err != nil {
		if n.logger != nil {
			n.logger.Warn("notification hook template failed", "state", state, "err", err)
		}
		return
	}
	env, err := notificationEnv(state, message, values)
	if err != nil {
		if n.logger != nil {
			n.logger.Warn("notification hook template failed", "state", state, "err", err)
		}
		return
	}
	env = append(env, command.Env...)
	cmd := exec.CommandContext(ctx, command.Path, command.Args...)
	cmd.Env = executor.WrapEnv(env)
	cmd.Dir = n.dir
	cmd.Stdout = n.output
	cmd.Stderr = n.output
	if err := cmd.Run(); err != nil && n.logger != nil {
		n.logger.Warn("notification hook failed", "state", state, "err", err)
	}
}

func (n *commandNotifier) enabled(state string) bool {
	if len(n.states) == 0 {
		return true
	}
	_, ok := n.states[state]
	return ok
}

func notificationEnv(state string, message string, values map[string]string) ([]string, error) {
	env := []string{
		"VIRTLE_NOTIFY_STATE=" + state,
		"VIRTLE_NOTIFY_MESSAGE=" + message,
	}

	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		envKey, err := executor.EnvName(key)
		if err != nil {
			return nil, err
		}
		env = append(env, fmt.Sprintf("VIRTLE_NOTIFY_CONTEXT_%s=%s", envKey, values[key]))
	}
	return env, nil
}
