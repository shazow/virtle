package vmmhost

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"syscall"
	"time"

	"github.com/shazow/virtle/internal/executor"
	imanifest "github.com/shazow/virtle/internal/manifest"
)

const (
	// helperGracePeriod is how long a helper gets to leave on SIGTERM before
	// it is killed; virtiofsd exits as soon as the VMM disconnects.
	helperGracePeriod  = 500 * time.Millisecond
	socketPollInterval = 10 * time.Millisecond
)

// Helpers are the manifest's host-side processes: its [[run]] entries and
// the share daemons a backend resolves into them. They start before the VMM
// in process groups of their own, as on QEMU, and stop after it has exited,
// while the state lock is still held.
type Helpers struct {
	group   executor.Group
	stderr  []*DiagnosticWriter // parallel to group.Processes()
	cleanup []string            // socket paths the helpers leave behind
}

// StartHelpers starts the manifest's runs, keeping each one's stderr tail
// for the diagnosis of a helper that dies before it is of use. The sockets
// resolution recorded as cleanup files are removed first: it judged them
// missing or dead and the helpers' to bind, and one left by a crashed launch
// would satisfy WaitSockets before its helper rebinds it.
func StartHelpers(mf *imanifest.Manifest, logger *slog.Logger) (*Helpers, error) {
	runs, err := mf.ResolvedRuns(0)
	if err != nil {
		return nil, err
	}
	h := &Helpers{}
	if h.cleanup, err = mf.ResolvedCleanupFiles(); err != nil {
		return nil, err
	}
	for _, path := range h.cleanup {
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("remove stale socket %q: %w", path, err)
		}
	}
	for _, run := range runs {
		cmd := executor.Command(run.Exec[0], run.Exec[1:], run.Env)
		cmd.Dir = run.Dir
		cmd.WaitDelay = time.Second // a child holding the pipes cannot hold teardown open
		cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
		stderr := &DiagnosticWriter{}
		cmd.Stdout, cmd.Stderr = io.Discard, stderr
		logger.Info("starting helper", "exec", run.Exec)
		process, err := (&executor.Runner{Logger: logger}).Start(cmd)
		if err != nil {
			_ = h.Stop()
			return nil, err
		}
		process.SetGracePeriod(helperGracePeriod)
		h.group.Add(process)
		h.stderr = append(h.stderr, stderr)
	}
	return h, nil
}

// WaitSockets returns once every path exists as a socket. It never connects:
// virtiofsd serves exactly one vhost-user connection, and a probe would take
// it from the VMM. The context ending (the VMM's exit, or the startup
// deadline) ends the wait before a helper's exit is consulted, so the
// cleanup that exit triggers is not taken for the cause; a helper that dies
// on its own ends it with its stderr.
func (h *Helpers) WaitSockets(ctx context.Context, paths []string) error {
	if len(paths) == 0 {
		return nil
	}
	ticker := time.NewTicker(socketPollInterval)
	defer ticker.Stop()
	for {
		missing := ""
		for _, path := range paths {
			if info, err := os.Stat(path); err != nil || info.Mode().Type() != os.ModeSocket {
				missing = path
				break
			}
		}
		if missing == "" {
			return nil
		}
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("wait for socket %q: %w", missing, err)
		}
		if err := h.Exited(); err != nil {
			return fmt.Errorf("wait for socket %q: %w", missing, err)
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("wait for socket %q: %w", missing, ctx.Err())
		case <-ticker.C:
		}
	}
}

// Exited reports the first helper that has died, with its stderr, or nil
// while every helper runs.
func (h *Helpers) Exited() error {
	process, exitErr, ok := h.group.FirstExit()
	if !ok {
		return nil
	}
	err := fmt.Errorf("helper %s exited", process.Name())
	if exitErr != nil {
		err = fmt.Errorf("helper %s exited: %w", process.Name(), exitErr)
	}
	for i, candidate := range h.group.Processes() {
		if candidate == process {
			if text := h.stderr[i].String(); text != "" {
				err = fmt.Errorf("%w; stderr: %q", err, text)
			}
		}
	}
	return err
}

// Stop ends the helpers and removes the sockets they leave behind (a killed
// daemon cannot unlink its own). It runs after the VMM has exited, so a
// daemon that noticed the disconnect is already gone.
func (h *Helpers) Stop() error {
	ctx, cancel := context.WithTimeout(context.Background(), 2*helperGracePeriod+time.Second)
	defer cancel()
	err := h.group.StopAll(ctx)
	for _, path := range h.cleanup {
		if removeErr := os.Remove(path); removeErr != nil && !errors.Is(removeErr, os.ErrNotExist) {
			err = errors.Join(err, removeErr)
		}
	}
	return err
}
