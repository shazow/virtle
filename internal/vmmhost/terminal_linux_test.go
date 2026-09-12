package vmmhost

import (
	"errors"
	"io"
	"os"
	"strings"
	"syscall"
	"testing"
	"time"
	"unsafe"
)

// The pair a VMM gets is raw in both directions and ends the master's reads
// once the slave is gone, which is how the reaper learns the console is over.
func TestOpenTerminal(t *testing.T) {
	master, slave, err := openTerminal()
	if err != nil {
		t.Fatal(err)
	}
	defer master.Close()
	if !strings.HasPrefix(slave.Name(), "/dev/pts/") {
		t.Fatalf("slave = %s, want a /dev/pts entry", slave.Name())
	}
	var termios syscall.Termios
	if _, _, errno := syscall.Syscall(syscall.SYS_IOCTL, slave.Fd(), syscall.TCGETS, uintptr(unsafe.Pointer(&termios))); errno != 0 {
		t.Fatal(errno)
	}
	if termios.Lflag&(syscall.ECHO|syscall.ICANON|syscall.ISIG) != 0 || termios.Oflag&syscall.OPOST != 0 || termios.Iflag&syscall.ICRNL != 0 {
		t.Fatalf("slave is not raw: %+v", termios)
	}
	// Bytes cross unchanged: no echo comes back and no newline is rewritten.
	if _, err := master.WriteString("in\n"); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 16)
	n, err := slave.Read(buf)
	if err != nil || string(buf[:n]) != "in\n" {
		t.Fatalf("slave read %q, %v", buf[:n], err)
	}
	if _, err := slave.WriteString("out\n"); err != nil {
		t.Fatal(err)
	}
	_ = master.SetReadDeadline(time.Now().Add(5 * time.Second))
	n, err = master.Read(buf)
	if err != nil || string(buf[:n]) != "out\n" {
		t.Fatalf("master read %q, %v", buf[:n], err)
	}
	if err := slave.Close(); err != nil {
		t.Fatal(err)
	}
	_, err = io.ReadAll(master)
	if !terminalClosed(err) && !errors.Is(err, os.ErrDeadlineExceeded) {
		t.Fatalf("master after the slave closed: %v, want EIO", err)
	}
	if errors.Is(err, os.ErrDeadlineExceeded) {
		t.Fatal("master read did not end when the slave closed")
	}
}
