package launch

import (
	"context"
	"os"
	"os/exec"

	"github.com/shazow/virtle/internal/executor"
)

type Lock interface {
	Release() error
}

type Locker interface {
	Acquire(path string) (Lock, error)
}

type VSockCIDChecker interface {
	Available(cid int) (bool, error)
}

type Runner interface {
	Start(cmd *exec.Cmd) (*executor.Process, error)
}

type SocketWaiter interface {
	Wait(ctx context.Context, socketPaths []string) error
}

type PIDSignaler interface {
	Exists(pid int) error
	Signal(pid int, sig os.Signal) error
}
