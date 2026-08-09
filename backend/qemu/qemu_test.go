package qemu

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/shazow/virtle/backend"
	"github.com/shazow/virtle/internal/executor/executortest"
	"github.com/shazow/virtle/internal/qmpclient"
	"github.com/shazow/virtle/units"
	"github.com/shazow/virtle/vm"
)

func testSpec(t *testing.T) *vm.Spec {
	t.Helper()
	dir := t.TempDir()
	return &vm.Spec{
		Kernel: vm.Kernel{
			Path:   filepath.Join(dir, "vmlinuz"),
			Initrd: filepath.Join(dir, "initrd.img"),
		},
		Memory: 2048 * units.Mebibyte,
		CPUs:   2,
		Dir:    filepath.Join(dir, "state"),
	}
}

func TestStartRunsQEMUAndReachesTheMonitor(t *testing.T) {
	b, runner, monitor, _ := testBackend(Config{Binary: "qemu-system-x86_64"})
	spec := testSpec(t)

	inst, err := b.Start(context.Background(), spec)
	if err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	t.Cleanup(func() { inst.Kill() })

	starts := runner.Starts()
	if len(starts) != 1 {
		t.Fatalf("started %d processes, want 1 (qemu)", len(starts))
	}
	args := strings.Join(starts[0].Args, " ")
	for _, want := range []string{"-m 2048", "-smp 2", "-kernel " + spec.Kernel.Path} {
		if !strings.Contains(args, want) {
			t.Errorf("qemu args %q missing %q", args, want)
		}
	}
	if monitor.disconnected {
		t.Error("monitor was disconnected while the instance is running")
	}
}

func TestStartStartsAVirtiofsDaemonPerShare(t *testing.T) {
	b, runner, _, _ := testBackend(Config{})
	spec := testSpec(t)
	spec.Shares = []vm.Share{{Tag: "workspace", HostPath: t.TempDir(), GuestPath: "/workspace"}}

	inst, err := b.Start(context.Background(), spec)
	if err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	t.Cleanup(func() { inst.Kill() })

	names := runner.StartedNames()
	if !slices.Contains(names, "virtiofsd") {
		t.Fatalf("started %v, want a virtiofsd among them", names)
	}
	if names[len(names)-1] == "virtiofsd" {
		t.Error("virtiofsd started after qemu; it must be listening first")
	}

	starts := runner.Starts()
	index := slices.IndexFunc(starts, func(s executortest.Start) bool { return s.Name == "virtiofsd" })
	args := strings.Join(starts[index].Args, " ")
	if !strings.Contains(args, "--tag=workspace") {
		t.Errorf("virtiofsd args %q missing the share tag", args)
	}
	if !strings.Contains(args, "--shared-dir="+spec.Shares[0].HostPath) {
		t.Errorf("virtiofsd args %q missing the host path", args)
	}
}

func TestStartReleasesHelpersWhenQEMUFailsToStart(t *testing.T) {
	b, runner, _, _ := testBackend(Config{Binary: "qemu-system-x86_64"})
	runner.StartErrors = map[string]error{"qemu-system-x86_64": errors.New("no such binary")}
	spec := testSpec(t)
	spec.Shares = []vm.Share{{Tag: "workspace", HostPath: t.TempDir()}}

	inst, err := b.Start(context.Background(), spec)
	if err == nil {
		t.Fatal("Start() error = nil, want the runner's failure")
	}
	if inst != nil {
		t.Errorf("Start() returned instance %v alongside an error", inst)
	}
	if !slices.Contains(runner.SignalNames(), "virtiofsd") {
		t.Errorf("virtiofsd survived the failed launch; signaled %v", runner.SignalNames())
	}
}

func TestStartWritesSpecFilesIntoTheGuest(t *testing.T) {
	b, _, _, agent := testBackend(Config{})
	spec := testSpec(t)
	spec.Files = []vm.File{{
		GuestPath: "/etc/hello",
		Content:   strings.NewReader("hello from the host\n"),
		Mode:      0o600,
	}}

	inst, err := b.Start(context.Background(), spec)
	if err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	t.Cleanup(func() { inst.Kill() })

	content, ok := agent.File("/etc/hello")
	if !ok {
		t.Fatal("guest file /etc/hello was not written")
	}
	if content != "hello from the host\n" {
		t.Errorf("guest file content = %q", content)
	}
	if commands := agent.Commands(); !slices.ContainsFunc(commands, func(c string) bool {
		return strings.HasPrefix(c, "chmod 0600 /etc/hello")
	}) {
		t.Errorf("guest commands %v missing the chmod that applies File.Mode", commands)
	}
}

func TestRemoteControlRunsGuestCommands(t *testing.T) {
	b, _, _, agent := testBackend(Config{})
	agent.stdout = "ok\n"

	inst, err := b.Start(context.Background(), testSpec(t))
	if err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	t.Cleanup(func() { inst.Kill() })

	guest, err := inst.RemoteControl()
	if err != nil {
		t.Fatalf("RemoteControl() error = %v", err)
	}
	result, err := guest.Run(context.Background(), &vm.GuestCmd{Path: "true", Args: []string{"-x"}})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if got := string(result.Stdout); got != "ok\n" {
		t.Errorf("Stdout = %q, want %q", got, "ok\n")
	}
	if got := agent.Commands(); len(got) != 1 || got[0] != "true -x" {
		t.Errorf("guest commands = %v, want [\"true -x\"]", got)
	}

	// A second call reuses the dialed connection rather than reconnecting.
	again, err := inst.RemoteControl()
	if err != nil {
		t.Fatalf("second RemoteControl() error = %v", err)
	}
	if again != guest {
		t.Error("RemoteControl returned a second guest connection")
	}
}

func TestRemoteControlRunsWithWorkingDirectoryThroughAShell(t *testing.T) {
	b, _, _, agent := testBackend(Config{})
	inst, err := b.Start(context.Background(), testSpec(t))
	if err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	t.Cleanup(func() { inst.Kill() })

	guest, err := inst.RemoteControl()
	if err != nil {
		t.Fatalf("RemoteControl() error = %v", err)
	}
	if _, err := guest.Run(context.Background(), &vm.GuestCmd{Path: "make", Dir: "/workspace"}); err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	commands := agent.Commands()
	if len(commands) != 1 {
		t.Fatalf("guest commands = %v, want one", commands)
	}
	if !strings.HasPrefix(commands[0], "/bin/sh -c cd /workspace && exec make") {
		t.Errorf("guest command = %q, want a shell that changes directory first", commands[0])
	}
}

func TestKillStopsEveryProcessAndIsIdempotent(t *testing.T) {
	b, runner, monitor, agent := testBackend(Config{})
	spec := testSpec(t)
	spec.Shares = []vm.Share{{Tag: "workspace", HostPath: t.TempDir()}}

	inst, err := b.Start(context.Background(), spec)
	if err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	if _, err := inst.RemoteControl(); err != nil {
		t.Fatalf("RemoteControl() error = %v", err)
	}

	qemuName := runner.StartedNames()[len(runner.StartedNames())-1]
	if err := inst.Kill(); err != nil {
		t.Fatalf("Kill() error = %v", err)
	}
	for _, name := range []string{qemuName, "virtiofsd"} {
		if !slices.Contains(runner.SignalNames(), name) {
			t.Errorf("process %q was not signaled by Kill; signaled %v", name, runner.SignalNames())
		}
	}
	if !monitor.disconnected {
		t.Error("Kill left the QMP connection open")
	}
	if !agent.closed {
		t.Error("Kill left the guest agent connection open")
	}

	if err := inst.Kill(); err != nil {
		t.Fatalf("second Kill() error = %v", err)
	}
	if _, err := inst.RemoteControl(); !errors.Is(err, errors.ErrUnsupported) {
		t.Errorf("RemoteControl() after Kill error = %v, want errors.ErrUnsupported", err)
	}
}

func TestShutdownAsksTheGuestThenWaitsForTheVMToExit(t *testing.T) {
	b, runner, _, agent := testBackend(Config{})
	inst, err := b.Start(context.Background(), testSpec(t))
	if err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	// The fake QEMU never exits on its own, so let the guest's power-down
	// request end it the way a real one would.
	qemuName := runner.StartedNames()[len(runner.StartedNames())-1]
	agent.onShutdown = func() { runner.LastProcess(qemuName).Complete(nil) }

	if err := backend.Shutdown(context.Background(), inst); err != nil {
		t.Fatalf("Shutdown() error = %v", err)
	}
	if !agent.shutdown {
		t.Error("guest was never asked to power down")
	}
}

func TestResizeMemoryNeedsABalloonDevice(t *testing.T) {
	withBalloon, _, _, _ := testBackend(Config{Balloon: true})
	var resized int64
	withBalloon.setBalloonSize = func(_ context.Context, _ qmpclient.Client, sizeBytes int64) error {
		resized = sizeBytes
		return nil
	}
	inst, err := withBalloon.Start(context.Background(), testSpec(t))
	if err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	t.Cleanup(func() { inst.Kill() })

	if err := withBalloon.ResizeMemory(context.Background(), inst, 512*units.Mebibyte); err != nil {
		t.Fatalf("ResizeMemory() error = %v", err)
	}
	if want := (512 * units.Mebibyte).Int64(); resized != want {
		t.Errorf("balloon size = %d, want %d", resized, want)
	}

	withoutBalloon, _, _, _ := testBackend(Config{})
	plain, err := withoutBalloon.Start(context.Background(), testSpec(t))
	if err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	t.Cleanup(func() { plain.Kill() })
	err = withoutBalloon.ResizeMemory(context.Background(), plain, 512*units.Mebibyte)
	if !errors.Is(err, errors.ErrUnsupported) {
		t.Errorf("ResizeMemory() without a balloon device error = %v, want errors.ErrUnsupported", err)
	}
}

func TestSuspendSavesStateAndResumeRestoresIt(t *testing.T) {
	b, _, monitor, _ := testBackend(Config{})
	spec := testSpec(t)
	stateDir := filepath.Join(t.TempDir(), "suspended")

	inst, err := b.Start(context.Background(), spec)
	if err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	if err := b.Suspend(context.Background(), inst, stateDir); err != nil {
		t.Fatalf("Suspend() error = %v", err)
	}
	if !slices.Contains(monitor.Calls(), "migrate-to-file:"+filepath.Join(stateDir, suspendVMStateFile)) {
		t.Errorf("monitor calls %v do not include the state save", monitor.Calls())
	}
	// Suspend saves through QEMU, which writes the file; the fake monitor
	// does not, so stand in for it before resuming.
	if err := os.WriteFile(filepath.Join(stateDir, suspendVMStateFile), []byte("vmstate"), 0o644); err != nil {
		t.Fatal(err)
	}

	resumed, err := b.Resume(context.Background(), spec, stateDir)
	if err != nil {
		t.Fatalf("Resume() error = %v", err)
	}
	t.Cleanup(func() { resumed.Kill() })
	if !slices.Contains(monitor.Calls(), "migrate-incoming:"+filepath.Join(stateDir, suspendVMStateFile)) {
		t.Errorf("monitor calls %v do not include the state restore", monitor.Calls())
	}
	if _, err := os.Stat(filepath.Join(stateDir, suspendStateFile)); !errors.Is(err, os.ErrNotExist) {
		t.Error("resumed suspend state was not consumed")
	}
}

func TestResumeRejectsStateFromAnotherVersion(t *testing.T) {
	b, _, _, _ := testBackend(Config{})
	stateDir := t.TempDir()
	stale := `{"version":0,"cid":3,"vmStatePath":"` + filepath.Join(stateDir, suspendVMStateFile) + `"}`
	if err := os.WriteFile(filepath.Join(stateDir, suspendStateFile), []byte(stale), 0o644); err != nil {
		t.Fatal(err)
	}

	_, err := b.Resume(context.Background(), testSpec(t), stateDir)
	if err == nil {
		t.Fatal("Resume() error = nil, want a version mismatch")
	}
	if !strings.Contains(err.Error(), "version") {
		t.Errorf("Resume() error = %v, want it to name the version mismatch", err)
	}
}

func TestBackendSatisfiesTheCapabilitiesItAdvertises(t *testing.T) {
	b, err := BackendWithQGA(Config{})
	if err != nil {
		t.Fatalf("BackendWithQGA() error = %v", err)
	}
	if _, ok := b.(backend.Suspender); !ok {
		t.Error("qemu backend does not satisfy backend.Suspender")
	}
	if _, ok := b.(backend.MemoryResizer); !ok {
		t.Error("qemu backend does not satisfy backend.MemoryResizer")
	}
}

func TestCapabilitiesRejectForeignInstances(t *testing.T) {
	first, _, _, _ := testBackend(Config{})
	second, _, _, _ := testBackend(Config{})

	inst, err := first.Start(context.Background(), testSpec(t))
	if err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	t.Cleanup(func() { inst.Kill() })

	if err := second.Suspend(context.Background(), inst, t.TempDir()); err == nil {
		t.Fatal("Suspend() accepted an instance from another backend")
	}
}
