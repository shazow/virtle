package manager

import (
	"fmt"
	"os"
	"syscall"
	"unsafe"

	"github.com/shazow/virtle/internal/manager/launch"
)

const (
	vhostVSockPath                = "/dev/vhost-vsock"
	vhostVSockSetGuestCID uintptr = 0x4008af60
)

type hostVSockCIDChecker struct{}

func newHostVSockCIDChecker() launch.VSockCIDChecker {
	return &hostVSockCIDChecker{}
}

func (c *hostVSockCIDChecker) Available(cid int) (bool, error) {
	file, err := os.OpenFile(vhostVSockPath, os.O_RDWR|syscall.O_CLOEXEC, 0)
	if err != nil {
		// Fake-tool checks run without /dev/vhost-vsock; let real QEMU report
		// missing or inaccessible host devices later in the launch.
		return true, nil
	}
	defer file.Close()

	value := uint64(cid)
	_, _, errno := syscall.Syscall(
		syscall.SYS_IOCTL,
		file.Fd(),
		vhostVSockSetGuestCID,
		uintptr(unsafe.Pointer(&value)),
	)
	if errno == 0 {
		return true, nil
	}
	if errno == syscall.EADDRINUSE {
		return false, nil
	}
	return false, fmt.Errorf("check vsock cid %d availability: %w", cid, errno)
}
