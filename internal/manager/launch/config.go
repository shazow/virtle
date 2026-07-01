package launch

import (
	"context"
	"io"
	"os"
	"os/exec"
	"time"

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

type SSHReadyDialer interface {
	Dial(ctx context.Context, socketPath string, timeout time.Duration) (io.ReadCloser, error)
}

type PIDSignaler interface {
	Exists(pid int) error
	Signal(pid int, sig os.Signal) error
}
