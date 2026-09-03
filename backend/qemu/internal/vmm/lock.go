package vmm

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"syscall"

	"github.com/shazow/virtle/backend/qemu/internal/launch"
)

type fileLocker struct{}

func (l *fileLocker) Acquire(path string) (launch.Lock, error) {
	if err := launch.EnsurePrivateDirectory(filepath.Dir(path)); err != nil {
		return nil, fmt.Errorf("create lock directory for %q: %w", path, err)
	}

	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("acquire lock %q: %w", path, err)
	}

	if err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		file.Close()
		return nil, fmt.Errorf("acquire lock %q: %w", path, err)
	}

	if err := file.Truncate(0); err != nil {
		file.Close()
		return nil, fmt.Errorf("reset lock %q: %w", path, err)
	}

	if _, err := file.WriteString(strconv.Itoa(os.Getpid()) + "\n"); err != nil {
		file.Close()
		return nil, fmt.Errorf("write lock %q: %w", path, err)
	}

	return &fileLock{path: path, file: file}, nil
}

type fileLock struct {
	path string
	file *os.File
}

func (l *fileLock) Release() error {
	if l.file == nil {
		return nil
	}

	if err := l.file.Close(); err != nil && !errors.Is(err, os.ErrClosed) {
		return fmt.Errorf("close lock %q: %w", l.path, err)
	}
	l.file = nil

	return nil
}
