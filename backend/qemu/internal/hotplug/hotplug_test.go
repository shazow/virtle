package hotplug

import (
	"context"
	"encoding/json"
	"errors"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/shazow/virtle/internal/executor"
	"github.com/shazow/virtle/internal/executor/executortest"
	"github.com/shazow/virtle/internal/manifest"
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

func TestVirtioFSAttachStartsSupervisedHelperAndMountsGuest(t *testing.T) {
	tmpDir := t.TempDir()
	runner, starter, qmp, guest := testRunner(tmpDir, manifest.HotplugDevice{
		Kind: manifest.HotplugKindVirtioFS,
		ID:   "cache",
		VirtioFS: manifest.HotplugVirtioFS{
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
	if got, want := starter.tracked, []int{100}; !reflect.DeepEqual(got, want) {
		t.Fatalf("supervised helper pids: got %#v want %#v", got, want)
	}
	if !strings.Contains(strings.Join(qmp.commands, "\n"), `"execute":"chardev-add"`) {
		t.Fatalf("expected chardev-add, got %#v", qmp.commands)
	}
	if got, want := guest.commands, [][]string{{"mount", "-t", "virtiofs", "cache", "/mnt/cache"}}; !reflect.DeepEqual(got, want) {
		t.Fatalf("guest commands: got %#v want %#v", got, want)
	}
}

func TestVirtioFSHelperExitStillAllowsDeviceDetach(t *testing.T) {
	tmpDir := t.TempDir()
	runner, starter, _, _ := testRunner(tmpDir, testVirtioFSDevice(tmpDir))
	starter.forgot = make(chan int, 1)

	if err := runner.Attach(context.Background(), "cache"); err != nil {
		t.Fatalf("attach: %v", err)
	}
	starter.processes[100].Complete(errors.New("virtiofsd exited"))

	select {
	case pid := <-starter.forgot:
		if pid != 100 {
			t.Fatalf("forgot helper pid %d, want 100", pid)
		}
	case <-time.After(time.Second):
		t.Fatal("helper exit was not observed")
	}
	if err := runner.Detach(context.Background(), "cache"); err != nil {
		t.Fatalf("detach after helper exit: %v", err)
	}
}

func TestMultipleAdHocHotplugDevicesUseDistinctReservedPorts(t *testing.T) {
	tmpDir := t.TempDir()
	secondShare := testVirtioFSDevice(tmpDir)
	secondShare.ID = "archive"
	secondShare.VirtioFS.SocketPath = filepath.Join(tmpDir, "archive.sock")
	devices := []manifest.HotplugDevice{
		testVirtioFSDevice(tmpDir),
		secondShare,
		{Kind: manifest.HotplugKindBlock, ID: "data", Block: manifest.HotplugBlock{ImagePath: "/tmp/data.raw", Format: "raw"}},
		{Kind: manifest.HotplugKindBlock, ID: "scratch", Block: manifest.HotplugBlock{ImagePath: "/tmp/scratch.raw", Format: "raw"}},
		{Kind: manifest.HotplugKindNet, ID: "web", Net: manifest.HotplugNet{Backend: "user", MAC: "02:00:00:00:00:10"}},
		{Kind: manifest.HotplugKindNet, ID: "dns", Net: manifest.HotplugNet{Backend: "user", MAC: "02:00:00:00:00:11"}},
	}
	runner, _, qmp, _ := testRunnerDevices(tmpDir, nil)
	runner.Ports = len(devices)

	for _, device := range devices {
		if err := runner.AttachDevice(context.Background(), device); err != nil {
			t.Fatalf("attach %q: %v", device.ID, err)
		}
	}
	joined := strings.Join(qmp.commands, "\n")
	for port := range devices {
		bus := BusName(port)
		if !strings.Contains(joined, `"bus":"`+bus+`"`) {
			t.Fatalf("QMP plans do not use reserved bus %q: %#v", bus, qmp.commands)
		}
	}
	for _, device := range devices {
		if err := runner.Detach(context.Background(), device.ID); err != nil {
			t.Fatalf("detach %q: %v", device.ID, err)
		}
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
	qmp.errAt = 0
	if err := runner.Attach(context.Background(), "cache"); err != nil {
		t.Fatalf("retry attach after rollback: %v", err)
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
	if err := runner.Attach(context.Background(), "cache"); err != nil {
		t.Fatalf("attach: %v", err)
	}
	qmp.events = nil
	qmp.commands = nil
	guest.commands = nil

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
	if err := runner.Attach(context.Background(), "cache"); err != nil {
		t.Fatalf("attach: %v", err)
	}
	qmp.events = nil
	qmp.commands = nil
	qmp.afterDeviceDel = cancel

	if err := runner.Detach(ctx, "cache"); err != nil {
		t.Fatalf("detach: %v", err)
	}
	if got, want := qmp.events, []string{"device_del:dev-cache", "run:chardev-remove"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("events: got %#v want %#v", got, want)
	}
}

func TestVirtioFSDetachCompletesQMPAfterGuestUnmountCancellation(t *testing.T) {
	tmpDir := t.TempDir()
	ctx, cancel := context.WithCancel(context.Background())
	runner, _, qmp, guest := testRunner(tmpDir, testVirtioFSDevice(tmpDir))
	if err := runner.Attach(context.Background(), "cache"); err != nil {
		t.Fatalf("attach: %v", err)
	}
	qmp.events = nil
	qmp.commands = nil
	guest.commands = nil
	guest.afterRun = cancel

	if err := runner.Detach(ctx, "cache"); err != nil {
		t.Fatalf("detach: %v", err)
	}
	if got, want := qmp.events, []string{"device_del:dev-cache", "run:chardev-remove"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("events: got %#v want %#v", got, want)
	}
}

func TestNetAttachDetachCommands(t *testing.T) {
	tmpDir := t.TempDir()
	runner, _, qmp, _ := testRunner(tmpDir, manifest.HotplugDevice{
		Kind: manifest.HotplugKindNet,
		ID:   "vpn",
		Net: manifest.HotplugNet{
			Backend: "user",
			MAC:     "02:02:00:00:00:10",
			Forward: []manifest.HotplugForward{{Proto: "tcp", Host: "127.0.0.1:2223", Guest: "10.0.2.15:22"}},
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

func TestBlockAttachDetachCommands(t *testing.T) {
	tmpDir := t.TempDir()
	runner, _, qmp, _ := testRunner(tmpDir, manifest.HotplugDevice{
		Kind: manifest.HotplugKindBlock,
		ID:   "data",
		Block: manifest.HotplugBlock{
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

func testRunner(tmpDir string, device manifest.HotplugDevice) (Runner, *fakeStarter, *fakeQMPClient, *fakeGuest) {
	return testRunnerDevices(tmpDir, []manifest.HotplugDevice{device})
}

func testRunnerDevices(tmpDir string, devices []manifest.HotplugDevice) (Runner, *fakeStarter, *fakeQMPClient, *fakeGuest) {
	starter := &fakeStarter{}
	client := &fakeQMPClient{}
	guest := &fakeGuest{}
	return Runner{
		WorkDir: tmpDir,
		Devices: devices,
		Start:   starter,
		Sockets: fakeSockets{},
		QMP:     QMPDeviceAdapter{Client: client},
		Guest:   guest,
		Runtime: NewRuntime(starter),
		Ports:   max(4, len(devices)),
	}, starter, client, guest
}

func testVirtioFSDevice(tmpDir string) manifest.HotplugDevice {
	return manifest.HotplugDevice{
		Kind: manifest.HotplugKindVirtioFS,
		ID:   "cache",
		VirtioFS: manifest.HotplugVirtioFS{
			Source:     filepath.Join(tmpDir, "cache"),
			Target:     "/mnt/cache",
			SocketPath: filepath.Join(tmpDir, "cache.sock"),
			Bin:        "/bin/virtiofsd",
		},
	}
}

type fakeStarter struct {
	starts    []string
	stopped   []int
	tracked   []int
	processes map[int]*executortest.Process
	forgot    chan int
}

func (s *fakeStarter) Start(ctx context.Context, cmd *exec.Cmd) (*executor.Process, error) {
	name := filepath.Base(cmd.Path)
	if len(cmd.Args) > 0 && cmd.Args[0] != "" {
		name = filepath.Base(cmd.Args[0])
	}
	s.starts = append(s.starts, name)
	if s.processes == nil {
		s.processes = make(map[int]*executortest.Process)
	}
	pid := 100 + len(s.processes)
	process := &executortest.Process{OverrideName: name, OverridePID: pid}
	s.processes[process.PID()] = process
	return process.Process(), nil
}

func (s *fakeStarter) Stop(process *executor.Process) error {
	s.stopped = append(s.stopped, process.PID())
	if running := s.processes[process.PID()]; running != nil {
		running.Complete(nil)
	}
	return nil
}

func (s *fakeStarter) Add(processes ...*executor.Process) {
	for _, process := range processes {
		s.tracked = append(s.tracked, process.PID())
	}
}

func (s *fakeStarter) Remove(process *executor.Process) bool {
	if s.forgot != nil {
		s.forgot <- process.PID()
	}
	return true
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
