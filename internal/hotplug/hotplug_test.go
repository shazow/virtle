package hotplug

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"syscall"
	"testing"

	"github.com/shazow/virtle/internal/executor"
	"github.com/shazow/virtle/internal/executor/executortest"
)

func TestQMPDeviceAdapterAttachesVirtioFSWithoutCallerRawJSON(t *testing.T) {
	client := &fakeQMPClient{}
	adapter := QMPDeviceAdapter{Client: client}
	device := testVirtioFSDevice(t.TempDir())

	rollback, err := adapter.AttachDevice(context.Background(), device, "pcie.hotplug.0")
	if err != nil {
		t.Fatalf("attach device: %v", err)
	}
	if rollback == nil {
		t.Fatal("expected rollback function")
	}
	if got, want := client.events, []string{"run:chardev-add", "run:device_add"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("events: got %#v want %#v", got, want)
	}
}

func TestQMPDeviceAdapterAttachStopsBeforeNextCommandWhenContextCanceled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	client := &fakeQMPClient{afterRun: cancel}
	adapter := QMPDeviceAdapter{Client: client}

	rollback, err := adapter.AttachDevice(ctx, testVirtioFSDevice(t.TempDir()), "pcie.hotplug.0")
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("attach error: got %v want context canceled", err)
	}
	if rollback != nil {
		t.Fatal("expected no rollback function")
	}
	if got, want := client.events, []string{"run:chardev-add", "run:chardev-remove"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("events: got %#v want %#v", got, want)
	}
}

func TestQMPDeviceAdapterDetachWaitsBeforeCleanup(t *testing.T) {
	client := &fakeQMPClient{}
	adapter := QMPDeviceAdapter{Client: client}

	if err := adapter.DetachDevice(context.Background(), testVirtioFSDevice(t.TempDir())); err != nil {
		t.Fatalf("detach device: %v", err)
	}
	if got, want := client.events, []string{"device_del:dev-cache", "run:chardev-remove"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("events: got %#v want %#v", got, want)
	}
}

func TestQMPDeviceAdapterDetachFinishesCleanupAfterDeviceDelCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	client := &fakeQMPClient{afterDeviceDel: cancel}
	adapter := QMPDeviceAdapter{Client: client}

	err := adapter.DetachDevice(ctx, testVirtioFSDevice(t.TempDir()))
	if err != nil {
		t.Fatalf("detach device: %v", err)
	}
	if got, want := client.events, []string{"device_del:dev-cache", "run:chardev-remove"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("events: got %#v want %#v", got, want)
	}
}

func TestVirtioFSAttachSuccessWritesState(t *testing.T) {
	tmpDir := t.TempDir()
	runner, starter, qmp, guest := testRunner(tmpDir, Device{
		Kind: KindVirtioFS,
		ID:   "cache",
		VirtioFS: VirtioFS{
			Source:     filepath.Join(tmpDir, "cache"),
			Target:     "/mnt/cache",
			SocketPath: filepath.Join(tmpDir, "cache.sock"),
			Bin:        "/bin/virtiofsd",
			Args:       []string{"--socket"},
		},
	})

	if err := runner.Attach(context.Background(), "cache"); err != nil {
		t.Fatalf("attach: %v", err)
	}
	if got, want := starter.starts, []string{"virtiofsd"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("starts: got %#v want %#v", got, want)
	}
	if !strings.Contains(strings.Join(qmp.commands, "\n"), `"execute":"chardev-add"`) {
		t.Fatalf("expected chardev-add, got %#v", qmp.commands)
	}
	if got, want := guest.commands, [][]string{{"mount", "-t", "virtiofs", "cache", "/mnt/cache"}}; !reflect.DeepEqual(got, want) {
		t.Fatalf("guest commands: got %#v want %#v", got, want)
	}
	state, err := ReadState(filepath.Join(tmpDir, "state", "hotplug", "cache.json"))
	if err != nil {
		t.Fatalf("read state: %v", err)
	}
	if state.ID != "cache" || state.Kind != KindVirtioFS || state.Bus != "pcie.hotplug.0" || state.PID != 100 {
		t.Fatalf("unexpected state: %#v", state)
	}
}

func TestVirtioFSAttachQMPFailureRollsBackHost(t *testing.T) {
	tmpDir := t.TempDir()
	runner, starter, qmp, _ := testRunner(tmpDir, testVirtioFSDevice(tmpDir))
	qmp.errAt = 2

	if err := runner.Attach(context.Background(), "cache"); err == nil {
		t.Fatal("expected attach failure")
	}
	if got, want := starter.stopped, []int{100}; !reflect.DeepEqual(got, want) {
		t.Fatalf("stopped: got %#v want %#v", got, want)
	}
	if got := strings.Join(qmp.commands, "\n"); !strings.Contains(got, `"execute":"chardev-remove"`) {
		t.Fatalf("expected qmp rollback, got %#v", qmp.commands)
	}
	if _, err := os.Stat(filepath.Join(tmpDir, "state", "hotplug", "cache.json")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("expected no state file, got %v", err)
	}
}

func TestVirtioFSAttachGuestFailureRollsBackQMPAndHost(t *testing.T) {
	tmpDir := t.TempDir()
	runner, starter, qmp, guest := testRunner(tmpDir, testVirtioFSDevice(tmpDir))
	guest.err = errors.New("mount failed")

	if err := runner.Attach(context.Background(), "cache"); err == nil {
		t.Fatal("expected attach failure")
	}
	if got, want := qmp.deviceDels, []string{"dev-cache"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("device dels: got %#v want %#v", got, want)
	}
	if got, want := starter.stopped, []int{100}; !reflect.DeepEqual(got, want) {
		t.Fatalf("stopped: got %#v want %#v", got, want)
	}
}

func TestVirtioFSDetachWaitsForDeviceDeletedBeforeChardevRemove(t *testing.T) {
	tmpDir := t.TempDir()
	runner, _, qmp, guest := testRunner(tmpDir, testVirtioFSDevice(tmpDir))
	statePath := filepath.Join(tmpDir, "state", "hotplug", "cache.json")
	if err := WriteState(statePath, State{ID: "cache", Kind: KindVirtioFS, Bus: "pcie.hotplug.0", PID: 42}); err != nil {
		t.Fatalf("write state: %v", err)
	}

	if err := runner.Detach(context.Background(), "cache"); err != nil {
		t.Fatalf("detach: %v", err)
	}
	if got, want := qmp.events, []string{"device_del:dev-cache", "run:chardev-remove"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("events: got %#v want %#v", got, want)
	}
	if got, want := guest.commands, [][]string{{"umount", "/mnt/cache"}}; !reflect.DeepEqual(got, want) {
		t.Fatalf("guest commands: got %#v want %#v", got, want)
	}
}

func TestVirtioFSDetachCompletesCleanupAfterDeviceDelCancellation(t *testing.T) {
	tmpDir := t.TempDir()
	ctx, cancel := context.WithCancel(context.Background())
	runner, _, qmp, _ := testRunner(tmpDir, testVirtioFSDevice(tmpDir))
	qmp.afterDeviceDel = cancel
	statePath := filepath.Join(tmpDir, "state", "hotplug", "cache.json")
	if err := WriteState(statePath, State{ID: "cache", Kind: KindVirtioFS, Bus: "pcie.hotplug.0", PID: 42}); err != nil {
		t.Fatalf("write state: %v", err)
	}

	if err := runner.Detach(ctx, "cache"); err != nil {
		t.Fatalf("detach: %v", err)
	}
	if got, want := qmp.events, []string{"device_del:dev-cache", "run:chardev-remove"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("events: got %#v want %#v", got, want)
	}
	if _, err := os.Stat(statePath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("expected hotplug state file removed, got %v", err)
	}
}

func TestVirtioFSDetachCompletesQMPAfterGuestUnmountCancellation(t *testing.T) {
	tmpDir := t.TempDir()
	ctx, cancel := context.WithCancel(context.Background())
	runner, _, qmp, guest := testRunner(tmpDir, testVirtioFSDevice(tmpDir))
	guest.afterRun = cancel
	statePath := filepath.Join(tmpDir, "state", "hotplug", "cache.json")
	if err := WriteState(statePath, State{ID: "cache", Kind: KindVirtioFS, Bus: "pcie.hotplug.0", PID: 42}); err != nil {
		t.Fatalf("write state: %v", err)
	}

	if err := runner.Detach(ctx, "cache"); err != nil {
		t.Fatalf("detach: %v", err)
	}
	if got, want := qmp.events, []string{"device_del:dev-cache", "run:chardev-remove"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("events: got %#v want %#v", got, want)
	}
	if _, err := os.Stat(statePath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("expected hotplug state file removed, got %v", err)
	}
}

func TestNetAttachDetachCommands(t *testing.T) {
	tmpDir := t.TempDir()
	runner, _, qmp, _ := testRunner(tmpDir, Device{
		Kind: KindNet,
		ID:   "vpn",
		Net: Net{
			Backend: "user",
			MAC:     "02:02:00:00:00:10",
			Forward: []Forward{{Proto: "tcp", Host: "127.0.0.1:2223", Guest: "10.0.2.15:22"}},
		},
	})

	if err := runner.Attach(context.Background(), "vpn"); err != nil {
		t.Fatalf("attach: %v", err)
	}
	if err := runner.Detach(context.Background(), "vpn"); err != nil {
		t.Fatalf("detach: %v", err)
	}
	joined := strings.Join(qmp.commands, "\n")
	for _, want := range []string{`"execute":"netdev_add"`, `"execute":"device_add"`, `"execute":"netdev_del"`} {
		if !strings.Contains(joined, want) {
			t.Fatalf("expected %s in qmp commands: %#v", want, qmp.commands)
		}
	}
	if got, want := qmp.deviceDels, []string{"dev-vpn"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("device dels: got %#v want %#v", got, want)
	}
}

func TestDetachRejectsStateKindMismatchBeforeCleanup(t *testing.T) {
	tmpDir := t.TempDir()
	runner, _, qmp, _ := testRunner(tmpDir, Device{
		Kind: KindNet,
		ID:   "cache",
		Net:  Net{Backend: "user", MAC: "02:02:00:00:00:10"},
	})
	statePath := filepath.Join(tmpDir, "state", "hotplug", "cache.json")
	if err := WriteState(statePath, State{ID: "cache", Kind: KindVirtioFS, Bus: "pcie.hotplug.0", PID: 42}); err != nil {
		t.Fatalf("write state: %v", err)
	}

	err := runner.Detach(context.Background(), "cache")
	if err == nil || !strings.Contains(err.Error(), `kind "virtiofs", not current manifest kind "net"`) {
		t.Fatalf("expected kind mismatch error, got %v", err)
	}
	if len(qmp.commands) != 0 || len(qmp.deviceDels) != 0 {
		t.Fatalf("expected no qmp cleanup, got commands=%#v deviceDels=%#v", qmp.commands, qmp.deviceDels)
	}
	if _, err := os.Stat(statePath); err != nil {
		t.Fatalf("expected state file to remain, got %v", err)
	}
}

func TestBlockAttachDetachCommands(t *testing.T) {
	tmpDir := t.TempDir()
	runner, _, qmp, _ := testRunner(tmpDir, Device{
		Kind: KindBlock,
		ID:   "data",
		Block: Block{
			ImagePath: filepath.Join(tmpDir, "data.qcow2"),
			Format:    "qcow2",
			Serial:    "data",
		},
	})

	if err := runner.Attach(context.Background(), "data"); err != nil {
		t.Fatalf("attach: %v", err)
	}
	if err := runner.Detach(context.Background(), "data"); err != nil {
		t.Fatalf("detach: %v", err)
	}
	joined := strings.Join(qmp.commands, "\n")
	for _, want := range []string{`"execute":"blockdev-add"`, `"execute":"device_add"`, `"execute":"blockdev-del"`} {
		if !strings.Contains(joined, want) {
			t.Fatalf("expected %s in qmp commands: %#v", want, qmp.commands)
		}
	}
	if got, want := qmp.deviceDels, []string{"dev-data"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("device dels: got %#v want %#v", got, want)
	}
}

func TestHotplugRegistryMissingID(t *testing.T) {
	tmpDir := t.TempDir()
	runner, _, _, _ := testRunner(tmpDir, testVirtioFSDevice(tmpDir))

	err := runner.Attach(context.Background(), "missing")
	if err == nil || !strings.Contains(err.Error(), `manifest.hotplug id "missing" not found`) {
		t.Fatalf("expected missing id error, got %v", err)
	}
}

func TestHotplugRegistryRejectsDuplicateIDs(t *testing.T) {
	tmpDir := t.TempDir()
	runner, _, _, _ := testRunnerDevices(tmpDir, []Device{
		testVirtioFSDevice(tmpDir),
		{
			Kind: KindNet,
			ID:   "cache",
			Net:  Net{Backend: "user", MAC: "02:02:00:00:00:10"},
		},
	})

	err := runner.Attach(context.Background(), "cache")
	if err == nil || !strings.Contains(err.Error(), `manifest.hotplug id "cache" is duplicated`) {
		t.Fatalf("expected duplicate id error, got %v", err)
	}
}

func TestHotplugRegistryRejectsUnsupportedKind(t *testing.T) {
	tmpDir := t.TempDir()
	runner, _, _, _ := testRunnerDevices(tmpDir, []Device{
		testVirtioFSDevice(tmpDir),
		{Kind: Kind("unsupported"), ID: "bad"},
	})

	err := runner.Attach(context.Background(), "bad")
	if err == nil || !strings.Contains(err.Error(), `manifest.hotplug id "bad" has unsupported kind "unsupported"`) {
		t.Fatalf("expected unsupported kind error, got %v", err)
	}
}

func testRunner(tmpDir string, device Device) (Runner, *fakeStarter, *fakeQMPClient, *fakeGuest) {
	return testRunnerDevices(tmpDir, []Device{device})
}

func testRunnerDevices(tmpDir string, devices []Device) (Runner, *fakeStarter, *fakeQMPClient, *fakeGuest) {
	starter := &fakeStarter{}
	client := &fakeQMPClient{}
	guest := &fakeGuest{}
	return Runner{
		StateDir: filepath.Join(tmpDir, "state"),
		WorkDir:  tmpDir,
		Devices:  devices,
		Start:    starter,
		Sockets:  fakeSockets{},
		QMP:      QMPDeviceAdapter{Client: client},
		Guest:    guest,
	}, starter, client, guest
}

// Every sequence in devicePlan is written out per kind rather than derived
// from the attach commands. The cost of that is a new kind silently omitting
// one and skipping its cleanup; this pins it.
func TestDevicePlansDeclareEverySequenceExplicitly(t *testing.T) {
	devices := []Device{
		testVirtioFSDevice(t.TempDir()),
		{Kind: KindNet, ID: "net0", Net: Net{Backend: "user", MAC: "02:02:00:00:00:10"}},
		{Kind: KindBlock, ID: "data", Block: Block{ImagePath: "/tmp/data.img", Format: "raw"}},
	}

	for _, device := range devices {
		t.Run(string(device.Kind), func(t *testing.T) {
			plan := planFor(device, "pcie.hotplug.0")
			if len(plan.backend) == 0 {
				t.Errorf("backend sequence is empty")
			}
			if plan.deviceAdd == "" {
				t.Errorf("deviceAdd command is empty")
			}
			if len(plan.release) == 0 {
				t.Errorf("release sequence is empty: attach rollback and detach would both leak the backend")
			}
		})
	}
}

func testVirtioFSDevice(tmpDir string) Device {
	return Device{
		Kind: KindVirtioFS,
		ID:   "cache",
		VirtioFS: VirtioFS{
			Source:     filepath.Join(tmpDir, "cache"),
			Target:     "/mnt/cache",
			SocketPath: filepath.Join(tmpDir, "cache.sock"),
			Bin:        "/bin/virtiofsd",
		},
	}
}

type fakeStarter struct {
	starts  []string
	stopped []int
}

func (s *fakeStarter) Start(ctx context.Context, cmd *exec.Cmd) (*executor.Process, error) {
	name := filepath.Base(cmd.Path)
	if len(cmd.Args) > 0 && cmd.Args[0] != "" {
		name = filepath.Base(cmd.Args[0])
	}
	s.starts = append(s.starts, name)
	return (&executortest.Process{OverrideName: name, OverridePID: 100, Exited: true}).Process(), nil
}

func (s *fakeStarter) Stop(process *executor.Process) error {
	s.stopped = append(s.stopped, process.PID())
	return nil
}

func (s *fakeStarter) SignalPIDGroup(pid int, signal syscall.Signal) error {
	s.stopped = append(s.stopped, pid)
	return nil
}

type fakeSockets struct{}

func (fakeSockets) Wait(ctx context.Context, stage string, socketPaths []string, process *executor.Process) error {
	return nil
}

type fakeQMPClient struct {
	commands       []string
	deviceDels     []string
	events         []string
	errAt          int
	afterRun       func()
	afterDeviceDel func()
}

func (q *fakeQMPClient) RunRaw(ctx context.Context, command string) error {
	q.commands = append(q.commands, command)
	var message struct {
		Execute string `json:"execute"`
	}
	_ = jsonUnmarshal(command, &message)
	q.events = append(q.events, "run:"+message.Execute)
	if q.errAt > 0 && len(q.commands) == q.errAt {
		return errors.New("qmp failed")
	}
	if q.afterRun != nil {
		q.afterRun()
	}
	return nil
}

func (q *fakeQMPClient) DeviceDelAndWait(ctx context.Context, id string) error {
	q.deviceDels = append(q.deviceDels, id)
	q.events = append(q.events, "device_del:"+id)
	if q.afterDeviceDel != nil {
		q.afterDeviceDel()
	}
	return nil
}

type fakeGuest struct {
	commands [][]string
	err      error
	afterRun func()
}

func (g *fakeGuest) Run(ctx context.Context, command []string) error {
	g.commands = append(g.commands, append([]string(nil), command...))
	if g.afterRun != nil {
		g.afterRun()
	}
	return g.err
}

func jsonUnmarshal(data string, v any) error {
	return json.Unmarshal([]byte(data), v)
}
