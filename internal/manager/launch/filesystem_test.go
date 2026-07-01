package launch

import (
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

func TestCheckSocketPathRejectsNonSocket(t *testing.T) {
	tmpDir := t.TempDir()
	path := filepath.Join(tmpDir, "cleanup.sock")
	if err := os.WriteFile(path, []byte("cleanup"), 0o600); err != nil {
		t.Fatalf("write cleanup file: %v", err)
	}

	err := CheckSocketPath(path)
	if err == nil || !strings.Contains(err.Error(), "path is not a socket") {
		t.Fatalf("expected non-socket rejection, got %v", err)
	}
	if errors.Is(err, ErrStaleSocket) {
		t.Fatal("regular file should not be stale")
	}
	if _, statErr := os.Stat(path); statErr != nil {
		t.Fatalf("non-socket path should remain: %v", statErr)
	}
}

func TestRemoveStaleSocketsRemovesSocketsAndIgnoresMissing(t *testing.T) {
	tmpDir := t.TempDir()
	socketPath := filepath.Join(tmpDir, "cleanup.sock")
	missingPath := filepath.Join(tmpDir, "missing.sock")
	createStaleUnixSocketForTest(t, socketPath)

	if err := RemoveStaleSockets(socketPath, missingPath); err != nil {
		t.Fatalf("remove socket paths: %v", err)
	}
	if _, err := os.Stat(socketPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("expected socket to be removed, stat err: %v", err)
	}
}

func TestRemoveStaleSocketsPreservesLiveSocket(t *testing.T) {
	tmpDir := t.TempDir()
	socketPath := filepath.Join(tmpDir, "live.sock")
	listenUnixSocketForTest(t, socketPath)

	err := RemoveStaleSockets(socketPath)
	if err == nil || !strings.Contains(err.Error(), "is still live") {
		t.Fatalf("expected live socket rejection, got %v", err)
	}
	if _, statErr := os.Stat(socketPath); statErr != nil {
		t.Fatalf("live socket path should remain: %v", statErr)
	}
}

func TestCheckSocketPathReportsMissingPathClear(t *testing.T) {
	tmpDir := t.TempDir()
	if err := CheckSocketPath(filepath.Join(tmpDir, "missing.sock")); err != nil {
		t.Fatalf("check missing socket: %v", err)
	}
}

func TestCheckSocketPathReportsStaleSocket(t *testing.T) {
	tmpDir := t.TempDir()
	socketPath := filepath.Join(tmpDir, "cleanup.sock")
	createStaleUnixSocketForTest(t, socketPath)

	err := CheckSocketPath(socketPath)
	if !errors.Is(err, ErrStaleSocket) {
		t.Fatalf("check stale socket: %v", err)
	}
}

func TestCheckSocketPathRejectsLiveSocket(t *testing.T) {
	tmpDir := t.TempDir()
	socketPath := filepath.Join(tmpDir, "live.sock")
	listenUnixSocketForTest(t, socketPath)

	err := CheckSocketPath(socketPath)
	if err == nil || !strings.Contains(err.Error(), "is still live") {
		t.Fatalf("expected live socket rejection, got %v", err)
	}
	if errors.Is(err, ErrStaleSocket) {
		t.Fatal("live socket should not be stale")
	}
}

func listenUnixSocketForTest(t *testing.T, path string) {
	t.Helper()
	listener, err := net.Listen("unix", path)
	if err != nil {
		t.Fatalf("listen on unix socket: %v", err)
	}
	t.Cleanup(func() {
		_ = listener.Close()
	})
}

func createStaleUnixSocketForTest(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("create socket parent: %v", err)
	}
	fd, err := syscall.Socket(syscall.AF_UNIX, syscall.SOCK_STREAM, 0)
	if err != nil {
		t.Fatalf("create unix socket: %v", err)
	}
	if err := syscall.Bind(fd, &syscall.SockaddrUnix{Name: path}); err != nil {
		_ = syscall.Close(fd)
		t.Fatalf("bind stale unix socket %q: %v", path, err)
	}
	if err := syscall.Close(fd); err != nil {
		t.Fatalf("close stale unix socket %q: %v", path, err)
	}
}
