package manager

import (
	"errors"
	"fmt"
	"os"
	"syscall"

	"github.com/shazow/virtle/internal/manager/launch"
)

var _ launch.PIDSignaler = syscallPIDSignaler{}

type syscallPIDSignaler struct{}

func (syscallPIDSignaler) Exists(pid int) error {
	err := syscall.Kill(pid, 0)
	if errors.Is(err, syscall.EPERM) {
		return nil
	}
	return err
}

func (syscallPIDSignaler) Signal(pid int, sig os.Signal) error {
	number, ok := sig.(syscall.Signal)
	if !ok {
		return fmt.Errorf("unsupported signal %v", sig)
	}
	return syscall.Kill(pid, number)
}
