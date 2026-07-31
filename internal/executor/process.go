package executor

import (
	"errors"
	"fmt"
	"os"
	"syscall"
	"time"
)

// Process tracks a process lifecycle and caches its wait result.
type Process struct {
	handle RunningProcess
	done   chan struct{}

	shutdown func() error
	waitErr  error
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

// minKillWait bounds how long Stop waits for a SIGKILLed process to be
// reaped before giving up on it.
const minKillWait = time.Second

// Stop gracefully stops the process, escalating to Kill after delay.
func (p *Process) Stop(delay time.Duration) error {
	if p == nil {
		return nil
	}
	if exited, _ := p.PollExit(); exited {
		return nil
	}

	var shutdownErr error
	if p.shutdown != nil {
		if err := p.shutdown(); err != nil {
			shutdownErr = fmt.Errorf("shutdown %s: %w", p.Name(), err)
		} else if p.waitForExit(delay) {
			return nil
		}
	}

	if err := p.Signal(syscall.SIGTERM); err != nil && !errors.Is(err, os.ErrProcessDone) {
		if shutdownErr != nil {
			return errors.Join(shutdownErr, fmt.Errorf("stop %s: %w", p.Name(), err))
		}
		return fmt.Errorf("stop %s: %w", p.Name(), err)
	}

	if p.waitForExit(delay) {
		return shutdownErr
	}
	if err := p.Kill(); err != nil && !errors.Is(err, os.ErrProcessDone) {
		if shutdownErr != nil {
			return errors.Join(shutdownErr, fmt.Errorf("kill %s: %w", p.Name(), err))
		}
		return fmt.Errorf("kill %s: %w", p.Name(), err)
	}
	// Bound the post-kill wait: a process wedged in uninterruptible sleep must
	// not hang teardown forever. The background Wait goroutine still reaps it
	// whenever it eventually exits. A floor keeps zero or tiny delays from
	// racing the asynchronous reaper of a process that did die from SIGKILL.
	killWait := delay
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

func (p *Process) waitForExit(delay time.Duration) bool {
	if delay <= 0 {
		select {
		case <-p.done:
			return true
		default:
			return false
		}
	}
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-p.done:
		return true
	case <-timer.C:
		return false
	}
}

func closedDone() <-chan struct{} {
	done := make(chan struct{})
	close(done)
	return done
}
