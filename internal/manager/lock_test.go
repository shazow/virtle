package manager

import (
	"path/filepath"
	"testing"
)

func TestFileLockerRejectsConcurrentAcquire(t *testing.T) {
	locker := &fileLocker{}
	lockPath := filepath.Join(t.TempDir(), "virtie.lock")

	lock, err := locker.Acquire(lockPath)
	if err != nil {
		t.Fatalf("acquire first lock: %v", err)
	}
	defer lock.Release()

	if _, err := locker.Acquire(lockPath); err == nil {
		t.Fatal("expected duplicate acquisition to fail")
	}
}

func TestFileLockerRecoversAfterUncleanClose(t *testing.T) {
	locker := &fileLocker{}
	lockPath := filepath.Join(t.TempDir(), "virtie.lock")

	lock, err := locker.Acquire(lockPath)
	if err != nil {
		t.Fatalf("acquire first lock: %v", err)
	}

	fileLock, ok := lock.(*fileLock)
	if !ok {
		t.Fatalf("unexpected lock type %T", lock)
	}

	if err := fileLock.file.Close(); err != nil {
		t.Fatalf("simulate crash close: %v", err)
	}
	fileLock.file = nil

	recovered, err := locker.Acquire(lockPath)
	if err != nil {
		t.Fatalf("acquire after unclean close: %v", err)
	}
	defer recovered.Release()
}
