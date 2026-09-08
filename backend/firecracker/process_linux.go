package firecracker

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/shazow/virtle/internal/executor"
	"golang.org/x/sys/unix"
)

const processGroupRetryInterval = 10 * time.Millisecond

// ownedProcess keeps the group leader waitable until its entire group stops.
// Reaping the leader earlier would allow its PID/PGID to be reused, making a
// later group kill unsafe. All group signals are serialized with retirement.
type ownedProcess struct {
	cmd     *exec.Cmd
	mu      sync.Mutex
	retired bool
}

func startProcess(cmd *exec.Cmd) (*executor.Process, error) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start %s: %w", executor.CommandName(cmd), err)
	}
	return executor.Wrap(&ownedProcess{cmd: cmd}), nil
}

func (p *ownedProcess) PID() int     { return p.cmd.Process.Pid }
func (p *ownedProcess) Name() string { return executor.CommandName(p.cmd) }
func (p *ownedProcess) Kill() error  { return p.Signal(syscall.SIGKILL) }

func (p *ownedProcess) Signal(sig os.Signal) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.retired {
		return os.ErrProcessDone
	}
	s, ok := sig.(syscall.Signal)
	if !ok {
		return fmt.Errorf("unsupported firecracker signal %v", sig)
	}
	err := syscall.Kill(-p.PID(), s)
	if errors.Is(err, syscall.ESRCH) {
		// An absent group is terminal. Never retry this PGID or fall back to
		// signaling the positive PID, even if a subsequent caller asks to kill.
		p.retired = true
		return os.ErrProcessDone
	}
	return err
}

func (p *ownedProcess) Wait() error {
	var info unix.Siginfo
	var err error
	for {
		err = unix.Waitid(unix.P_PID, p.PID(), &info, unix.WEXITED|unix.WNOWAIT, nil)
		if !errors.Is(err, syscall.EINTR) {
			break
		}
	}
	if err == nil {
		killErr := p.Kill()
		if !errors.Is(killErr, os.ErrProcessDone) {
			err = killErr
		}
		// SIGKILL delivery is asynchronous. Non-child descendants cannot be
		// waited on with waitid; observe their state through procfs instead.
		// Keep the leader and runtime ownership even if a thread is stuck or
		// procfs is temporarily unreadable. Stop bounds its caller's wait; this
		// background waiter completes cleanup when termination is confirmed.
		ticker := time.NewTicker(processGroupRetryInterval)
		defer ticker.Stop()
		for {
			alive, scanErr := processGroupAlive(os.DirFS("/proc"), p.PID())
			if scanErr == nil && !alive {
				break
			}
			<-ticker.C
		}
	}
	p.mu.Lock()
	p.retired = true
	p.mu.Unlock()
	// No group signal may follow this wait, including concurrent Stop calls.
	return errors.Join(err, p.cmd.Wait())
}

func processGroupAlive(proc fs.FS, pgid int) (bool, error) {
	entries, err := fs.ReadDir(proc, ".")
	if err != nil {
		return false, err
	}
	for _, entry := range entries {
		if _, err := strconv.Atoi(entry.Name()); err != nil {
			continue
		}
		data, err := fs.ReadFile(proc, entry.Name()+"/stat")
		if errors.Is(err, fs.ErrNotExist) {
			continue // this process was reaped during the scan
		}
		if err != nil {
			return false, err
		}
		group, alive, err := processStat(data)
		if err != nil {
			return false, err
		}
		if group == pgid && alive {
			return true, nil
		}
	}
	return false, nil
}

func processStat(data []byte) (pgid int, alive bool, err error) {
	// comm is parenthesized and may itself contain spaces and parentheses.
	end := strings.LastIndexByte(string(data), ')')
	if end < 0 {
		return 0, false, errors.New("invalid process stat")
	}
	fields := strings.Fields(string(data[end+1:]))
	if len(fields) < 18 {
		return 0, false, errors.New("invalid process stat")
	}
	pgid, err = strconv.Atoi(fields[2])
	if err != nil {
		return 0, false, err
	}
	threads, err := strconv.Atoi(fields[17])
	// A zombie leader is retained while sibling threads finish exiting.
	// num_threads (field 20) includes that leader until the final reap.
	return pgid, (fields[0] != "Z" && fields[0] != "X") || threads > 1, err
}
