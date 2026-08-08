package executor

import (
	"context"
	"errors"
	"fmt"
	"os"
	"syscall"
	"time"
)

// DefaultGracePeriod paces stop escalation for processes that do not
// configure their own grace period.
const DefaultGracePeriod = 15 * time.Second

// Process tracks a process lifecycle and caches its wait result.
type Process struct {
	handle RunningProcess
	done   chan struct{}

	shutdown    func() error
	gracePeriod time.Duration
	waitErr     error
}

// Wrap starts tracking completion for an already-running process handle.
func Wrap(handle RunningProcess) *Process {
	p := &Process{
		handle: handle,
		done:   make(chan struct{}),
	}
	go func() {
		if handle != nil {
			p.waitErr = handle.Wait()
		}
		close(p.done)
	}()
	return p
}

func (p *Process) Wait() error {
	if p == nil {
		return nil
	}
	<-p.done
	return p.waitErr
}

// WaitContext blocks until the process exits or ctx ends. It returns the
// process's wait result, or context.Cause(ctx) if ctx ended first.
func (p *Process) WaitContext(ctx context.Context) error {
	if p == nil {
		return nil
	}
	select {
	case <-p.done:
		return p.waitErr
	case <-ctx.Done():
		return context.Cause(ctx)
	}
}

func (p *Process) Signal(sig os.Signal) error {
	if p == nil || p.handle == nil {
		return nil
	}
	return p.handle.Signal(sig)
}

func (p *Process) Kill() error {
	if p == nil || p.handle == nil {
		return nil
	}
	return p.handle.Kill()
}

func (p *Process) PID() int {
	if p == nil || p.handle == nil {
		return 0
	}
	return p.handle.PID()
}

func (p *Process) Name() string {
	if p == nil || p.handle == nil {
		return ""
	}
	return p.handle.Name()
}

// Done returns a channel closed when the process exits.
func (p *Process) Done() <-chan struct{} {
	if p == nil {
		return closedDone()
	}
	return p.done
}

// PollExit reports whether the process has exited without blocking.
func (p *Process) PollExit() (bool, error) {
	if p == nil {
		return true, nil
	}
	select {
	case <-p.done:
		return true, p.waitErr
	default:
		return false, nil
	}
}

// SetShutdown installs a graceful shutdown callback tried before SIGTERM.
// It must be called before Stop may run concurrently; the field is not
// synchronized.
func (p *Process) SetShutdown(shutdown func() error) {
	if p == nil {
		return
	}
	p.shutdown = shutdown
}

// SetGracePeriod overrides how long each Stop escalation rung waits before
// escalating. Zero keeps DefaultGracePeriod. Like SetShutdown, it must be
// called before Stop may run concurrently; the field is not synchronized.
func (p *Process) SetGracePeriod(gracePeriod time.Duration) {
	if p == nil {
		return
	}
	p.gracePeriod = gracePeriod
}

func (p *Process) getGracePeriod() time.Duration {
	if p.gracePeriod > 0 {
		return p.gracePeriod
	}
	return DefaultGracePeriod
}

// minKillWait bounds how long Stop waits for a SIGKILLed process to be
// reaped before giving up on it.
const minKillWait = time.Second

// Stop gracefully stops the process, escalating shutdown → SIGTERM → SIGKILL
// with the grace period between rungs. When ctx ends — canceled or past its
// deadline — the remaining graceful rungs are abandoned and the process is
// killed; Stop never returns with the process left running.
func (p *Process) Stop(ctx context.Context) error {
	if p == nil {
		return nil
	}
	if exited, _ := p.PollExit(); exited {
		return nil
	}

	gracePeriod := p.getGracePeriod()
	var shutdownErr error
	if p.shutdown != nil && ctx.Err() == nil {
		if err := p.shutdown(); err != nil {
			shutdownErr = fmt.Errorf("shutdown %s: %w", p.Name(), err)
		} else if p.waitGrace(ctx, gracePeriod) {
			return nil
		}
	}

	if ctx.Err() == nil {
		if err := p.Signal(syscall.SIGTERM); err != nil && !errors.Is(err, os.ErrProcessDone) {
			if shutdownErr != nil {
				return errors.Join(shutdownErr, fmt.Errorf("stop %s: %w", p.Name(), err))
			}
			return fmt.Errorf("stop %s: %w", p.Name(), err)
		}
		if p.waitGrace(ctx, gracePeriod) {
			return shutdownErr
		}
	}

	if err := p.Kill(); err != nil && !errors.Is(err, os.ErrProcessDone) {
		if shutdownErr != nil {
			return errors.Join(shutdownErr, fmt.Errorf("kill %s: %w", p.Name(), err))
		}
		return fmt.Errorf("kill %s: %w", p.Name(), err)
	}
	// Bound the post-kill wait: a process wedged in uninterruptible sleep must
	// not hang teardown forever. The background Wait goroutine still reaps it
	// whenever it eventually exits. A floor keeps zero or tiny grace periods
	// from racing the asynchronous reaper of a process that did die from
	// SIGKILL. This wait deliberately ignores ctx: the kill is already sent,
	// and reporting failure before the reaper confirms exit would misattribute
	// a process that died moments later.
	killWait := gracePeriod
	if killWait < minKillWait {
		killWait = minKillWait
	}
	if !p.waitForExit(killWait) {
		err := fmt.Errorf("kill %s: process did not exit", p.Name())
		if shutdownErr != nil {
			return errors.Join(shutdownErr, err)
		}
		return err
	}
	return shutdownErr
}

// KillAndWait kills the process and waits for it to exit.
func (p *Process) KillAndWait() error {
	if p == nil {
		return nil
	}
	if exited, _ := p.PollExit(); exited {
		return nil
	}
	if err := p.Kill(); err != nil && !errors.Is(err, os.ErrProcessDone) {
		return fmt.Errorf("kill %s: %w", p.Name(), err)
	}
	_ = p.Wait()
	return nil
}

// SignalProcessGroup signals pid's process group, falling back to pid itself.
func SignalProcessGroup(pid int, sig syscall.Signal) error {
	if pid <= 0 {
		return nil
	}
	if err := syscall.Kill(-pid, sig); err == nil || errors.Is(err, syscall.ESRCH) {
		return nil
	}
	if err := syscall.Kill(pid, sig); err != nil && !errors.Is(err, syscall.ESRCH) {
		return fmt.Errorf("signal pid %d: %w", pid, err)
	}
	return nil
}

// waitGrace waits up to gracePeriod for the process to exit; ctx ending cuts
// the wait short so Stop can escalate immediately. gracePeriod is always
// positive: getGracePeriod floors it and the post-kill wait floors at
// minKillWait.
func (p *Process) waitGrace(ctx context.Context, gracePeriod time.Duration) bool {
	timer := time.NewTimer(gracePeriod)
	defer timer.Stop()
	select {
	case <-p.done:
		return true
	case <-timer.C:
		return false
	case <-ctx.Done():
		return false
	}
}

// waitForExit waits up to delay for the process to exit, deliberately
// ignoring any caller context (see Stop's post-kill comment): Background's
// nil Done channel means that select arm never fires.
func (p *Process) waitForExit(delay time.Duration) bool {
	return p.waitGrace(context.Background(), delay)
}

func closedDone() <-chan struct{} {
	done := make(chan struct{})
	close(done)
	return done
}
