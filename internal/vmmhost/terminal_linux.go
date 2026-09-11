package vmmhost

import (
	"errors"
	"fmt"
	"os"
	"syscall"
	"unsafe"
)

// openTerminal opens a pseudo-terminal pair for a VMM that reads console
// input only from a terminal. The master is non-blocking, so Go's poller
// drives it and Close unblocks a reader; the slave is blocking, as a child's
// standard streams should be, and raw, so no echo or newline translation
// happens in the line discipline before the VMM configures it itself.
func openTerminal() (master, slave *os.File, err error) {
	// The devpts instance's own ptmx first: in a sandbox with a private
	// instance, /dev/ptmx may be the host's, whose slaves live elsewhere.
	fd, err := syscall.Open("/dev/pts/ptmx", syscall.O_RDWR|syscall.O_NOCTTY|syscall.O_CLOEXEC|syscall.O_NONBLOCK, 0)
	if err != nil {
		fd, err = syscall.Open("/dev/ptmx", syscall.O_RDWR|syscall.O_NOCTTY|syscall.O_CLOEXEC|syscall.O_NONBLOCK, 0)
	}
	if err != nil {
		return nil, nil, fmt.Errorf("open pseudo-terminal: %w", err)
	}
	master = os.NewFile(uintptr(fd), "ptmx")
	var unlock int32
	if err := ioctl(fd, syscall.TIOCSPTLCK, unsafe.Pointer(&unlock)); err != nil {
		_ = master.Close()
		return nil, nil, fmt.Errorf("unlock pseudo-terminal: %w", err)
	}
	var number uint32
	if err := ioctl(fd, syscall.TIOCGPTN, unsafe.Pointer(&number)); err != nil {
		_ = master.Close()
		return nil, nil, fmt.Errorf("name pseudo-terminal: %w", err)
	}
	path := fmt.Sprintf("/dev/pts/%d", number)
	sfd, err := syscall.Open(path, syscall.O_RDWR|syscall.O_NOCTTY|syscall.O_CLOEXEC, 0)
	if err != nil {
		_ = master.Close()
		return nil, nil, fmt.Errorf("open %s: %w", path, err)
	}
	slave = os.NewFile(uintptr(sfd), path)
	if err := makeRaw(sfd); err != nil {
		_ = slave.Close()
		_ = master.Close()
		return nil, nil, fmt.Errorf("raw mode on %s: %w", path, err)
	}
	return master, slave, nil
}

// makeRaw is cfmakeraw(3): no input or output translation, no echo, no
// canonical line editing, no signal characters, 8-bit characters.
func makeRaw(fd int) error {
	var t syscall.Termios
	if err := ioctl(fd, syscall.TCGETS, unsafe.Pointer(&t)); err != nil {
		return err
	}
	t.Iflag &^= syscall.IGNBRK | syscall.BRKINT | syscall.PARMRK | syscall.ISTRIP | syscall.INLCR | syscall.IGNCR | syscall.ICRNL | syscall.IXON
	t.Oflag &^= syscall.OPOST
	t.Lflag &^= syscall.ECHO | syscall.ECHONL | syscall.ICANON | syscall.ISIG | syscall.IEXTEN
	t.Cflag &^= syscall.CSIZE | syscall.PARENB
	t.Cflag |= syscall.CS8
	t.Cc[syscall.VMIN] = 1
	t.Cc[syscall.VTIME] = 0
	return ioctl(fd, syscall.TCSETS, unsafe.Pointer(&t))
}

func ioctl(fd int, request uintptr, arg unsafe.Pointer) error {
	if _, _, errno := syscall.Syscall(syscall.SYS_IOCTL, uintptr(fd), request, uintptr(arg)); errno != 0 {
		return errno
	}
	return nil
}

// terminalClosed reports the read error a pseudo-terminal master returns
// once every descriptor of its slave is closed: the VMM has exited.
func terminalClosed(err error) bool {
	return errors.Is(err, syscall.EIO)
}
