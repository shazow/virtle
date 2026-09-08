package firecracker

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"golang.org/x/sys/unix"
)

func runWrapperHelper(mode string) {
	if mode == "descendant" {
		signals := make(chan os.Signal, 1)
		signal.Notify(signals, syscall.SIGTERM)
		fmt.Fprintln(os.Stdout, os.Getpid(), syscall.Getpgrp())
		// The wrapper exits only after its descendant is ready. Close output
		// explicitly so inherited descriptors cannot delay the wrapper's Wait.
		_ = os.Stdout.Close()
		_ = os.Stderr.Close()
		ready := os.NewFile(3, "ready")
		_, _ = ready.Write([]byte{1})
		_ = ready.Close()
		<-signals
		return
	}
	read, write, err := os.Pipe()
	if err != nil {
		panic(err)
	}
	cmd := exec.Command(os.Args[0])
	cmd.Env = append(os.Environ(), "VIRTLE_TEST_FIRECRACKER=descendant")
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	cmd.ExtraFiles = []*os.File{write}
	if err := cmd.Start(); err != nil {
		panic(err)
	}
	_ = write.Close()
	if _, err := io.ReadFull(read, make([]byte, 1)); err != nil {
		panic(err)
	}
	_ = read.Close()
}

func TestStartupRollbackReapsWrapperDescendant(t *testing.T) {
	if os.Getenv("VIRTLE_TEST_SUBREAPER") != "1" {
		// Isolate subreaper status from other tests. It lets this test reap the
		// orphan itself, without relying on init's reaping schedule.
		ctx, cancel := context.WithTimeout(t.Context(), 3*testTimeout)
		defer cancel()
		cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestStartupRollbackReapsWrapperDescendant$", "-test.v")
		cmd.Env = append(os.Environ(), "VIRTLE_TEST_SUBREAPER=1")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("rollback supervisor: %v\n%s", err, out)
		}
		return
	}
	if err := unix.Prctl(unix.PR_SET_CHILD_SUBREAPER, 1, 0, 0, 0); err != nil {
		t.Fatal(err)
	}
	b, spec := helperBackend(t, "wrapper")
	read, write := io.Pipe()
	defer read.Close()
	defer write.Close()
	b.Console, b.ConsoleOutput = "print", write
	result := make(chan error, 1)
	go func() { _, err := b.Start(t.Context(), spec); result <- err }()
	var pid, pgid int
	if _, err := fmt.Fscan(read, &pid, &pgid); err != nil {
		t.Fatal(err)
	}
	reaped := false
	defer func() {
		if reaped {
			return
		}
		// The descendant has not been reaped, so its PID is still owned.
		_ = syscall.Kill(pid, syscall.SIGKILL)
		var status syscall.WaitStatus
		_, _ = syscall.Wait4(pid, &status, 0, nil)
	}()
	var exitErr *exec.ExitError
	if err := <-result; !errors.As(err, &exitErr) || exitErr.ExitCode() != 17 {
		t.Fatalf("wrapper exit status: %v", err)
	}
	var info unix.Siginfo
	if err := unix.Waitid(unix.P_PID, pid, &info, unix.WEXITED|unix.WNOWAIT|unix.WNOHANG, nil); err != nil {
		t.Fatal(err)
	}
	if info.Signo == 0 {
		t.Fatalf("startup rollback left descendant %d alive in wrapper process group %d", pid, pgid)
	}
	var status syscall.WaitStatus
	if _, err := syscall.Wait4(pid, &status, 0, nil); err != nil {
		t.Fatal(err)
	}
	reaped = true
	if !status.Signaled() || status.Signal() != syscall.SIGKILL {
		t.Fatalf("descendant exit status: %v", status)
	}
	if err := syscall.Kill(-pgid, 0); !errors.Is(err, syscall.ESRCH) {
		t.Fatalf("wrapper process group remains: %v", err)
	}
	// Ownership is available again only after the descendant has terminated.
	lock, err := lockState(filepath.Join(spec.Dir, ".virtle"), "virtle")
	if err != nil {
		t.Fatal(err)
	}
	_ = lock.Close()
}

func TestStateDirectorySymlinkForms(t *testing.T) {
	for _, suffix := range []string{"", "/", "///"} {
		t.Run(fmt.Sprintf("suffix=%q", suffix), func(t *testing.T) {
			target := t.TempDir()
			link := filepath.Join(t.TempDir(), "state")
			if err := os.Symlink(target, link); err != nil {
				t.Fatal(err)
			}
			lock, err := lockState(link+suffix, "virtle")
			if lock != nil {
				_ = lock.Close()
			}
			if err == nil || !strings.Contains(err.Error(), "private directory") {
				t.Errorf("state symlink %q: %v", link+suffix, err)
			}
			entries, err := os.ReadDir(target)
			if err != nil || len(entries) != 0 {
				t.Fatalf("symlink target changed: %v %v", entries, err)
			}
		})
	}
}

func TestStateDirectoryPathForms(t *testing.T) {
	for _, suffix := range []string{"", "/", "///"} {
		t.Run(fmt.Sprintf("suffix=%q", suffix), func(t *testing.T) {
			dir := filepath.Join(t.TempDir(), "state")
			lock, err := lockState(dir+suffix, "virtle")
			if err != nil {
				t.Fatal(err)
			}
			_ = lock.Close()
			info, err := os.Stat(dir)
			if err != nil || info.Mode().Perm() != 0700 {
				t.Fatalf("private state directory: %v %v", info, err)
			}
			if err := os.Chmod(dir, 0755); err != nil {
				t.Fatal(err)
			}
			if lock, err := lockState(dir+suffix, "virtle"); err == nil {
				_ = lock.Close()
				t.Fatal("non-private directory accepted")
			}
		})
	}
}
