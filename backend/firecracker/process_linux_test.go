package firecracker

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"testing"
	"testing/fstest"
)

func TestProcessGroupTermination(t *testing.T) {
	stat := func(state string, group, threads int) *fstest.MapFile {
		return &fstest.MapFile{Data: []byte(fmt.Sprintf("123 (wrapper (child)) %s 1 %d%s %d\n", state, group, strings.Repeat(" 0", 14), threads))}
	}
	for _, tt := range []struct {
		name    string
		state   string
		threads int
		alive   bool
	}{
		{"running", "R", 1, true},
		{"sleeping", "S", 1, true},
		{"stuck", "D", 1, true},
		{"zombie", "Z", 1, false},
		{"dead", "X", 1, false},
		{"zombie with remaining threads", "Z", 2, true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			proc := fstest.MapFS{
				"10/stat": stat("Z", 10, 1),
				"11/stat": stat(tt.state, 10, tt.threads),
				"12/stat": stat("R", 12, 1),
			}
			alive, err := processGroupAlive(proc, 10)
			if err != nil || alive != tt.alive {
				t.Fatalf("group alive = %v, %v; want %v", alive, err, tt.alive)
			}
		})
	}
}

func TestOwnedProcessRetiresAbsentGroup(t *testing.T) {
	// This child shares the test's group, so -child.PID names no group. It
	// must survive both the ESRCH and every later signal on the retired handle.
	cmd := exec.Command(os.Args[0])
	cmd.Env = append(os.Environ(), "VIRTLE_TEST_FIRECRACKER=no-socket")
	ready := &eventWriter{ready: make(chan struct{})}
	cmd.Stdout = ready
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })
	<-ready.ready
	p := &ownedProcess{cmd: cmd}
	for _, sig := range []os.Signal{syscall.SIGKILL, syscall.SIGTERM} {
		if err := p.Signal(sig); !errors.Is(err, os.ErrProcessDone) {
			t.Fatalf("signal %v: %v", sig, err)
		}
	}
	if err := cmd.Process.Signal(syscall.Signal(0)); err != nil {
		t.Fatalf("signaled positive PID after missing group: %v", err)
	}
}
