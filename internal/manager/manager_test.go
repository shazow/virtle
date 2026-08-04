package manager

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/adrg/xdg"
	doQMP "github.com/digitalocean/go-qemu/qmp"
	rawQMP "github.com/digitalocean/go-qemu/qmp/raw"
	diskfs "github.com/diskfs/go-diskfs"
	"github.com/diskfs/go-diskfs/filesystem"
	"github.com/shazow/virtle/internal/balloon"
	"github.com/shazow/virtle/internal/executor"
	"github.com/shazow/virtle/internal/executor/executortest"
	"github.com/shazow/virtle/internal/hotplug"
	control "github.com/shazow/virtle/internal/manager/control"
	"github.com/shazow/virtle/internal/manager/launch"
	runtimepkg "github.com/shazow/virtle/internal/manager/runtime"
	"github.com/shazow/virtle/internal/manifest"
	"github.com/shazow/virtle/internal/qga"
	"github.com/shazow/virtle/internal/qmpclient"
	"github.com/shazow/virtle/internal/units"
)

const (
	testMiB             int64 = 1024 * 1024
	testNoReturnTimeout       = 50 * time.Millisecond
)

func manifestWriteText(text string) manifest.WriteFile {
	return manifest.WriteFile{
		Content:     manifest.WriteFileContent{Kind: manifest.WriteFileContentText, Text: text},
		FollowLinks: true,
	}
}

func manifestWritePath(path string) manifest.WriteFile {
	return manifest.WriteFile{
		Content:     manifest.WriteFileContent{Kind: manifest.WriteFileContentPath, Path: path},
		FollowLinks: true,
	}
}

func newTestLaunchLifecycle() *launch.Lifecycle {
	return launch.NewLifecycle(nil, func() {}, func() {})
}

func createStaleUnixSocket(t *testing.T, path string) {
	t.Helper()
	if err := createStaleUnixSocketPath(path); err != nil {
		t.Fatalf("create stale unix socket %q: %v", path, err)
	}
}

func createStaleUnixSocketPath(path string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	_ = os.Remove(path)
	fd, err := syscall.Socket(syscall.AF_UNIX, syscall.SOCK_STREAM, 0)
	if err != nil {
		return err
	}
	if err := syscall.Bind(fd, &syscall.SockaddrUnix{Name: path}); err != nil {
		_ = syscall.Close(fd)
		return err
	}
	return syscall.Close(fd)
}

type fakeSuspendHandler struct {
	err error
}

func (h fakeSuspendHandler) Handle(ctx context.Context, coordinator *launch.SuspendCoordinator) error {
	_ = ctx
	coordinator.Begin()
	coordinator.Complete(h.err)
	return h.err
}

func TestManagerStartExternalVirtioFSFailureKeepsRuntimeSockets(t *testing.T) {
	tmpDir := t.TempDir()
	manager := newLaunchProviderTestManager(nil)
	cfg := validProviderLaunchManifest(tmpDir)
	cfg.QEMU.Devices.VirtioFS = []manifest.QEMUVirtioFSShare{
		{
			ID:         "fs0",
			SocketPath: filepath.Join(tmpDir, "missing-external.sock"),
			Tag:        "workspace",
			Transport:  "pci",
		},
	}
	plan, err := manager.planLaunch(launch.Spec{Manifest: cfg, Options: launch.Options{SSH: false}})
	if err != nil {
		t.Fatalf("plan launch: %v", err)
	}
	for _, path := range plan.RuntimeSocketCleanupFiles() {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatalf("create socket parent: %v", err)
		}
		if err := os.WriteFile(path, []byte("stale socket placeholder"), 0o644); err != nil {
			t.Fatalf("write socket placeholder: %v", err)
		}
	}

	_, err = manager.startWithPlan(context.Background(), plan)
	if err == nil {
		t.Fatal("expected external virtiofs failure")
	}
	if !strings.Contains(err.Error(), "external virtiofs socket") {
		t.Fatalf("error: got %v want external virtiofs socket failure", err)
	}
	for _, path := range plan.RuntimeSocketCleanupFiles() {
		if _, statErr := os.Stat(path); statErr != nil {
			t.Fatalf("runtime socket cleanup path %q was removed: %v", path, statErr)
		}
	}
}

func TestManagerStartQEMULogsFullCommand(t *testing.T) {
	var logOutput bytes.Buffer
	runner := &executortest.Runner{}
	manager := &manager{
		runner: runner,
		logger: slog.New(slog.NewTextHandler(&logOutput, nil)),
	}
	cmd := exec.Command("/bin/qemu-system-x86_64", "-name", "guest vm", "-append", "console=ttyS0 quiet")

	if _, err := manager.startQEMU(cmd); err != nil {
		t.Fatalf("start qemu: %v", err)
	}

	logs := logOutput.String()
	for _, want := range []string{
		"msg=\"starting qemu\"",
		"/bin/qemu-system-x86_64",
		"-name 'guest vm'",
		"-append 'console=ttyS0 quiet'",
	} {
		if !strings.Contains(logs, want) {
			t.Fatalf("expected qemu log to contain %q, got %q", want, logs)
		}
	}
}

func TestManagerStartQEMUNilRunnerWrapsOnce(t *testing.T) {
	tmpDir := t.TempDir()
	manager := newLaunchProviderTestManager(nil)
	cfg := validProviderLaunchManifest(tmpDir)
	plan, err := manager.planLaunch(launch.Spec{Manifest: cfg, Options: launch.Options{SSH: false}})
	if err != nil {
		t.Fatalf("plan launch: %v", err)
	}

	_, err = manager.startWithPlan(context.Background(), plan)
	if err == nil {
		t.Fatal("expected nil runner failure")
	}
	if got, want := strings.Count(err.Error(), "vm startup:"), 1; got != want {
		t.Fatalf("vm startup wrapping count: got %d want %d in %q", got, want, err.Error())
	}
	if !strings.Contains(err.Error(), "qemu runner is not configured") {
		t.Fatalf("error: got %v want qemu runner is not configured", err)
	}
}

func newLaunchProviderTestManager(runner launch.Runner) *manager {
	return &manager{
		locker:            &fileLocker{},
		runner:            runner,
		logger:            slog.New(slog.DiscardHandler),
		shutdownDelay:     time.Millisecond,
		qmpConnectTimeout: time.Second,
	}
}

func validProviderLaunchManifest(workingDir string) *manifest.Manifest {
	cfg := validManifest(workingDir)
	cfg.Paths.LockPath = filepath.Join(workingDir, "virtle.lock")
	cfg.Run = nil
	cfg.CleanupFiles = nil
	cfg.Volumes = nil
	cfg.QEMU.Devices.VirtioFS = nil
	cfg.QEMU.Devices.Block = nil
	cfg.QEMU.SSHReady.SocketPath = ""
	return cfg
}

func TestManifestValidate(t *testing.T) {
	emptyManifest := &manifest.Manifest{}
	if err := emptyManifest.Validate(); err == nil {
		t.Fatalf("expected validation error for empty manifest")
	}

	valid := validManifest("/tmp/work")

	if err := valid.Validate(); err != nil {
		t.Fatalf("unexpected validation error: %v", err)
	}
	if got, want := valid.VSock.CIDRange.Start, 3; got != want {
		t.Fatalf("unexpected default vsock start: got %d want %d", got, want)
	}
	if got, want := valid.VSock.CIDRange.End, 65535; got != want {
		t.Fatalf("unexpected default vsock end: got %d want %d", got, want)
	}

	invalidUser := *valid
	invalidUser.SSH.User = ""
	if err := invalidUser.Validate(); err == nil {
		t.Fatalf("expected validation error for missing ssh user")
	}

	invalidRange := *valid
	invalidRange.VSock.CIDRange.Start = 2
	if err := invalidRange.Validate(); err == nil {
		t.Fatalf("expected validation error for invalid cid range")
	}

	invalidQMP := *valid
	invalidQMP.QEMU.QMP.SocketPath = ""
	if err := invalidQMP.Validate(); err == nil {
		t.Fatalf("expected validation error for missing qmp socket path")
	}
}

func TestManagerPlanLaunchResolvesRuntimeInputs(t *testing.T) {
	tmpDir := t.TempDir()
	cfg := validManifest(tmpDir)
	cfg.Paths.RuntimeDir = manifest.RuntimeDir{Mode: manifest.RuntimeDirPath, Path: ".runtime"}
	cfg.Persistence.StateDir = ".state"

	remoteCommand := []string{"uname", "-a"}
	manager := &manager{}
	plan, err := manager.planLaunch(launch.Spec{Manifest: cfg, RemoteCommand: remoteCommand, Options: LaunchOptions{Resume: ResumeModeNo, SSH: true}})
	if err != nil {
		t.Fatalf("plan launch: %v", err)
	}

	remoteCommand[0] = "mutated"
	if got, want := plan.RemoteCommand, []string{"uname", "-a"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("expected copied remote command: got %#v want %#v", got, want)
	}
	if got, want := plan.Paths.ControlSocket, filepath.Join(tmpDir, ".runtime", "virtle.sock"); got != want {
		t.Fatalf("unexpected control socket path: got %q want %q", got, want)
	}
	if got, want := plan.Paths.QMPSocket, filepath.Join(tmpDir, ".runtime", "qmp.sock"); got != want {
		t.Fatalf("unexpected qmp socket path: got %q want %q", got, want)
	}
	if got, want := plan.Paths.StateDir, filepath.Join(tmpDir, ".state"); got != want {
		t.Fatalf("unexpected state dir: got %q want %q", got, want)
	}

}

func TestManagerPlanUsesDefaultConfig(t *testing.T) {
	tmpDir := t.TempDir()
	cfg := validManifest(tmpDir)
	remoteCommand := []string{"hostname"}

	plan, err := newManagerFromConfig(DefaultConfig()).planLaunch(launch.Spec{
		Manifest:      cfg,
		RemoteCommand: remoteCommand,
		Options:       LaunchOptions{Resume: ResumeModeNo, SSH: true},
	})
	if err != nil {
		t.Fatalf("manager plan: %v", err)
	}

	remoteCommand[0] = "mutated"
	if got, want := plan.RemoteCommand, []string{"hostname"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("expected copied remote command: got %#v want %#v", got, want)
	}
	if got, want := plan.Paths.ControlSocket, filepath.Join(tmpDir, "virtle.sock"); got != want {
		t.Fatalf("unexpected control socket path: got %q want %q", got, want)
	}
}

func TestManagerLaunchSIGTERMSequenceAndTeardownOrder(t *testing.T) {
	tmpDir := t.TempDir()
	cfg := validManifest(tmpDir)
	cfg.Paths.LockPath = filepath.Join(tmpDir, "virtle.lock")
	cfg.Paths.RuntimeDir = manifest.RuntimeDir{Mode: manifest.RuntimeDirPath, Path: ".virtle"}
	cfg.Persistence.Directories = []string{"persist"}
	cfg.QEMU.QMP.SocketPath = "qmp.sock"
	cfg.QEMU.Devices.Block[0].ImagePath = "overlay.img"
	cfg.CleanupFiles = []string{"sock-a", "sock-b", "cleanup.sock"}
	cfg.Volumes = []manifest.Volume{
		{
			ImagePath:  "overlay.img",
			Size:       256,
			FSType:     "ext4",
			AutoCreate: true,
		},
	}
	cfg.Run = []manifest.Run{
		{
			Exec: []string{"/bin/virtiofsd-ro-store"},
			Vars: map[string]any{"Socket": filepath.Join(tmpDir, ".virtle", "sock-a")},
		},
		{
			Exec: []string{"/bin/virtiofsd-workspace"},
			Vars: map[string]any{"Socket": filepath.Join(tmpDir, ".virtle", "sock-b")},
		},
	}
	cfg.QEMU.Devices.VirtioFS = []manifest.QEMUVirtioFSShare{
		{ID: "fs0", SocketPath: "sock-a", Tag: "ro-store", Transport: "pci"},
		{ID: "fs1", SocketPath: "sock-b", Tag: "workspace", Transport: "pci"},
	}
	if err := launch.WriteSuspendStateData(cfg, launch.SuspendState{
		QMPSocketPath: filepath.Join(tmpDir, "old-qmp.sock"),
		Status:        "paused",
	}); err != nil {
		t.Fatalf("write stale suspend state: %v", err)
	}

	volumeImage := filepath.Join(tmpDir, "overlay.img")
	cleanupPath := filepath.Join(tmpDir, ".virtle", "cleanup.sock")
	untouchedPath := filepath.Join(tmpDir, ".virtle", "external.sock")
	if err := os.MkdirAll(filepath.Dir(cleanupPath), 0o755); err != nil {
		t.Fatalf("create cleanup directory: %v", err)
	}
	createStaleUnixSocket(t, cleanupPath)
	if err := os.WriteFile(untouchedPath, []byte("external"), 0o600); err != nil {
		t.Fatalf("write external path: %v", err)
	}

	signalCh := make(chan os.Signal, 1)
	runner := &launchRunner{cancel: func() {
		signalCh <- syscall.SIGTERM
	}}
	qmpClient := &fakeQMPClient{
		onQuit: func() {
			runner.exitQEMU(nil)
		},
	}
	waiter := &fakeSocketWaiter{
		callback: func(paths []string) error {
			for _, path := range paths {
				if err := createStaleUnixSocketPath(path); err != nil {
					return err
				}
			}
			return nil
		},
	}

	var logOutput bytes.Buffer
	manager := &manager{
		locker:            &fileLocker{},
		runner:            runner,
		socketWaiter:      waiter,
		qmpDialer:         &fakeQMPDialer{client: qmpClient},
		logger:            slog.New(slog.NewTextHandler(&logOutput, nil)),
		logWriter:         &logOutput,
		sshRetryDelay:     0,
		shutdownDelay:     10 * time.Millisecond,
		qmpRetryDelay:     0,
		qmpConnectTimeout: time.Millisecond,
		qmpQuitTimeout:    time.Millisecond,
		signals:           signalCh,
	}

	err := manager.launch(context.Background(), cfg, nil)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context cancellation, got %v", err)
	}

	wantStarts := []string{"virtiofsd-ro-store", "virtiofsd-workspace", "qemu-system-x86_64", "ssh"}
	if !reflect.DeepEqual(runner.startedNames(), wantStarts) {
		t.Fatalf("unexpected start order: got %v want %v", runner.startedNames(), wantStarts)
	}

	if !containsString(runner.qemuArgs(), "-qmp") {
		t.Fatalf("expected qemu args to contain qmp socket: %v", runner.qemuArgs())
	}
	if !containsString(runner.qemuArgs(), "unix:"+filepath.Join(tmpDir, ".virtle", "qmp.sock")+",server,nowait") {
		t.Fatalf("expected qemu args to contain resolved qmp socket path: %v", runner.qemuArgs())
	}
	if !containsString(runner.qemuArgs(), "guest-cid=3") {
		t.Fatalf("expected qemu args to contain runtime vsock cid: %v", runner.qemuArgs())
	}
	if !containsString(runner.qemuArgs(), "virtio-blk-pci,drive=vda") {
		t.Fatalf("expected qemu args to contain virtio block device: %v", runner.qemuArgs())
	}
	if !containsString(runner.qemuArgs(), "vhost-user-fs-pci,chardev=char-fs1,tag=workspace") {
		t.Fatalf("expected qemu args to contain virtiofs share: %v", runner.qemuArgs())
	}
	if !containsString(runner.qemuArgs(), "socket,path="+filepath.Join(tmpDir, ".virtle", "ready.sock")+",server=on,wait=off,id=ready_char") {
		t.Fatalf("expected qemu args to contain ssh readiness socket: %v", runner.qemuArgs())
	}
	if !containsString(runner.qemuArgs(), "virtserialport,chardev=ready_char,name=virtle.ready") {
		t.Fatalf("expected qemu args to contain ssh readiness port: %v", runner.qemuArgs())
	}
	if containsString(runner.qemuArgs(), "balloon") {
		t.Fatalf("expected qemu args to omit balloon device when not configured: %v", runner.qemuArgs())
	}

	if got := runner.qemuEnv(); len(got) != 0 {
		t.Fatalf("unexpected qemu env: got %v want no extra env", got)
	}

	if got := len(runner.sshArgs()); got != 1 {
		t.Fatalf("unexpected ssh attempts: got %d want 1", got)
	}
	for i, args := range runner.sshArgs() {
		if !containsString(args, "agent@vsock/3") {
			t.Fatalf("ssh attempt %d missing runtime destination: %v", i, args)
		}
		if containsString(args, "true") {
			t.Fatalf("autoconnect attempt %d unexpectedly used readiness probe: %v", i, args)
		}
	}

	wantSignals := []string{"ssh", "virtiofsd-workspace", "virtiofsd-ro-store"}
	if !reflect.DeepEqual(runner.signalNames(), wantSignals) {
		t.Fatalf("unexpected stop order: got %v want %v", runner.signalNames(), wantSignals)
	}
	if qmpClient.quitCalls != 1 {
		t.Fatalf("expected qmp quit to be used for qemu shutdown, got %d calls", qmpClient.quitCalls)
	}

	if got, want := waiter.calls, 3; got != want {
		t.Fatalf("unexpected waiter calls: got %d want %d", got, want)
	}

	if _, err := os.Stat(filepath.Join(tmpDir, "persist")); err != nil {
		t.Fatalf("expected persistence directory to exist: %v", err)
	}
	if info, err := os.Stat(volumeImage); err != nil {
		t.Fatalf("expected volume image to exist: %v", err)
	} else if got, want := info.Size(), int64(256*1024*1024); got != want {
		t.Fatalf("unexpected volume size: got %d want %d", got, want)
	}
	if _, err := os.Stat(launch.SuspendStatePath(cfg)); !os.IsNotExist(err) {
		t.Fatalf("expected launch to clear stale suspend state, stat err: %v", err)
	}
	if _, err := os.Stat(cleanupPath); !os.IsNotExist(err) {
		t.Fatalf("expected cleanup file to be removed, stat err: %v", err)
	}
	if _, err := os.Stat(untouchedPath); err != nil {
		t.Fatalf("expected unlisted path to remain: %v", err)
	}
	logs := logOutput.String()
	for _, want := range []string{
		"creating volume image",
		"path=" + volumeImage,
		"size_mib=256",
		"fs_type=ext4",
	} {
		if !strings.Contains(logs, want) {
			t.Fatalf("expected volume creation log to contain %q, got %q", want, logs)
		}
	}
	assertLaunchStatsLog(t, logs, []string{
		"started_to_boot=",
		"boot_to_qmp=",
		"files_to_first_ssh=",
		"files_to_ssh=",
		"boot_to_ssh=",
		"ssh_to_completed=",
		"total=",
		"ssh_attempts=1",
	}, []string{
		"boot_to_completed=",
		"qmp_to_guest_agent=",
		"guest_agent_to_files=",
	})
}

func TestManagerLaunchStartsRunCommands(t *testing.T) {
	tmpDir := t.TempDir()
	cfg := validManifest(tmpDir)
	cfg.Paths.LockPath = filepath.Join(tmpDir, "virtle.lock")
	cfg.Persistence.StateDir = ".virtle"
	cfg.QEMU.QMP.SocketPath = "qmp.sock"
	cfg.Volumes[0].AutoCreate = false
	cfg.Workspace = manifest.Workspace{GuestDir: "/home/agent/workspace"}
	cfg.Run = append(cfg.Run, manifest.Run{
		Exec: []string{"/bin/proxy", "--workspace={{.Workspace.GuestPath}}", "--cid={{.CID}}", "--name={{.Name}}"},
		Vars: map[string]any{"Name": "notifications"},
	})

	var eventsMu sync.Mutex
	var events []string
	recordEvent := func(event string) {
		eventsMu.Lock()
		defer eventsMu.Unlock()
		events = append(events, event)
	}

	runner := &launchRunner{
		finishInteractiveSSH: true,
		onStart: func(name string, cmd *exec.Cmd) {
			recordEvent("start:" + name)
		},
	}
	qmpClient := &fakeQMPClient{
		onQuit: func() {
			runner.exitQEMU(nil)
		},
	}
	waiter := &fakeSocketWaiter{
		callback: func(paths []string) error {
			if len(paths) > 0 {
				recordEvent("wait:" + filepath.Base(paths[0]))
			}
			return nil
		},
	}
	var logOutput bytes.Buffer
	manager := &manager{
		locker:            &fileLocker{},
		runner:            runner,
		socketWaiter:      waiter,
		qmpDialer:         &fakeQMPDialer{client: qmpClient},
		sshReadyDialer:    &fakeSSHReadyDialer{},
		logger:            slog.New(slog.NewTextHandler(&logOutput, nil)),
		logWriter:         &logOutput,
		shutdownDelay:     10 * time.Millisecond,
		qmpRetryDelay:     0,
		qmpConnectTimeout: time.Millisecond,
		qmpQuitTimeout:    time.Millisecond,
	}

	if err := manager.launchWithOptions(context.Background(), cfg, nil, LaunchOptions{Resume: ResumeModeNo, SSH: true}); err != nil {
		t.Fatalf("launch with run: %v", err)
	}

	runName := "proxy"
	if !containsString(runner.startedNames(), runName) {
		t.Fatalf("expected run process start in %v", runner.startedNames())
	}
	if got, want := runner.startedNames(), []string{"virtiofsd-workspace", runName, "qemu-system-x86_64", "ssh"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("unexpected start order: got %v want %v", got, want)
	}
	if got, want := runner.processDirs()[runName], tmpDir; got != want {
		t.Fatalf("unexpected run working directory: got %q want %q", got, want)
	}
	if got, want := runner.runArgs()[runName], []string{"--workspace=/home/agent/workspace", "--cid=3", "--name=notifications"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("unexpected run args: got %#v want %#v", got, want)
	}
	for _, want := range []string{
		"CID=3",
		"NAME=notifications",
	} {
		if !containsString(runner.runEnv()[runName], want) {
			t.Fatalf("expected run env %q in %#v", want, runner.runEnv()[runName])
		}
	}
	for _, entry := range runner.runEnv()[runName] {
		if strings.HasPrefix(entry, "WORKSPACE=") {
			t.Fatalf("structured workspace should not produce scalar env in %#v", runner.runEnv()[runName])
		}
	}
	wantSocketWaits := [][]string{
		{filepath.Join(tmpDir, "fs.sock")},
		{filepath.Join(tmpDir, "qmp.sock")},
	}
	if len(waiter.paths) < len(wantSocketWaits) {
		t.Fatalf("expected at least %d socket waits, got %v", len(wantSocketWaits), waiter.paths)
	}
	if got := waiter.paths[:len(wantSocketWaits)]; !reflect.DeepEqual(got, wantSocketWaits) {
		t.Fatalf("unexpected initial socket waits: got %v want %v", got, wantSocketWaits)
	}
	eventsMu.Lock()
	gotEvents := append([]string(nil), events...)
	eventsMu.Unlock()
	wantEvents := []string{
		"start:virtiofsd-workspace",
		"start:" + runName,
		"wait:fs.sock",
		"start:qemu-system-x86_64",
		"wait:qmp.sock",
		"start:ssh",
	}
	if !reflect.DeepEqual(gotEvents, wantEvents) {
		t.Fatalf("unexpected launch events: got %v want %v", gotEvents, wantEvents)
	}
}

func TestManagerLaunchFailsWhenRunStartFails(t *testing.T) {
	tmpDir := t.TempDir()
	cfg := validManifest(tmpDir)
	cfg.Paths.LockPath = filepath.Join(tmpDir, "virtle.lock")
	cfg.Paths.RuntimeDir = manifest.RuntimeDir{Mode: manifest.RuntimeDirPath, Path: ".virtle"}
	cfg.Persistence.StateDir = ".virtle"
	cfg.Volumes[0].AutoCreate = false
	cfg.QEMU.Devices.VirtioFS = nil
	cfg.CleanupFiles = []string{"cleanup.sock"}
	cfg.Run = []manifest.Run{
		{
			Exec: []string{"/bin/proxy"},
		},
	}
	cleanupPath := filepath.Join(tmpDir, ".virtle", "cleanup.sock")
	createStaleUnixSocket(t, cleanupPath)

	runner := &launchRunner{
		startErrors: map[string]error{
			"proxy": errors.New("proxy start failed"),
		},
	}
	var logOutput bytes.Buffer
	manager := &manager{
		locker:        &fileLocker{},
		runner:        runner,
		socketWaiter:  &fakeSocketWaiter{},
		logger:        slog.New(slog.NewTextHandler(&logOutput, nil)),
		logWriter:     &logOutput,
		shutdownDelay: 10 * time.Millisecond,
	}

	err := manager.launchWithOptions(context.Background(), cfg, nil, LaunchOptions{Resume: ResumeModeNo, SSH: true})
	if err == nil || !strings.Contains(err.Error(), "run startup") {
		t.Fatalf("expected run startup error, got %v", err)
	}
	if !strings.Contains(err.Error(), "proxy start failed") {
		t.Fatalf("expected run process error, got %v", err)
	}
	if containsString(runner.startedNames(), "qemu-system-x86_64") {
		t.Fatalf("expected qemu not to start, got starts %v", runner.startedNames())
	}
	if _, err := os.Stat(cleanupPath); !os.IsNotExist(err) {
		t.Fatalf("expected cleanup file to be removed before startup failure, stat err: %v", err)
	}
	if !strings.Contains(logOutput.String(), "stats:") {
		t.Fatalf("expected normal launch cleanup to emit stats, got %q", logOutput.String())
	}
}

func TestManagerLaunchStopsStartedRunsWhenLaterRunFails(t *testing.T) {
	tmpDir := t.TempDir()
	cfg := validManifest(tmpDir)
	cfg.Paths.LockPath = filepath.Join(tmpDir, "virtle.lock")
	cfg.Persistence.StateDir = ".virtle"
	cfg.Volumes[0].AutoCreate = false
	cfg.QEMU.Devices.VirtioFS = nil
	cfg.Run = []manifest.Run{
		{
			Exec: []string{"/bin/proxy-one"},
		},
		{
			Exec: []string{"/bin/proxy-two"},
		},
	}

	firstName := "proxy-one"
	secondName := "proxy-two"
	runner := &launchRunner{
		startErrors: map[string]error{
			secondName: errors.New("start second run failed"),
		},
	}
	waiter := &fakeSocketWaiter{callback: func(paths []string) error { return nil }}
	var logOutput bytes.Buffer
	manager := &manager{
		locker:        &fileLocker{},
		runner:        runner,
		socketWaiter:  waiter,
		logger:        slog.New(slog.NewTextHandler(&logOutput, nil)),
		logWriter:     &logOutput,
		shutdownDelay: 10 * time.Millisecond,
	}

	err := manager.launchWithOptions(context.Background(), cfg, nil, LaunchOptions{Resume: ResumeModeNo, SSH: true})
	if err == nil || !strings.Contains(err.Error(), "run startup") || !strings.Contains(err.Error(), "start second run failed") {
		t.Fatalf("expected second run startup error, got %v", err)
	}
	if got, want := runner.startedNames(), []string{firstName, secondName}; !reflect.DeepEqual(got, want) {
		t.Fatalf("unexpected start order: got %v want %v", got, want)
	}
	if got, want := runner.signalNames(), []string{firstName}; !reflect.DeepEqual(got, want) {
		t.Fatalf("unexpected cleanup signals: got %v want %v", got, want)
	}
	if waiter.calls != 0 {
		t.Fatalf("expected socket wait to be skipped after startup failure, got %d calls", waiter.calls)
	}
	if containsString(runner.startedNames(), "qemu-system-x86_64") {
		t.Fatalf("expected qemu not to start, got starts %v", runner.startedNames())
	}
	if containsString(runner.startedNames(), "virtiofsd-workspace") {
		t.Fatalf("expected virtiofsd not to start after tunnel failure, got starts %v", runner.startedNames())
	}
}

func TestManagerLaunchRemovesCleanupPathAfterQMPStartupFailure(t *testing.T) {
	tmpDir := t.TempDir()
	cfg := validManifest(tmpDir)
	cfg.Paths.LockPath = filepath.Join(tmpDir, "virtle.lock")
	cfg.Volumes[0].AutoCreate = false

	cleanupPath := filepath.Join(tmpDir, "fs.sock")
	waiter := &fakeSocketWaiter{
		callback: func(paths []string) error {
			if len(paths) == 1 && paths[0] == cleanupPath {
				if err := createStaleUnixSocketPath(cleanupPath); err != nil {
					return err
				}
				return nil
			}
			return errors.New("qmp did not start")
		},
	}
	runner := &launchRunner{}
	var logOutput bytes.Buffer
	manager := &manager{
		locker:            &fileLocker{},
		runner:            runner,
		socketWaiter:      waiter,
		qmpDialer:         &fakeQMPDialer{},
		logger:            slog.New(slog.NewTextHandler(&logOutput, nil)),
		logWriter:         &logOutput,
		shutdownDelay:     10 * time.Millisecond,
		qmpRetryDelay:     0,
		qmpConnectTimeout: time.Millisecond,
	}

	err := manager.launchWithOptions(context.Background(), cfg, nil, LaunchOptions{Resume: ResumeModeNo, SSH: true})
	if err == nil || !strings.Contains(err.Error(), "qmp did not start") {
		t.Fatalf("expected qmp startup error, got %v", err)
	}
	if got, want := runner.startedNames(), []string{"virtiofsd-workspace", "qemu-system-x86_64"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("unexpected start order: got %v want %v", got, want)
	}
	if got, want := runner.signalNames(), []string{"qemu-system-x86_64", "virtiofsd-workspace"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("unexpected cleanup signals: got %v want %v", got, want)
	}
	if _, err := os.Stat(cleanupPath); !os.IsNotExist(err) {
		t.Fatalf("expected cleanup file to be removed after qmp failure, stat err: %v", err)
	}
}

func TestRemoveStaleSocketsIgnoresMissing(t *testing.T) {
	tmpDir := t.TempDir()
	filePath := filepath.Join(tmpDir, "cleanup.sock")
	missingPath := filepath.Join(tmpDir, "missing.sock")
	createStaleUnixSocket(t, filePath)

	if err := launch.RemoveStaleSockets(filePath, missingPath); err != nil {
		t.Fatalf("remove socket paths: %v", err)
	}
	if _, err := os.Stat(filePath); !os.IsNotExist(err) {
		t.Fatalf("expected cleanup file to be removed, stat err: %v", err)
	}
}

func TestCreateVolumeImageCreatesNativeExt4(t *testing.T) {
	label := "persist"
	for _, tt := range []struct {
		name      string
		sizeMiB   units.MiB
		label     string
		wantLabel string
	}{
		{name: "minimum-size-without-label", sizeMiB: 256},
		{name: "minimum-size-with-label", sizeMiB: 256, label: label, wantLabel: label},
		{name: "default-home-size", sizeMiB: 4096, label: label, wantLabel: label},
	} {
		t.Run(tt.name, func(t *testing.T) {
			tmpDir := t.TempDir()
			imagePath := filepath.Join(tmpDir, "volume.img")

			err := launch.CreateVolumeImage(manifest.Volume{
				ImagePath:  imagePath,
				Size:       tt.sizeMiB,
				FSType:     "ext4",
				AutoCreate: true,
				Label:      tt.label,
			})
			if err != nil {
				t.Fatalf("create volume image: %v", err)
			}

			if info, err := os.Stat(imagePath); err != nil {
				t.Fatalf("expected volume image to exist: %v", err)
			} else if got, want := info.Size(), tt.sizeMiB.Bytes(); got != want {
				t.Fatalf("unexpected volume size: got %d want %d", got, want)
			}

			image, err := diskfs.Open(imagePath, diskfs.WithOpenMode(diskfs.ReadOnly))
			if err != nil {
				t.Fatalf("open generated image: %v", err)
			}
			defer image.Close()

			fs, err := image.GetFilesystem(0)
			if err != nil {
				t.Fatalf("read generated filesystem: %v", err)
			}
			if got, want := fs.Type(), filesystem.TypeExt4; got != want {
				t.Fatalf("unexpected filesystem type: got %v want %v", got, want)
			}
			if got := strings.TrimSpace(fs.Label()); got != tt.wantLabel {
				t.Fatalf("unexpected filesystem label: got %q want %q", got, tt.wantLabel)
			}
		})
	}
}

func TestCreateVolumeImageRunsChattrBeforeSizingImage(t *testing.T) {
	tmpDir := t.TempDir()
	binDir := filepath.Join(tmpDir, "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatalf("create fake bin dir: %v", err)
	}
	chattrLog := filepath.Join(tmpDir, "chattr-size.log")
	chattrPath := filepath.Join(binDir, "chattr")
	if err := os.WriteFile(chattrPath, []byte("#!/usr/bin/env sh\nset -eu\nstat -c '%s' \"$2\" > \"$CHATTR_LOG\"\n"), 0o755); err != nil {
		t.Fatalf("write fake chattr tool: %v", err)
	}
	t.Setenv("CHATTR_LOG", chattrLog)
	t.Setenv("PATH", binDir+":"+os.Getenv("PATH"))

	imagePath := filepath.Join(tmpDir, "volume.img")
	if err := launch.CreateVolumeImage(manifest.Volume{
		ImagePath:  imagePath,
		Size:       256,
		FSType:     "ext4",
		AutoCreate: true,
	}); err != nil {
		t.Fatalf("create volume image: %v", err)
	}

	data, err := os.ReadFile(chattrLog)
	if err != nil {
		t.Fatalf("read chattr log: %v", err)
	}
	if got, want := strings.TrimSpace(string(data)), "0"; got != want {
		t.Fatalf("expected chattr to run before image sizing: got size %q want %q", got, want)
	}
}

func TestManagerLaunchWithoutSSHPrintsConnectHintAndWaitsForQEMU(t *testing.T) {
	tmpDir := t.TempDir()
	cfg := validManifest(tmpDir)
	cfg.Paths.LockPath = filepath.Join(tmpDir, "virtle.lock")
	cfg.QEMU.QMP.SocketPath = "qmp.sock"
	cfg.Volumes[0].AutoCreate = false

	runner := &launchRunner{}
	qmpClient := &fakeQMPClient{}
	var logOutput bytes.Buffer
	manager := &manager{
		locker:            &fileLocker{},
		runner:            runner,
		socketWaiter:      &fakeSocketWaiter{callback: func(paths []string) error { return nil }},
		qmpDialer:         &fakeQMPDialer{client: qmpClient},
		logger:            slog.New(slog.NewTextHandler(&logOutput, nil)),
		logWriter:         &logOutput,
		sshRetryDelay:     0,
		shutdownDelay:     10 * time.Millisecond,
		qmpRetryDelay:     0,
		qmpConnectTimeout: time.Millisecond,
		qmpQuitTimeout:    time.Millisecond,
	}

	exitReadyQEMU := make(chan struct{})
	go func() {
		defer close(exitReadyQEMU)
		time.Sleep(10 * time.Millisecond)
		runner.exitQEMU(nil)
	}()

	if err := manager.launchWithOptions(context.Background(), cfg, nil, LaunchOptions{Resume: ResumeModeNo}); err != nil {
		t.Fatalf("launch without ssh: %v", err)
	}
	<-exitReadyQEMU

	wantStarts := []string{"virtiofsd-workspace", "qemu-system-x86_64"}
	if !reflect.DeepEqual(runner.startedNames(), wantStarts) {
		t.Fatalf("unexpected start order: got %v want %v", runner.startedNames(), wantStarts)
	}
	if got := len(runner.sshArgs()); got != 0 {
		t.Fatalf("expected no ssh starts, got %d", got)
	}
	if strings.Contains(logOutput.String(), "msg=\"ssh command\"") {
		t.Fatalf("unexpected interactive ssh command log, got %q", logOutput.String())
	}
	if !strings.Contains(logOutput.String(), "connect with: /bin/ssh agent@vsock/3") {
		t.Fatalf("expected out-of-band ssh hint, got %q", logOutput.String())
	}
	assertLaunchStatsLog(t, logOutput.String(), []string{
		"started_to_boot=",
		"boot_to_qmp=",
		"files_to_ssh=",
		"boot_to_ssh=",
		"boot_to_completed=",
		"total=",
	}, []string{
		"ssh_to_completed=",
		"ssh_attempts=",
	})
	if qmpClient.quitCalls != 0 {
		t.Fatalf("expected natural qemu exit without qmp quit, got %d calls", qmpClient.quitCalls)
	}
}

func TestManagerStartAndWaitForRunningLaunchWithoutSSH(t *testing.T) {
	tmpDir := t.TempDir()
	cfg := validManifest(tmpDir)
	cfg.Paths.LockPath = filepath.Join(tmpDir, "virtle.lock")
	cfg.QEMU.QMP.SocketPath = "qmp.sock"
	cfg.Volumes[0].AutoCreate = false

	runner := &launchRunner{}
	qmpClient := &fakeQMPClient{}
	var logOutput bytes.Buffer
	manager := newManagerFromConfig(Config{
		Locker:            &fileLocker{},
		Runner:            runner,
		SocketWaiter:      &fakeSocketWaiter{callback: func(paths []string) error { return nil }},
		QMPDialer:         &fakeQMPDialer{client: qmpClient},
		Logger:            slog.New(slog.NewTextHandler(&logOutput, nil)),
		LogWriter:         &logOutput,
		ShutdownDelay:     10 * time.Millisecond,
		QMPRetryDelay:     0,
		QMPConnectTimeout: time.Millisecond,
		QMPQuitTimeout:    time.Millisecond,
	})

	plan, err := manager.planLaunch(launch.Spec{
		Manifest: cfg,
		Options:  LaunchOptions{Resume: ResumeModeNo, SSH: false},
	})
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	running, err := manager.startWithPlan(context.Background(), plan)
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	defer func() {
		if err := running.Close(); err != nil {
			t.Errorf("runtime close: %v", err)
		}
	}()

	exitReadyQEMU := make(chan struct{})
	go func() {
		defer close(exitReadyQEMU)
		time.Sleep(10 * time.Millisecond)
		runner.exitQEMU(nil)
	}()

	if err := manager.waitForRunningLaunch(context.Background(), running, WaitVM); err != nil {
		t.Fatalf("wait: %v", err)
	}
	<-exitReadyQEMU

	if got, want := runner.startedNames(), []string{"virtiofsd-workspace", "qemu-system-x86_64"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("unexpected start order: got %v want %v", got, want)
	}
	if got := len(runner.sshArgs()); got != 0 {
		t.Fatalf("expected no ssh starts, got %d", got)
	}
	if !strings.Contains(logOutput.String(), "connect with: /bin/ssh agent@vsock/") {
		t.Fatalf("expected out-of-band ssh hint, got %q", logOutput.String())
	}
}

func TestWaitForRunningLaunchNilRunningReturnsStageError(t *testing.T) {
	manager := &manager{}

	err := manager.waitForRunningLaunch(context.Background(), nil, WaitVM)

	var stageErr *launch.StageError
	if !errors.As(err, &stageErr) {
		t.Fatalf("error type: got %T", err)
	}
	if stageErr.Stage != "runtime wait" {
		t.Fatalf("stage: got %q want runtime wait", stageErr.Stage)
	}
}

func TestWaitForRunningLaunchWaitModeOverrideEnablesSSH(t *testing.T) {
	tmpDir := t.TempDir()
	cfg := validManifest(tmpDir)
	runner := &launchRunner{finishInteractiveSSH: true}
	manager := &manager{
		runner:            runner,
		logger:            slog.New(slog.DiscardHandler),
		shutdownDelay:     time.Millisecond,
		qmpConnectTimeout: time.Second,
	}
	processes := launch.NewProcessSet()
	plan := &launch.Plan{
		Manifest: cfg,
		Options:  LaunchOptions{SSH: false},
		CID:      3,
	}
	running := &runningLaunch{
		plan:           plan,
		stats:          launch.NewStats(time.Now()),
		qmp:            &fakeQMPClient{},
		lifecycle:      newTestLaunchLifecycle(),
		suspendHandler: fakeSuspendHandler{},
		processes:      processes,
	}

	if err := manager.waitForRunningLaunch(context.Background(), running, WaitSSH); err != nil {
		t.Fatalf("wait: %v", err)
	}
	if plan.Options.SSH {
		t.Fatal("original plan SSH option was mutated")
	}
	if got, want := len(runner.sshArgs()), 1; got != want {
		t.Fatalf("ssh starts: got %d want %d", got, want)
	}
}

func TestWaitForRunningLaunchSavedSuspendSkipsCloseWriteBack(t *testing.T) {
	tmpDir := t.TempDir()
	cfg := validManifest(tmpDir)
	stats := launch.NewStats(time.Now())
	processes := launch.NewProcessSet()
	processes.SetQEMU((&executortest.Process{OverrideName: "qemu-system-x86_64"}).Process())
	var writeBackCalls int
	runtime := runtimepkg.New(runtimepkg.RuntimeConfig{
		Manifest:      cfg,
		Stats:         stats,
		QMP:           &fakeQMPClient{},
		Processes:     processes,
		ShutdownDelay: time.Millisecond,
		WriteBack: func(context.Context) error {
			writeBackCalls++
			return nil
		},
		QMPTimeout: time.Second,
		Logger:     slog.New(slog.DiscardHandler),
	})
	lifecycle := newTestLaunchLifecycle()
	lifecycle.Suspend().Request()
	running := &runningLaunch{
		runtime:        runtime,
		plan:           &launch.Plan{Manifest: cfg, Options: LaunchOptions{SSH: false}},
		stats:          stats,
		qmp:            &fakeQMPClient{},
		lifecycle:      lifecycle,
		suspendHandler: fakeSuspendHandler{err: launch.ErrSavedSuspendExit},
		processes:      processes,
	}

	if err := (&manager{}).waitForRunningLaunch(context.Background(), running, WaitVM); !errors.Is(err, launch.ErrSavedSuspendExit) {
		t.Fatalf("wait error: got %v want %v", err, launch.ErrSavedSuspendExit)
	}
	if err := running.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	if writeBackCalls != 0 {
		t.Fatalf("write-back calls: got %d want 0", writeBackCalls)
	}
}

func TestManagerLaunchRejectsSSHWithoutExec(t *testing.T) {
	tmpDir := t.TempDir()
	cfg := validManifest(tmpDir)
	cfg.Paths.LockPath = filepath.Join(tmpDir, "virtle.lock")
	cfg.QEMU.QMP.SocketPath = "qmp.sock"
	cfg.QEMU.SSHReady.SocketPath = ""
	cfg.Volumes[0].AutoCreate = false
	cfg.SSH.Argv = nil

	runner := &launchRunner{}
	manager := &manager{runner: runner}

	err := manager.launchWithOptions(context.Background(), cfg, nil, LaunchOptions{Resume: ResumeModeNo, SSH: true})
	if err == nil || !strings.Contains(err.Error(), "--ssh requires a non-empty manifest.ssh.exec") {
		t.Fatalf("expected missing ssh exec error, got %v", err)
	}
	if len(runner.startedNames()) != 0 {
		t.Fatalf("expected failure before starting processes, got starts %v", runner.startedNames())
	}
}

func TestManagerLaunchRejectsRemoteCommandWithoutSSHExec(t *testing.T) {
	tmpDir := t.TempDir()
	cfg := validManifest(tmpDir)
	cfg.Paths.LockPath = filepath.Join(tmpDir, "virtle.lock")
	cfg.QEMU.QMP.SocketPath = "qmp.sock"
	cfg.QEMU.SSHReady.SocketPath = ""
	cfg.Volumes[0].AutoCreate = false
	cfg.SSH.Argv = nil

	runner := &launchRunner{}
	qmpClient := &fakeQMPClient{
		onQuit: func() {
			runner.exitQEMU(nil)
		},
	}
	var logOutput bytes.Buffer
	manager := &manager{
		locker:            &fileLocker{},
		runner:            runner,
		socketWaiter:      &fakeSocketWaiter{callback: func(paths []string) error { return nil }},
		qmpDialer:         &fakeQMPDialer{client: qmpClient},
		logger:            slog.New(slog.NewTextHandler(&logOutput, nil)),
		logWriter:         &logOutput,
		shutdownDelay:     10 * time.Millisecond,
		qmpRetryDelay:     0,
		qmpConnectTimeout: time.Millisecond,
		qmpQuitTimeout:    time.Millisecond,
	}

	err := manager.launchWithOptions(context.Background(), cfg, []string{"echo", "hi"}, LaunchOptions{Resume: ResumeModeNo, SSH: true})
	if err == nil || !strings.Contains(err.Error(), "--ssh requires a non-empty manifest.ssh.exec") {
		t.Fatalf("expected missing ssh argv error, got %v", err)
	}
	if len(runner.startedNames()) != 0 {
		t.Fatalf("expected failure before starting processes, got starts %v", runner.startedNames())
	}
	if got := len(runner.sshArgs()); got != 0 {
		t.Fatalf("expected no ssh starts, got %d", got)
	}
}

func TestManagerLaunchStartsSSHOnceAfterReadiness(t *testing.T) {
	tmpDir := t.TempDir()
	cfg := validManifest(tmpDir)
	cfg.Paths.LockPath = filepath.Join(tmpDir, "virtle.lock")
	cfg.QEMU.QMP.SocketPath = "qmp.sock"
	cfg.Volumes[0].AutoCreate = false

	runner := &launchRunner{
		finishInteractiveSSH:      true,
		finishInteractiveSSHDelay: 2 * defaultSocketPollInterval,
	}
	qmpClient := &fakeQMPClient{
		onQuit: func() {
			runner.exitQEMU(nil)
		},
	}
	var logOutput bytes.Buffer
	manager := &manager{
		locker:            &fileLocker{},
		runner:            runner,
		socketWaiter:      &fakeSocketWaiter{callback: func(paths []string) error { return nil }},
		qmpDialer:         &fakeQMPDialer{client: qmpClient},
		sshReadyDialer:    &fakeSSHReadyDialer{},
		logger:            slog.New(slog.NewTextHandler(&logOutput, nil)),
		logWriter:         &logOutput,
		shutdownDelay:     10 * time.Millisecond,
		qmpRetryDelay:     0,
		qmpConnectTimeout: time.Millisecond,
		qmpQuitTimeout:    time.Millisecond,
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	if err := manager.launchWithOptions(ctx, cfg, nil, LaunchOptions{Resume: ResumeModeNo, SSH: true}); err != nil {
		t.Fatalf("launch with ssh: %v", err)
	}
	if got, want := len(runner.sshArgs()), 1; got != want {
		t.Fatalf("unexpected ssh starts: got %d want %d", got, want)
	}
	for i, args := range runner.sshArgs() {
		if containsString(args, "true") {
			t.Fatalf("autoconnect retry %d unexpectedly used readiness probe: %v", i, args)
		}
	}
	logs := logOutput.String()
	if !strings.Contains(logs, "waiting for ssh readiness") {
		t.Fatalf("expected ssh readiness log, got %q", logs)
	}
	if !strings.Contains(logs, "msg=\"ssh command\"") {
		t.Fatalf("expected per-attempt ssh command log, got %q", logs)
	}
	assertLaunchStatsLog(t, logs, []string{
		"ssh_attempts=1",
		"boot_to_ssh=",
	}, []string{})
}

func TestManagerLaunchPacesSSHRetriesAndWarnsAfterFiveFailures(t *testing.T) {
	tmpDir := t.TempDir()
	cfg := validManifest(tmpDir)
	cfg.Paths.LockPath = filepath.Join(tmpDir, "virtle.lock")
	cfg.Volumes[0].AutoCreate = false
	cfg.SSH.RetryDelay = 10 * time.Millisecond

	var sshStarts []time.Time
	runner := &launchRunner{
		transientSSHFailures: 5,
		finishInteractiveSSH: true,
		onStart: func(name string, _ *exec.Cmd) {
			if name == "ssh" {
				sshStarts = append(sshStarts, time.Now())
			}
		},
	}
	qmpClient := &fakeQMPClient{
		onQuit: func() {
			runner.exitQEMU(nil)
		},
	}
	var logOutput bytes.Buffer
	manager := &manager{
		locker:            &fileLocker{},
		runner:            runner,
		socketWaiter:      &fakeSocketWaiter{callback: func(paths []string) error { return nil }},
		qmpDialer:         &fakeQMPDialer{client: qmpClient},
		sshReadyDialer:    &fakeSSHReadyDialer{},
		logger:            slog.New(slog.NewTextHandler(&logOutput, nil)),
		logWriter:         &logOutput,
		sshRetryDelay:     0,
		shutdownDelay:     10 * time.Millisecond,
		qmpRetryDelay:     0,
		qmpConnectTimeout: time.Millisecond,
		qmpQuitTimeout:    time.Millisecond,
	}

	if err := manager.launchWithOptions(context.Background(), cfg, nil, LaunchOptions{Resume: ResumeModeNo, SSH: true}); err != nil {
		t.Fatalf("launch with ssh retries: %v", err)
	}
	if got, want := len(runner.sshArgs()), 6; got != want {
		t.Fatalf("unexpected ssh starts: got %d want %d", got, want)
	}
	for i := 1; i < len(sshStarts); i++ {
		if got, want := sshStarts[i].Sub(sshStarts[i-1]), cfg.SSH.RetryDelay; got < want {
			t.Fatalf("ssh retry %d delay: got %s want at least %s", i, got, want)
		}
	}
	logs := logOutput.String()
	if !strings.Contains(logs, "ssh exec failed 5 times") {
		t.Fatalf("expected five-retry warning, got %q", logs)
	}
	if !strings.Contains(logs, "ssh_failures=5") {
		t.Fatalf("expected ssh failure count, got %q", logs)
	}
}

func TestWaitForSSHReadyFailsWhenQEMUExitsFirst(t *testing.T) {
	qemuProcess := &executortest.Process{OverrideName: "qemu"}
	qemu := qemuProcess.Process()
	waiterStarted := make(chan struct{})
	manager := &manager{
		socketWaiter: &fakeSocketWaiter{
			noAutoSSHReady: true,
			callback: func(paths []string) error {
				close(waiterStarted)
				<-time.After(5 * defaultSocketPollInterval)
				return nil
			},
		},
		sshReadyDialer:  &fakeSSHReadyDialer{},
		sshReadyTimeout: time.Second,
	}

	go func() {
		<-waiterStarted
		qemuProcess.Complete(errors.New("qemu failed"))
	}()

	err := manager.waitForSSHReady(context.Background(), "/tmp/ready.sock", executor.NewGroup(qemu))
	if err == nil || !strings.Contains(err.Error(), "vm startup") || !strings.Contains(err.Error(), "qemu failed") {
		t.Fatalf("expected qemu exit startup error, got %v", err)
	}
}

func TestWaitForSSHReadyFailsOnTimeout(t *testing.T) {
	manager := &manager{
		socketWaiter:    &fakeSocketWaiter{callback: func(paths []string) error { return nil }},
		sshReadyDialer:  &fakeSSHReadyDialer{block: true},
		sshReadyTimeout: 10 * time.Millisecond,
	}

	err := manager.waitForSSHReady(context.Background(), "/tmp/ready.sock", executor.Group{})
	if err == nil || !strings.Contains(err.Error(), "wait for ssh readiness") || !strings.Contains(err.Error(), "context deadline exceeded") {
		t.Fatalf("expected readiness timeout error, got %v", err)
	}
}

func TestWaitForSocketsReportsConfiguredStage(t *testing.T) {
	manager := &manager{
		socketWaiter: &fakeSocketWaiter{
			callback: func(paths []string) error {
				return errors.New("socket unavailable")
			},
		},
	}

	err := manager.waitForSockets(context.Background(), "virtiofs startup", []string{"/tmp/fs.sock"}, executor.Group{})
	if err == nil || !strings.Contains(err.Error(), "virtiofs startup") || !strings.Contains(err.Error(), "socket unavailable") {
		t.Fatalf("expected virtiofs startup socket error, got %v", err)
	}
}

func TestNewManagerUsesSSHReadyTimeoutEnv(t *testing.T) {
	t.Setenv(sshReadyTimeoutEnv, "5m")

	manager := newManager()
	if got, want := manager.sshReadyTimeout, 5*time.Minute; got != want {
		t.Fatalf("unexpected ssh readiness timeout: got %s want %s", got, want)
	}
}

func TestNewManagerIgnoresInvalidSSHReadyTimeoutEnv(t *testing.T) {
	t.Setenv(sshReadyTimeoutEnv, "0")

	manager := newManager()
	if got, want := manager.sshReadyTimeout, defaultSSHReadyTimeout; got != want {
		t.Fatalf("unexpected ssh readiness timeout: got %s want %s", got, want)
	}
}

func TestWaitForSSHReadyRejectsUnexpectedToken(t *testing.T) {
	manager := &manager{
		socketWaiter:    &fakeSocketWaiter{callback: func(paths []string) error { return nil }},
		sshReadyDialer:  &fakeSSHReadyDialer{data: "NOT_READY\n"},
		sshReadyTimeout: time.Second,
	}

	err := manager.waitForSSHReady(context.Background(), "/tmp/ready.sock", executor.Group{})
	if err == nil || !strings.Contains(err.Error(), "unexpected readiness token") || !strings.Contains(err.Error(), "NOT_READY") {
		t.Fatalf("expected unexpected token error, got %v", err)
	}
}

func TestManagerLaunchPrintsGuestInfoOnSIGUSR1(t *testing.T) {
	tmpDir := t.TempDir()
	cfg := validManifest(tmpDir)
	cfg.Paths.LockPath = filepath.Join(tmpDir, "virtle.lock")
	cfg.QEMU.QMP.SocketPath = "qmp.sock"
	cfg.QEMU.GuestAgent.SocketPath = "qga.sock"
	cfg.Volumes[0].AutoCreate = false

	signalCh := make(chan os.Signal, 8)
	var logOutput bytes.Buffer
	runner := &launchRunner{}
	qmpClient := &fakeQMPClient{
		onQuit: func() {
			runner.exitQEMU(nil)
		},
	}
	guestAgent := &fakeGuestAgentClient{
		execStatuses: []qga.ExecStatus{{
			Exited:  true,
			OutData: "cm9vdCB6c2gKYWdlbnQgdmlydGxlIGxhdW5jaCAtLXNzaApyb290IGluaXQK",
		}},
		record: func(event string) {
			if event == "guest-ps" {
				go runner.exitQEMU(nil)
			}
		},
	}
	manager := &manager{
		locker:              &fileLocker{},
		runner:              runner,
		socketWaiter:        &fakeSocketWaiter{callback: func(paths []string) error { return nil }},
		qmpDialer:           &fakeQMPDialer{client: qmpClient},
		guestAgentDialer:    &fakeGuestAgentDialer{client: guestAgent},
		logger:              slog.New(slog.NewTextHandler(&logOutput, nil)),
		logWriter:           &logOutput,
		sshRetryDelay:       time.Hour,
		shutdownDelay:       10 * time.Millisecond,
		qmpRetryDelay:       0,
		qmpConnectTimeout:   time.Millisecond,
		qmpQuitTimeout:      time.Millisecond,
		qmpMigrationTimeout: time.Second,
		signals:             signalCh,
	}

	go func() {
		signalCh <- syscall.SIGUSR1
	}()

	if err := manager.launchWithOptions(context.Background(), cfg, nil, LaunchOptions{Resume: ResumeModeNo, SSH: false}); err != nil {
		t.Fatalf("launch: %v", err)
	}

	if got, want := len(guestAgent.execs), 1; got != want {
		t.Fatalf("unexpected guest exec count: got %d want %d", got, want)
	}
	exec := guestAgent.execs[0]
	if exec.path != "ps" || !reflect.DeepEqual(exec.args, []string{"-eo", "user=,comm="}) || !exec.captureOutput {
		t.Fatalf("unexpected ps exec: %#v", exec)
	}
	logs := logOutput.String()
	if !strings.Contains(logs, "guest info") || !strings.Contains(logs, "USER COMMAND\nagent virtle\nroot init\nroot zsh\n") {
		t.Fatalf("expected guest process list in logs, got %q", logs)
	}
	if strings.Contains(logs, "--ssh") {
		t.Fatalf("expected guest process list to omit command arguments, got %q", logs)
	}
}

func TestManagerLaunchLogsGuestInfoFailureOnSIGUSR1(t *testing.T) {
	tmpDir := t.TempDir()
	cfg := validManifest(tmpDir)
	cfg.Paths.LockPath = filepath.Join(tmpDir, "virtle.lock")
	cfg.QEMU.QMP.SocketPath = "qmp.sock"
	cfg.QEMU.GuestAgent.SocketPath = "qga.sock"
	cfg.Volumes[0].AutoCreate = false

	signalCh := make(chan os.Signal, 8)
	var logOutput bytes.Buffer
	runner := &launchRunner{}
	qmpClient := &fakeQMPClient{
		onQuit: func() {
			runner.exitQEMU(nil)
		},
	}
	manager := &manager{
		locker:              &fileLocker{},
		runner:              runner,
		socketWaiter:        &fakeSocketWaiter{callback: func(paths []string) error { return nil }},
		qmpDialer:           &fakeQMPDialer{client: qmpClient},
		guestAgentDialer:    &fakeGuestAgentDialer{client: &fakeGuestAgentClient{execErr: errors.New("exec failed")}},
		logger:              slog.New(slog.NewTextHandler(&logOutput, nil)),
		logWriter:           &logOutput,
		sshRetryDelay:       time.Hour,
		shutdownDelay:       10 * time.Millisecond,
		qmpRetryDelay:       0,
		qmpConnectTimeout:   time.Millisecond,
		qmpQuitTimeout:      time.Millisecond,
		qmpMigrationTimeout: time.Second,
		signals:             signalCh,
	}

	go func() {
		signalCh <- syscall.SIGUSR1
		time.Sleep(2 * defaultSocketPollInterval)
		runner.exitQEMU(nil)
	}()

	if err := manager.launchWithOptions(context.Background(), cfg, nil, LaunchOptions{Resume: ResumeModeNo, SSH: false}); err != nil {
		t.Fatalf("launch: %v", err)
	}
	if logs := logOutput.String(); !strings.Contains(logs, "guest info failed") || !strings.Contains(logs, "exec failed") {
		t.Fatalf("expected guest info failure log, got %q", logs)
	}
}

func TestManagerMountsWorkspaceCWD(t *testing.T) {
	tmpDir := t.TempDir()
	cfg := validManifest(tmpDir)
	cfg.Paths.WorkingDir = filepath.Join(tmpDir, "agentspace")
	cfg.Workspace = manifest.Workspace{
		GuestDir: "/home/agent/workspace",
		MountCWD: true,
	}
	guestAgent := &fakeGuestAgentClient{
		execStatuses: []qga.ExecStatus{{Exited: true}, {Exited: true}},
	}
	manager := &manager{
		socketWaiter:      &fakeSocketWaiter{callback: func(paths []string) error { return nil }},
		guestAgentDialer:  &fakeGuestAgentDialer{client: guestAgent},
		logger:            slog.New(slog.DiscardHandler),
		qmpConnectTimeout: time.Second,
	}

	if err := manager.writeGuestFiles(context.Background(), cfg, nil, executor.Group{}); err != nil {
		t.Fatalf("mount workspace cwd: %v", err)
	}

	want := []guestExecCall{
		{
			path:          "install",
			args:          []string{"-d", "/home/agent/workspace", "/home/agent/workspace/agentspace"},
			captureOutput: true,
		},
		{
			path:          "mount",
			args:          []string{"--bind", "/mnt/cwd", "/home/agent/workspace/agentspace"},
			captureOutput: true,
		},
	}
	if !reflect.DeepEqual(guestAgent.execs, want) {
		t.Fatalf("unexpected guest execs: got %#v want %#v", guestAgent.execs, want)
	}
}

func TestManagerLaunchWritesGuestFilesBeforeSSHSession(t *testing.T) {
	tmpDir := t.TempDir()
	cfg := validManifest(tmpDir)
	cfg.Paths.LockPath = filepath.Join(tmpDir, "virtle.lock")
	cfg.QEMU.QMP.SocketPath = "qmp.sock"
	cfg.QEMU.GuestAgent.SocketPath = "qga.sock"
	cfg.Volumes[0].AutoCreate = false

	hostPath := "host-file"
	hostBytes := []byte("from host")
	if err := os.WriteFile(filepath.Join(tmpDir, hostPath), hostBytes, 0o644); err != nil {
		t.Fatalf("write host fixture: %v", err)
	}
	inlineText := "inline"
	inlineChown := "agent:users"
	inlineMode := "0640"
	overwrite := true
	cfg.WriteFiles = manifest.WriteFiles{
		"/etc/virtle/inline":   {Content: manifest.WriteFileContent{Kind: manifest.WriteFileContentText, Text: inlineText}, Chown: inlineChown, Mode: inlineMode, Overwrite: overwrite, FollowLinks: true},
		"/var/lib/virtle/host": {Content: manifest.WriteFileContent{Kind: manifest.WriteFileContentPath, Path: hostPath}, Overwrite: overwrite, FollowLinks: true},
	}

	var eventMu sync.Mutex
	var events []string
	record := func(event string) {
		eventMu.Lock()
		defer eventMu.Unlock()
		events = append(events, event)
	}

	runner := &launchRunner{
		finishInteractiveSSH: true,
		onStart: func(name string, cmd *exec.Cmd) {
			record("start:" + name)
		},
	}
	qmpClient := &fakeQMPClient{
		onQuit: func() {
			runner.exitQEMU(nil)
		},
	}
	guestAgent := &fakeGuestAgentClient{
		record: record,
		execStatuses: []qga.ExecStatus{
			{Exited: true, ExitCode: 1}, // test -d /etc/virtle
			{Exited: true},              // test -d /etc
			{Exited: true},              // install -d /etc/virtle
			{Exited: true},              // chown /etc/virtle/inline
			{Exited: true},              // chmod /etc/virtle/inline
			{Exited: true, ExitCode: 1}, // test -d /var/lib/virtle
			{Exited: true, ExitCode: 1}, // test -d /var/lib
			{Exited: true, ExitCode: 1}, // test -d /var
			{Exited: true},              // test -d /
			{Exited: true},              // install -d /var
			{Exited: true},              // install -d /var/lib
			{Exited: true},              // install -d /var/lib/virtle
		},
	}
	manager := &manager{
		locker:            &fileLocker{},
		runner:            runner,
		socketWaiter:      &fakeSocketWaiter{callback: func(paths []string) error { return nil }},
		qmpDialer:         &fakeQMPDialer{client: qmpClient},
		guestAgentDialer:  &fakeGuestAgentDialer{client: guestAgent},
		sshReadyDialer:    &fakeSSHReadyDialer{record: record},
		logger:            slog.New(slog.DiscardHandler),
		sshRetryDelay:     0,
		shutdownDelay:     10 * time.Millisecond,
		qmpRetryDelay:     0,
		qmpConnectTimeout: 100 * time.Millisecond,
		qmpQuitTimeout:    time.Millisecond,
	}

	if err := manager.launch(context.Background(), cfg, nil); err != nil {
		t.Fatalf("launch: %v", err)
	}

	if got, want := guestAgent.writes["/etc/virtle/inline"], "aW5saW5l"; got != want {
		t.Fatalf("unexpected inline write text: got %q want %q", got, want)
	}
	if got, want := guestAgent.writes["/var/lib/virtle/host"], "ZnJvbSBob3N0"; got != want {
		t.Fatalf("unexpected host write text: got %q want %q", got, want)
	}
	if got, want := guestAgent.execs, []guestExecCall{
		{
			path:          guestTestPath,
			args:          []string{"-d", "/etc/virtle"},
			captureOutput: true,
		},
		{
			path:          guestTestPath,
			args:          []string{"-d", "/etc"},
			captureOutput: true,
		},
		{
			path:          guestInstallPath,
			args:          []string{"-d", "-o", "agent", "-g", "users", "-m", "0750", "/etc/virtle"},
			captureOutput: true,
		},
		{
			path:          guestChownPath,
			args:          []string{"agent:users", "/etc/virtle/inline"},
			captureOutput: true,
		},
		{
			path:          guestChmodPath,
			args:          []string{"0640", "/etc/virtle/inline"},
			captureOutput: true,
		},
		{
			path:          guestTestPath,
			args:          []string{"-d", "/var/lib/virtle"},
			captureOutput: true,
		},
		{
			path:          guestTestPath,
			args:          []string{"-d", "/var/lib"},
			captureOutput: true,
		},
		{
			path:          guestTestPath,
			args:          []string{"-d", "/var"},
			captureOutput: true,
		},
		{
			path:          guestTestPath,
			args:          []string{"-d", "/"},
			captureOutput: true,
		},
		{
			path:          guestInstallPath,
			args:          []string{"-d", "/var"},
			captureOutput: true,
		},
		{
			path:          guestInstallPath,
			args:          []string{"-d", "/var/lib"},
			captureOutput: true,
		},
		{
			path:          guestInstallPath,
			args:          []string{"-d", "/var/lib/virtle"},
			captureOutput: true,
		},
	}; !reflect.DeepEqual(got, want) {
		t.Fatalf("unexpected guest execs: got %#v want %#v", got, want)
	}

	firstSSH := indexString(events, "start:ssh")
	ping := indexString(events, "guest-ping")
	testInline := indexString(events, "guest-test-dir:/etc/virtle")
	installInline := indexString(events, "guest-install-dir:/etc/virtle")
	openInline := indexString(events, "guest-open:/etc/virtle/inline")
	closeInline := indexString(events, "guest-close:/etc/virtle/inline")
	chownInline := indexString(events, "guest-chown:/etc/virtle/inline:agent:users")
	chmodInline := indexString(events, "guest-chmod:/etc/virtle/inline:0640")
	testHost := indexString(events, "guest-test-dir:/var/lib/virtle")
	installHost := indexString(events, "guest-install-dir:/var/lib/virtle")
	openHost := indexString(events, "guest-open:/var/lib/virtle/host")
	closeHost := indexString(events, "guest-close:/var/lib/virtle/host")
	sshReady := indexString(events, "ssh-ready-dial:"+filepath.Join(tmpDir, "ready.sock"))
	if firstSSH < 0 || ping < 0 || testInline < 0 || installInline < 0 || openInline < 0 || closeInline < 0 || chownInline < 0 || chmodInline < 0 || testHost < 0 || installHost < 0 || openHost < 0 || closeHost < 0 || sshReady < 0 {
		t.Fatalf("expected guest agent and ssh events, got %v", events)
	}
	if !(ping < testInline && testInline < installInline && installInline < openInline && openInline < closeInline && closeInline < chownInline && chownInline < chmodInline && chmodInline < testHost && testHost < installHost && installHost < openHost && openHost < closeHost && closeHost < sshReady && sshReady < firstSSH) {
		t.Fatalf("expected guest writes before ssh session, got events %v", events)
	}
}

func TestManagerLaunchWritesBackGuestFilesOnShutdown(t *testing.T) {
	tmpDir := t.TempDir()
	cfg := validManifest(tmpDir)
	cfg.Paths.LockPath = filepath.Join(tmpDir, "virtle.lock")
	cfg.QEMU.QMP.SocketPath = "qmp.sock"
	cfg.QEMU.GuestAgent.SocketPath = "qga.sock"
	cfg.Volumes[0].AutoCreate = false

	hostPath := filepath.Join(tmpDir, "host-file")
	if err := os.WriteFile(hostPath, []byte("from host"), 0o644); err != nil {
		t.Fatalf("write host fixture: %v", err)
	}
	overwrite := true
	writeBack := true
	cfg.WriteFiles = manifest.WriteFiles{
		"/var/lib/virtle/host": {Content: manifest.WriteFileContent{Kind: manifest.WriteFileContentPath, Path: hostPath}, Overwrite: overwrite, FollowLinks: true, WriteBack: writeBack},
	}

	runner := &launchRunner{finishInteractiveSSH: true}
	qmpClient := &fakeQMPClient{
		onQuit: func() {
			runner.exitQEMU(nil)
		},
	}
	guestAgent := &fakeGuestAgentClient{
		readPayloads: map[string][]string{
			"/var/lib/virtle/host": {"ZnJvbSBndWVzdA=="},
		},
		execStatuses: []qga.ExecStatus{{Exited: true}},
	}
	manager := &manager{
		locker:            &fileLocker{},
		runner:            runner,
		socketWaiter:      &fakeSocketWaiter{callback: func(paths []string) error { return nil }},
		qmpDialer:         &fakeQMPDialer{client: qmpClient},
		guestAgentDialer:  &fakeGuestAgentDialer{client: guestAgent},
		sshReadyDialer:    &fakeSSHReadyDialer{},
		logger:            slog.New(slog.DiscardHandler),
		sshRetryDelay:     0,
		shutdownDelay:     10 * time.Millisecond,
		qmpRetryDelay:     0,
		qmpConnectTimeout: 100 * time.Millisecond,
		qmpQuitTimeout:    time.Millisecond,
	}

	if err := manager.launch(context.Background(), cfg, nil); err != nil {
		t.Fatalf("launch: %v", err)
	}

	data, err := os.ReadFile(hostPath)
	if err != nil {
		t.Fatalf("read host file: %v", err)
	}
	if got, want := string(data), "from guest"; got != want {
		t.Fatalf("unexpected write-back content: got %q want %q", got, want)
	}
}

func TestLaunchSuspendHandlerWritesBackGuestFilesBeforeSuspend(t *testing.T) {
	tmpDir := t.TempDir()
	cfg := validManifest(tmpDir)
	cfg.QEMU.QMP.SocketPath = "qmp.sock"
	cfg.QEMU.GuestAgent.SocketPath = "qga.sock"
	hostPath := filepath.Join(tmpDir, "host-file")
	if err := os.WriteFile(hostPath, []byte("from host"), 0o644); err != nil {
		t.Fatalf("write host fixture: %v", err)
	}
	writeBack := true
	cfg.WriteFiles = manifest.WriteFiles{
		"/var/lib/virtle/host": {Content: manifest.WriteFileContent{Kind: manifest.WriteFileContentPath, Path: hostPath}, FollowLinks: true, WriteBack: writeBack},
	}

	guestAgent := &fakeGuestAgentClient{
		readPayloads: map[string][]string{
			"/var/lib/virtle/host": {"ZnJvbSBzdXNwZW5k"},
		},
	}
	qmpClient := &fakeQMPClient{status: "running"}
	manager := &manager{
		guestAgentDialer:    &fakeGuestAgentDialer{client: guestAgent},
		socketWaiter:        &fakeSocketWaiter{callback: func(paths []string) error { return nil }},
		logger:              slog.New(slog.DiscardHandler),
		qmpConnectTimeout:   time.Millisecond,
		qmpMigrationTimeout: time.Second,
	}
	handler := newLaunchSuspendHandler(manager, cfg, filepath.Join(tmpDir, "qmp.sock"), qmpClient, 7, nil, func() bool {
		return true
	})

	if err := handler.saveAndExit(context.Background()); !errors.Is(err, launch.ErrSavedSuspendExit) {
		t.Fatalf("suspend returned %v, want launch.ErrSavedSuspendExit", err)
	}

	data, err := os.ReadFile(hostPath)
	if err != nil {
		t.Fatalf("read host file: %v", err)
	}
	if got, want := string(data), "from suspend"; got != want {
		t.Fatalf("unexpected write-back content: got %q want %q", got, want)
	}
	qmpClient.mu.Lock()
	migrateCalls := qmpClient.migrateCalls
	qmpClient.mu.Unlock()
	if migrateCalls != 1 {
		t.Fatalf("expected suspend migration after write-back, got %d migrate calls", migrateCalls)
	}
}

func TestWriteBackGuestFilesDoesNotReplaceHostOnGuestReadError(t *testing.T) {
	tmpDir := t.TempDir()
	cfg := validManifest(tmpDir)
	cfg.QEMU.GuestAgent.SocketPath = "qga.sock"
	hostPath := filepath.Join(tmpDir, "host-file")
	if err := os.WriteFile(hostPath, []byte("original"), 0o644); err != nil {
		t.Fatalf("write host fixture: %v", err)
	}
	writeBack := true
	cfg.WriteFiles = manifest.WriteFiles{
		"/var/lib/virtle/host": {Content: manifest.WriteFileContent{Kind: manifest.WriteFileContentPath, Path: hostPath}, FollowLinks: true, WriteBack: writeBack},
	}

	guestAgent := &fakeGuestAgentClient{
		readPayloads: map[string][]string{
			"/var/lib/virtle/host": {"not base64"},
		},
	}
	manager := &manager{
		guestAgentDialer:  &fakeGuestAgentDialer{client: guestAgent},
		socketWaiter:      &fakeSocketWaiter{callback: func(paths []string) error { return nil }},
		logger:            slog.New(slog.DiscardHandler),
		qmpConnectTimeout: time.Millisecond,
	}

	err := manager.writeBackGuestFiles(context.Background(), cfg, executor.Group{})
	if err == nil || !strings.Contains(err.Error(), "guest file write-back") {
		t.Fatalf("expected staged write-back error, got %v", err)
	}
	data, readErr := os.ReadFile(hostPath)
	if readErr != nil {
		t.Fatalf("read host file: %v", readErr)
	}
	if got, want := string(data), "original"; got != want {
		t.Fatalf("host file changed after failed write-back: got %q want %q", got, want)
	}
}

func TestWriteBackGuestFilesFollowsHostSymlinkWhenFollowLinksEnabled(t *testing.T) {
	tmpDir := t.TempDir()
	cfg := validManifest(tmpDir)
	cfg.QEMU.GuestAgent.SocketPath = "qga.sock"
	targetPath := filepath.Join(tmpDir, "target-file")
	if err := os.WriteFile(targetPath, []byte("original"), 0o644); err != nil {
		t.Fatalf("write target fixture: %v", err)
	}
	linkPath := filepath.Join(tmpDir, "host-link")
	if err := os.Symlink(targetPath, linkPath); err != nil {
		t.Fatalf("create symlink fixture: %v", err)
	}
	writeBack := true
	cfg.WriteFiles = manifest.WriteFiles{
		"/var/lib/virtle/host": {Content: manifest.WriteFileContent{Kind: manifest.WriteFileContentPath, Path: linkPath}, FollowLinks: true, WriteBack: writeBack},
	}

	guestAgent := &fakeGuestAgentClient{
		readPayloads: map[string][]string{
			"/var/lib/virtle/host": {"ZnJvbSBndWVzdA=="},
		},
	}
	manager := &manager{
		guestAgentDialer:  &fakeGuestAgentDialer{client: guestAgent},
		socketWaiter:      &fakeSocketWaiter{callback: func(paths []string) error { return nil }},
		logger:            slog.New(slog.DiscardHandler),
		qmpConnectTimeout: time.Millisecond,
	}

	if err := manager.writeBackGuestFiles(context.Background(), cfg, executor.Group{}); err != nil {
		t.Fatalf("write back guest files: %v", err)
	}
	targetData, err := os.ReadFile(targetPath)
	if err != nil {
		t.Fatalf("read target file: %v", err)
	}
	if got, want := string(targetData), "from guest"; got != want {
		t.Fatalf("unexpected target content: got %q want %q", got, want)
	}
	info, err := os.Lstat(linkPath)
	if err != nil {
		t.Fatalf("stat symlink: %v", err)
	}
	if info.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("expected write-back path to remain a symlink, got mode %s", info.Mode())
	}
}

func TestManagerLaunchAutoprovisionsSSHKeyAfterAuthFailure(t *testing.T) {
	tmpDir := t.TempDir()
	cfg := validManifest(tmpDir)
	cfg.Paths.LockPath = filepath.Join(tmpDir, "virtle.lock")
	cfg.QEMU.QMP.SocketPath = "qmp.sock"
	cfg.QEMU.GuestAgent.SocketPath = "qga.sock"
	cfg.Volumes[0].AutoCreate = false
	cfg.SSH.Autoprovision = true

	runner := &launchRunner{
		authSSHFailures:           1,
		finishInteractiveSSH:      true,
		finishInteractiveSSHDelay: 2 * defaultSocketPollInterval,
	}
	qmpClient := &fakeQMPClient{
		onQuit: func() {
			runner.exitQEMU(nil)
		},
	}
	guestAgent := &fakeGuestAgentClient{}
	manager := &manager{
		locker:            &fileLocker{},
		runner:            runner,
		socketWaiter:      &fakeSocketWaiter{callback: func(paths []string) error { return nil }},
		qmpDialer:         &fakeQMPDialer{client: qmpClient},
		guestAgentDialer:  &fakeGuestAgentDialer{client: guestAgent},
		logger:            slog.New(slog.DiscardHandler),
		sshRetryDelay:     0,
		shutdownDelay:     10 * time.Millisecond,
		qmpRetryDelay:     0,
		qmpConnectTimeout: 100 * time.Millisecond,
		qmpQuitTimeout:    time.Millisecond,
	}

	if err := manager.launch(context.Background(), cfg, nil); err != nil {
		t.Fatalf("launch: %v", err)
	}

	identityFile := filepath.Join(cfg.ResolvedPersistenceStateDir(), "id_ed25519")
	if got, want := len(runner.sshArgs()), 2; got != want {
		t.Fatalf("unexpected ssh starts: got %d want %d", got, want)
	}
	if containsString(runner.sshArgs()[0], identityFile) {
		t.Fatalf("first ssh attempt unexpectedly used autoprovisioned identity: %v", runner.sshArgs()[0])
	}
	if !containsString(runner.sshArgs()[1], identityFile) || !containsString(runner.sshArgs()[1], "IdentitiesOnly=yes") {
		t.Fatalf("second ssh attempt did not use autoprovisioned identity: %v", runner.sshArgs()[1])
	}
	if info, err := os.Stat(identityFile); err != nil {
		t.Fatalf("stat autoprovisioned identity: %v", err)
	} else if got, want := info.Mode().Perm(), os.FileMode(0o600); got != want {
		t.Fatalf("unexpected identity mode: got %v want %v", got, want)
	}
	if got := guestAgent.writes["/run/virtle-autoprovision-authorized-key.pub"]; got == "" {
		t.Fatalf("expected temporary public key write, got writes %#v", guestAgent.writes)
	}
	if !containsGuestExec(guestAgent.execs, "sh", "/home/agent/.ssh/authorized_keys") {
		t.Fatalf("expected authorized_keys append command, got %#v", guestAgent.execs)
	}
}

func TestManagerLaunchDoesNotAutoprovisionWhenDisabled(t *testing.T) {
	tmpDir := t.TempDir()
	cfg := validManifest(tmpDir)
	cfg.Paths.LockPath = filepath.Join(tmpDir, "virtle.lock")
	cfg.QEMU.QMP.SocketPath = "qmp.sock"
	cfg.QEMU.GuestAgent.SocketPath = "qga.sock"
	cfg.Volumes[0].AutoCreate = false

	runner := &launchRunner{authSSHFailures: 1}
	qmpClient := &fakeQMPClient{
		onQuit: func() {
			runner.exitQEMU(nil)
		},
	}
	guestDialer := &fakeGuestAgentDialer{client: &fakeGuestAgentClient{}}
	manager := &manager{
		locker:            &fileLocker{},
		runner:            runner,
		socketWaiter:      &fakeSocketWaiter{callback: func(paths []string) error { return nil }},
		qmpDialer:         &fakeQMPDialer{client: qmpClient},
		guestAgentDialer:  guestDialer,
		logger:            slog.New(slog.DiscardHandler),
		sshRetryDelay:     0,
		shutdownDelay:     10 * time.Millisecond,
		qmpRetryDelay:     0,
		qmpConnectTimeout: 100 * time.Millisecond,
		qmpQuitTimeout:    time.Millisecond,
	}

	err := manager.launch(context.Background(), cfg, nil)
	if err == nil || !strings.Contains(err.Error(), "active session") {
		t.Fatalf("expected active session auth failure, got %v", err)
	}
	if got, want := len(runner.sshArgs()), 1; got != want {
		t.Fatalf("unexpected ssh starts: got %d want %d", got, want)
	}
	if guestDialer.attempts != 0 {
		t.Fatalf("expected no guest agent use without autoprovision, got %d attempts", guestDialer.attempts)
	}
}

func TestManagerLaunchSkipsGuestFileDirectoryInstallWhenParentExists(t *testing.T) {
	tmpDir := t.TempDir()
	cfg := validManifest(tmpDir)
	cfg.Paths.LockPath = filepath.Join(tmpDir, "virtle.lock")
	cfg.QEMU.QMP.SocketPath = "qmp.sock"
	cfg.QEMU.GuestAgent.SocketPath = "qga.sock"
	cfg.Volumes[0].AutoCreate = false

	inlineText := "inline"
	inlineChown := "agent:users"
	overwrite := true
	cfg.WriteFiles = manifest.WriteFiles{
		"/etc/virtle/inline": {Content: manifest.WriteFileContent{Kind: manifest.WriteFileContentText, Text: inlineText}, Chown: inlineChown, Overwrite: overwrite, FollowLinks: true},
	}

	runner := &launchRunner{finishInteractiveSSH: true}
	qmpClient := &fakeQMPClient{
		onQuit: func() {
			runner.exitQEMU(nil)
		},
	}
	guestAgent := &fakeGuestAgentClient{
		execStatuses: []qga.ExecStatus{
			{Exited: true},
			{Exited: true},
		},
	}
	manager := &manager{
		logger:            slog.New(slog.DiscardHandler),
		locker:            &fileLocker{},
		runner:            runner,
		socketWaiter:      &fakeSocketWaiter{callback: func(paths []string) error { return nil }},
		qmpDialer:         &fakeQMPDialer{client: qmpClient},
		guestAgentDialer:  &fakeGuestAgentDialer{client: guestAgent},
		sshRetryDelay:     0,
		shutdownDelay:     10 * time.Millisecond,
		qmpRetryDelay:     0,
		qmpConnectTimeout: 100 * time.Millisecond,
		qmpQuitTimeout:    time.Millisecond,
	}

	if err := manager.launch(context.Background(), cfg, nil); err != nil {
		t.Fatalf("launch: %v", err)
	}

	if got, want := guestAgent.execs, []guestExecCall{
		{
			path:          guestTestPath,
			args:          []string{"-d", "/etc/virtle"},
			captureOutput: true,
		},
		{
			path:          guestChownPath,
			args:          []string{"agent:users", "/etc/virtle/inline"},
			captureOutput: true,
		},
	}; !reflect.DeepEqual(got, want) {
		t.Fatalf("unexpected guest execs: got %#v want %#v", got, want)
	}
	if got, want := guestAgent.writes["/etc/virtle/inline"], "aW5saW5l"; got != want {
		t.Fatalf("unexpected inline write text: got %q want %q", got, want)
	}
}

func TestManagerLaunchSkipsGuestFileWhenOverwriteFalseAndPathExists(t *testing.T) {
	tmpDir := t.TempDir()
	cfg := validManifest(tmpDir)
	cfg.Paths.LockPath = filepath.Join(tmpDir, "virtle.lock")
	cfg.QEMU.QMP.SocketPath = "qmp.sock"
	cfg.QEMU.GuestAgent.SocketPath = "qga.sock"
	cfg.Volumes[0].AutoCreate = false

	hostPath := "missing-host-file"
	overwrite := false
	cfg.WriteFiles = manifest.WriteFiles{
		"/etc/virtle/existing": {Content: manifest.WriteFileContent{Kind: manifest.WriteFileContentPath, Path: hostPath}, Overwrite: overwrite, FollowLinks: true},
	}

	runner := &launchRunner{finishInteractiveSSH: true}
	qmpClient := &fakeQMPClient{
		onQuit: func() {
			runner.exitQEMU(nil)
		},
	}
	guestAgent := &fakeGuestAgentClient{
		execStatuses: []qga.ExecStatus{
			{Exited: true},
		},
	}
	var logOutput bytes.Buffer
	manager := &manager{
		logger:            slog.New(slog.NewTextHandler(&logOutput, nil)),
		locker:            &fileLocker{},
		runner:            runner,
		socketWaiter:      &fakeSocketWaiter{callback: func(paths []string) error { return nil }},
		qmpDialer:         &fakeQMPDialer{client: qmpClient},
		guestAgentDialer:  &fakeGuestAgentDialer{client: guestAgent},
		sshRetryDelay:     0,
		shutdownDelay:     10 * time.Millisecond,
		qmpRetryDelay:     0,
		qmpConnectTimeout: 100 * time.Millisecond,
		qmpQuitTimeout:    time.Millisecond,
	}

	if err := manager.launch(context.Background(), cfg, nil); err != nil {
		t.Fatalf("launch: %v", err)
	}

	if got, want := guestAgent.execs, []guestExecCall{
		{
			path:          guestTestPath,
			args:          []string{"-e", "/etc/virtle/existing"},
			captureOutput: true,
		},
	}; !reflect.DeepEqual(got, want) {
		t.Fatalf("unexpected guest execs: got %#v want %#v", got, want)
	}
	if len(guestAgent.writes) != 0 {
		t.Fatalf("expected no guest writes, got %#v", guestAgent.writes)
	}
	if logs := logOutput.String(); !strings.Contains(logs, "skipped existing guest file because overwrite is false") || !strings.Contains(logs, "/etc/virtle/existing") {
		t.Fatalf("expected overwrite=false skip log, got %q", logs)
	}
}

func TestManagerLaunchCreatesAllMissingGuestParentDirectoriesWithOwnerAndMode(t *testing.T) {
	tmpDir := t.TempDir()
	cfg := validManifest(tmpDir)
	cfg.Paths.LockPath = filepath.Join(tmpDir, "virtle.lock")
	cfg.QEMU.QMP.SocketPath = "qmp.sock"
	cfg.QEMU.GuestAgent.SocketPath = "qga.sock"
	cfg.Volumes[0].AutoCreate = false

	text := "nested"
	chown := "agent:users"
	mode := "0640"
	overwrite := true
	cfg.WriteFiles = manifest.WriteFiles{
		"/etc/virtle/nested/new": {Content: manifest.WriteFileContent{Kind: manifest.WriteFileContentText, Text: text}, Chown: chown, Mode: mode, Overwrite: overwrite, FollowLinks: true},
	}

	runner := &launchRunner{finishInteractiveSSH: true}
	qmpClient := &fakeQMPClient{
		onQuit: func() {
			runner.exitQEMU(nil)
		},
	}
	guestAgent := &fakeGuestAgentClient{
		execStatuses: []qga.ExecStatus{
			{Exited: true, ExitCode: 1}, // test -d /etc/virtle/nested
			{Exited: true, ExitCode: 1}, // test -d /etc/virtle
			{Exited: true},              // test -d /etc
			{Exited: true},              // install -d /etc/virtle
			{Exited: true},              // install -d /etc/virtle/nested
			{Exited: true},              // chown file
			{Exited: true},              // chmod file
		},
	}
	manager := &manager{
		logger:            slog.New(slog.DiscardHandler),
		locker:            &fileLocker{},
		runner:            runner,
		socketWaiter:      &fakeSocketWaiter{callback: func(paths []string) error { return nil }},
		qmpDialer:         &fakeQMPDialer{client: qmpClient},
		guestAgentDialer:  &fakeGuestAgentDialer{client: guestAgent},
		sshRetryDelay:     0,
		shutdownDelay:     10 * time.Millisecond,
		qmpRetryDelay:     0,
		qmpConnectTimeout: 100 * time.Millisecond,
		qmpQuitTimeout:    time.Millisecond,
	}

	if err := manager.launch(context.Background(), cfg, nil); err != nil {
		t.Fatalf("launch: %v", err)
	}

	if got, want := guestAgent.execs, []guestExecCall{
		{
			path:          guestTestPath,
			args:          []string{"-d", "/etc/virtle/nested"},
			captureOutput: true,
		},
		{
			path:          guestTestPath,
			args:          []string{"-d", "/etc/virtle"},
			captureOutput: true,
		},
		{
			path:          guestTestPath,
			args:          []string{"-d", "/etc"},
			captureOutput: true,
		},
		{
			path:          guestInstallPath,
			args:          []string{"-d", "-o", "agent", "-g", "users", "-m", "0750", "/etc/virtle"},
			captureOutput: true,
		},
		{
			path:          guestInstallPath,
			args:          []string{"-d", "-o", "agent", "-g", "users", "-m", "0750", "/etc/virtle/nested"},
			captureOutput: true,
		},
		{
			path:          guestChownPath,
			args:          []string{"agent:users", "/etc/virtle/nested/new"},
			captureOutput: true,
		},
		{
			path:          guestChmodPath,
			args:          []string{"0640", "/etc/virtle/nested/new"},
			captureOutput: true,
		},
	}; !reflect.DeepEqual(got, want) {
		t.Fatalf("unexpected guest execs: got %#v want %#v", got, want)
	}
	if got, want := guestAgent.writes["/etc/virtle/nested/new"], "bmVzdGVk"; got != want {
		t.Fatalf("unexpected guest write text: got %q want %q", got, want)
	}
}

func TestManagerLaunchWritesGuestFileWhenOverwriteFalseAndPathMissing(t *testing.T) {
	tmpDir := t.TempDir()
	cfg := validManifest(tmpDir)
	cfg.Paths.LockPath = filepath.Join(tmpDir, "virtle.lock")
	cfg.QEMU.QMP.SocketPath = "qmp.sock"
	cfg.QEMU.GuestAgent.SocketPath = "qga.sock"
	cfg.Volumes[0].AutoCreate = false

	text := "new"
	overwrite := false
	cfg.WriteFiles = manifest.WriteFiles{
		"/etc/virtle/new": {Content: manifest.WriteFileContent{Kind: manifest.WriteFileContentText, Text: text}, Overwrite: overwrite, FollowLinks: true},
	}

	runner := &launchRunner{finishInteractiveSSH: true}
	qmpClient := &fakeQMPClient{
		onQuit: func() {
			runner.exitQEMU(nil)
		},
	}
	guestAgent := &fakeGuestAgentClient{
		execStatuses: []qga.ExecStatus{
			{Exited: true, ExitCode: 1},
			{Exited: true},
			{Exited: true},
		},
	}
	manager := &manager{
		logger:            slog.New(slog.DiscardHandler),
		locker:            &fileLocker{},
		runner:            runner,
		socketWaiter:      &fakeSocketWaiter{callback: func(paths []string) error { return nil }},
		qmpDialer:         &fakeQMPDialer{client: qmpClient},
		guestAgentDialer:  &fakeGuestAgentDialer{client: guestAgent},
		sshRetryDelay:     0,
		shutdownDelay:     10 * time.Millisecond,
		qmpRetryDelay:     0,
		qmpConnectTimeout: 100 * time.Millisecond,
		qmpQuitTimeout:    time.Millisecond,
	}

	if err := manager.launch(context.Background(), cfg, nil); err != nil {
		t.Fatalf("launch: %v", err)
	}

	if got, want := guestAgent.execs, []guestExecCall{
		{
			path:          guestTestPath,
			args:          []string{"-e", "/etc/virtle/new"},
			captureOutput: true,
		},
		{
			path:          guestTestPath,
			args:          []string{"-d", "/etc/virtle"},
			captureOutput: true,
		},
	}; !reflect.DeepEqual(got, want) {
		t.Fatalf("unexpected guest execs: got %#v want %#v", got, want)
	}
	if got, want := guestAgent.writes["/etc/virtle/new"], "bmV3"; got != want {
		t.Fatalf("unexpected guest write text: got %q want %q", got, want)
	}
}

func TestManagerLaunchFailsOnGuestFileChownFailure(t *testing.T) {
	tmpDir := t.TempDir()
	cfg := validManifest(tmpDir)
	cfg.Paths.LockPath = filepath.Join(tmpDir, "virtle.lock")
	cfg.QEMU.QMP.SocketPath = "qmp.sock"
	cfg.QEMU.GuestAgent.SocketPath = "qga.sock"
	cfg.Volumes[0].AutoCreate = false

	inlineText := "inline"
	inlineChown := "agent:users"
	inlineMode := "0600"
	overwrite := true
	cfg.WriteFiles = manifest.WriteFiles{
		"/etc/inline": {Content: manifest.WriteFileContent{Kind: manifest.WriteFileContentText, Text: inlineText}, Chown: inlineChown, Mode: inlineMode, Overwrite: overwrite, FollowLinks: true},
	}

	runner := &launchRunner{finishInteractiveSSH: true}
	qmpClient := &fakeQMPClient{
		onQuit: func() {
			runner.exitQEMU(nil)
		},
	}
	guestAgent := &fakeGuestAgentClient{
		execStatuses: []qga.ExecStatus{
			{Exited: true},
			{Exited: true, ExitCode: 1, ErrData: "Y2hvd24gZmFpbGVk"},
		},
	}
	manager := &manager{
		logger:            slog.New(slog.DiscardHandler),
		locker:            &fileLocker{},
		runner:            runner,
		socketWaiter:      &fakeSocketWaiter{callback: func(paths []string) error { return nil }},
		qmpDialer:         &fakeQMPDialer{client: qmpClient},
		guestAgentDialer:  &fakeGuestAgentDialer{client: guestAgent},
		sshRetryDelay:     0,
		shutdownDelay:     10 * time.Millisecond,
		qmpRetryDelay:     0,
		qmpConnectTimeout: 100 * time.Millisecond,
		qmpQuitTimeout:    time.Millisecond,
	}

	err := manager.launch(context.Background(), cfg, nil)
	if err == nil {
		t.Fatal("expected launch to fail")
	}
	for _, want := range []string{"guest file write", "chown \"/etc/inline\" exited with status 1", "chown failed"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("expected error containing %q, got %v", want, err)
		}
	}
	if got, want := guestAgent.execs, []guestExecCall{
		{
			path:          guestTestPath,
			args:          []string{"-d", "/etc"},
			captureOutput: true,
		},
		{
			path:          guestChownPath,
			args:          []string{"agent:users", "/etc/inline"},
			captureOutput: true,
		},
	}; !reflect.DeepEqual(got, want) {
		t.Fatalf("unexpected guest execs after chown failure: got %#v want %#v", got, want)
	}
	if len(runner.sshArgs()) != 0 {
		t.Fatalf("expected chown failure before ssh starts, got ssh starts %v", runner.sshArgs())
	}
}

func TestManagerLaunchFailsOnGuestFileDirectoryFailure(t *testing.T) {
	tmpDir := t.TempDir()
	cfg := validManifest(tmpDir)
	cfg.Paths.LockPath = filepath.Join(tmpDir, "virtle.lock")
	cfg.QEMU.QMP.SocketPath = "qmp.sock"
	cfg.QEMU.GuestAgent.SocketPath = "qga.sock"
	cfg.Volumes[0].AutoCreate = false

	inlineText := "inline"
	inlineChown := "agent:users"
	overwrite := true
	cfg.WriteFiles = manifest.WriteFiles{
		"/etc/virtle/inline": {Content: manifest.WriteFileContent{Kind: manifest.WriteFileContentText, Text: inlineText}, Chown: inlineChown, Overwrite: overwrite, FollowLinks: true},
	}

	runner := &launchRunner{finishInteractiveSSH: true}
	qmpClient := &fakeQMPClient{
		onQuit: func() {
			runner.exitQEMU(nil)
		},
	}
	guestAgent := &fakeGuestAgentClient{
		execStatuses: []qga.ExecStatus{
			{Exited: true, ExitCode: 1}, // test -d /etc/virtle
			{Exited: true},              // test -d /etc
			{Exited: true, ExitCode: 1, ErrData: "aW5zdGFsbCBmYWlsZWQ="}, // install -d /etc/virtle
		},
	}
	manager := &manager{
		logger:            slog.New(slog.DiscardHandler),
		locker:            &fileLocker{},
		runner:            runner,
		socketWaiter:      &fakeSocketWaiter{callback: func(paths []string) error { return nil }},
		qmpDialer:         &fakeQMPDialer{client: qmpClient},
		guestAgentDialer:  &fakeGuestAgentDialer{client: guestAgent},
		sshRetryDelay:     0,
		shutdownDelay:     10 * time.Millisecond,
		qmpRetryDelay:     0,
		qmpConnectTimeout: 100 * time.Millisecond,
		qmpQuitTimeout:    time.Millisecond,
	}

	err := manager.launch(context.Background(), cfg, nil)
	if err == nil {
		t.Fatal("expected launch to fail")
	}
	for _, want := range []string{"guest file write", "install -d \"/etc/virtle\" exited with status 1", "install failed"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("expected error containing %q, got %v", want, err)
		}
	}
	if got, want := guestAgent.execs, []guestExecCall{
		{
			path:          guestTestPath,
			args:          []string{"-d", "/etc/virtle"},
			captureOutput: true,
		},
		{
			path:          guestTestPath,
			args:          []string{"-d", "/etc"},
			captureOutput: true,
		},
		{
			path:          guestInstallPath,
			args:          []string{"-d", "-o", "agent", "-g", "users", "/etc/virtle"},
			captureOutput: true,
		},
	}; !reflect.DeepEqual(got, want) {
		t.Fatalf("unexpected guest execs after install failure: got %#v want %#v", got, want)
	}
	if len(guestAgent.writes) != 0 {
		t.Fatalf("expected no guest writes after install failure, got %#v", guestAgent.writes)
	}
	if len(runner.sshArgs()) != 0 {
		t.Fatalf("expected install failure before ssh starts, got ssh starts %v", runner.sshArgs())
	}
}

func TestManagerLaunchFailsOnGuestFileChmodFailure(t *testing.T) {
	tmpDir := t.TempDir()
	cfg := validManifest(tmpDir)
	cfg.Paths.LockPath = filepath.Join(tmpDir, "virtle.lock")
	cfg.QEMU.QMP.SocketPath = "qmp.sock"
	cfg.QEMU.GuestAgent.SocketPath = "qga.sock"
	cfg.Volumes[0].AutoCreate = false

	inlineText := "inline"
	inlineMode := "0600"
	overwrite := true
	cfg.WriteFiles = manifest.WriteFiles{
		"/etc/inline": {Content: manifest.WriteFileContent{Kind: manifest.WriteFileContentText, Text: inlineText}, Mode: inlineMode, Overwrite: overwrite, FollowLinks: true},
	}

	runner := &launchRunner{finishInteractiveSSH: true}
	qmpClient := &fakeQMPClient{
		onQuit: func() {
			runner.exitQEMU(nil)
		},
	}
	guestAgent := &fakeGuestAgentClient{
		execStatuses: []qga.ExecStatus{
			{
				Exited: true,
			},
			{
				Exited:   true,
				ExitCode: 1,
				ErrData:  "Y2htb2QgZmFpbGVk",
			},
		},
	}
	manager := &manager{
		logger:            slog.New(slog.DiscardHandler),
		locker:            &fileLocker{},
		runner:            runner,
		socketWaiter:      &fakeSocketWaiter{callback: func(paths []string) error { return nil }},
		qmpDialer:         &fakeQMPDialer{client: qmpClient},
		guestAgentDialer:  &fakeGuestAgentDialer{client: guestAgent},
		sshRetryDelay:     0,
		shutdownDelay:     10 * time.Millisecond,
		qmpRetryDelay:     0,
		qmpConnectTimeout: 100 * time.Millisecond,
		qmpQuitTimeout:    time.Millisecond,
	}

	err := manager.launch(context.Background(), cfg, nil)
	if err == nil {
		t.Fatal("expected launch to fail")
	}
	for _, want := range []string{"guest file write", "chmod \"/etc/inline\" exited with status 1", "chmod failed"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("expected error containing %q, got %v", want, err)
		}
	}
	if len(runner.sshArgs()) != 0 {
		t.Fatalf("expected chmod failure before ssh starts, got ssh starts %v", runner.sshArgs())
	}
}

func TestManagerLaunchSkipsGuestFilesOnResume(t *testing.T) {
	tmpDir := t.TempDir()
	cfg := validManifest(tmpDir)
	cfg.Paths.LockPath = filepath.Join(tmpDir, "virtle.lock")
	cfg.QEMU.QMP.SocketPath = "qmp.sock"
	cfg.QEMU.GuestAgent.SocketPath = "qga.sock"
	cfg.Volumes[0].AutoCreate = false
	inlineText := "inline"
	cfg.WriteFiles = manifest.WriteFiles{
		"/etc/inline": manifestWriteText(inlineText),
	}

	vmStatePath := filepath.Join(tmpDir, ".agentspace", "agent-sandbox.vmstate")
	if err := os.MkdirAll(filepath.Dir(vmStatePath), 0o755); err != nil {
		t.Fatalf("create state dir: %v", err)
	}
	if err := os.WriteFile(vmStatePath, []byte("state"), 0o644); err != nil {
		t.Fatalf("write vm state: %v", err)
	}
	if err := launch.WriteSuspendStateData(cfg, launch.SuspendState{
		HostName:      cfg.Identity.HostName,
		QMPSocketPath: filepath.Join(tmpDir, "old-qmp.sock"),
		VMStatePath:   vmStatePath,
		CID:           3,
		Status:        "saved",
	}); err != nil {
		t.Fatalf("write suspend state: %v", err)
	}

	runner := &launchRunner{
		finishInteractiveSSH: true,
	}
	qmpClient := &fakeQMPClient{
		onQuit: func() {
			runner.exitQEMU(nil)
		},
	}
	guestDialer := &fakeGuestAgentDialer{client: &fakeGuestAgentClient{}}
	manager := &manager{
		logger:              slog.New(slog.DiscardHandler),
		locker:              &fileLocker{},
		runner:              runner,
		socketWaiter:        &fakeSocketWaiter{callback: func(paths []string) error { return nil }},
		qmpDialer:           &fakeQMPDialer{client: qmpClient},
		guestAgentDialer:    guestDialer,
		sshRetryDelay:       0,
		shutdownDelay:       10 * time.Millisecond,
		qmpRetryDelay:       0,
		qmpConnectTimeout:   time.Millisecond,
		qmpQuitTimeout:      time.Millisecond,
		qmpMigrationTimeout: time.Millisecond,
	}

	if err := manager.launchWithOptions(context.Background(), cfg, nil, LaunchOptions{Resume: ResumeModeForce, SSH: true}); err != nil {
		t.Fatalf("resume launch: %v", err)
	}
	if guestDialer.attempts != 0 {
		t.Fatalf("expected resume launch to skip guest agent writes, got %d dial attempts", guestDialer.attempts)
	}
	if qmpClient.migrateIncomingCalls != 1 || qmpClient.contCalls != 1 {
		t.Fatalf("expected resume path to restore and continue, migrate=%d cont=%d", qmpClient.migrateIncomingCalls, qmpClient.contCalls)
	}
}

func TestManagerWriteGuestFileClosesAfterWriteFailure(t *testing.T) {
	client := &fakeGuestAgentClient{writeErr: errors.New("write failed")}
	manager := &manager{qmpConnectTimeout: time.Millisecond}

	err := manager.writeGuestFile(client, "/etc/fail", "ZmFpbA==")
	if err == nil || !strings.Contains(err.Error(), "write failed") {
		t.Fatalf("expected write failure, got %v", err)
	}
	if len(client.closes) != 1 || client.closes[0] != "/etc/fail" {
		t.Fatalf("expected close after write failure, got closes %v", client.closes)
	}
}

func TestManagerLaunchWithoutSSHSavesQueuedSuspend(t *testing.T) {
	tmpDir := t.TempDir()
	cfg := validManifest(tmpDir)
	cfg.Paths.LockPath = filepath.Join(tmpDir, "virtle.lock")
	cfg.QEMU.QMP.SocketPath = "qmp.sock"
	cfg.Volumes[0].AutoCreate = false

	signalCh := make(chan os.Signal, 8)
	runner := &launchRunner{
		onStart: func(name string, cmd *exec.Cmd) {
			if strings.HasPrefix(name, "qemu-system") {
				signalCh <- syscall.SIGTSTP
			}
		},
	}
	qmpClient := &fakeQMPClient{
		status: "running",
		onQuit: func() {
			runner.exitQEMU(nil)
		},
	}
	manager := &manager{
		logger:              slog.New(slog.DiscardHandler),
		locker:              &fileLocker{},
		runner:              runner,
		socketWaiter:        &fakeSocketWaiter{callback: func(paths []string) error { return nil }},
		qmpDialer:           &fakeQMPDialer{client: qmpClient},
		sshRetryDelay:       time.Hour,
		shutdownDelay:       10 * time.Millisecond,
		qmpRetryDelay:       0,
		qmpConnectTimeout:   time.Millisecond,
		qmpQuitTimeout:      time.Millisecond,
		qmpMigrationTimeout: time.Second,
		signals:             signalCh,
	}

	if err := manager.launchWithOptions(context.Background(), cfg, nil, LaunchOptions{Resume: ResumeModeNo, SSH: false}); err != nil {
		t.Fatalf("launch: %v", err)
	}

	state, err := launch.ReadSuspendState(cfg)
	if err != nil {
		t.Fatalf("read suspend state: %v", err)
	}
	if state.Status != "saved" || state.CID != 3 || state.VMStatePath != launch.VMStatePath(cfg) {
		t.Fatalf("unexpected suspend state: %+v", state)
	}
	if qmpClient.migrateCalls != 1 {
		t.Fatalf("expected one migration over launch-owned qmp, got %d", qmpClient.migrateCalls)
	}
	if len(runner.sshArgs()) != 0 {
		t.Fatalf("expected no ssh starts, got %d", len(runner.sshArgs()))
	}
	for _, signal := range runner.processSignals() {
		if signal.sig == syscall.SIGTSTP || signal.sig == syscall.SIGSTOP || signal.sig == syscall.SIGCONT {
			t.Fatalf("unexpected job-control signal forwarded to %s: %v", signal.name, signal.sig)
		}
	}
}

func TestManagerLaunchControlSuspendWaitsForGuestProvisioning(t *testing.T) {
	tmpDir := t.TempDir()
	cfg := validManifest(tmpDir)
	cfg.Paths.LockPath = filepath.Join(tmpDir, "virtle.lock")
	cfg.QEMU.QMP.SocketPath = "qmp.sock"
	cfg.QEMU.GuestAgent.SocketPath = "qga.sock"
	cfg.QEMU.SSHReady.SocketPath = ""
	cfg.Volumes[0].AutoCreate = false
	cfg.WriteFiles = manifest.WriteFiles{
		"/etc/virtle/startup": {
			Overwrite: true,
			Content:   manifest.WriteFileContent{Kind: manifest.WriteFileContentText, Text: "ready\n"},
		},
	}

	writeStarted := make(chan struct{})
	allowWrite := make(chan struct{})
	var writeStartedOnce sync.Once
	guestAgent := &fakeGuestAgentClient{
		record: func(event string) {
			if strings.HasPrefix(event, "guest-write:") {
				writeStartedOnce.Do(func() {
					close(writeStarted)
				})
				<-allowWrite
			}
		},
	}
	runner := &launchRunner{}
	qmpClient := &fakeQMPClient{
		status: "running",
		onQuit: func() {
			runner.exitQEMU(nil)
		},
	}
	manager := &manager{
		logger:              slog.New(slog.DiscardHandler),
		locker:              &fileLocker{},
		runner:              runner,
		socketWaiter:        &fakeSocketWaiter{callback: func(paths []string) error { return nil }},
		qmpDialer:           &fakeQMPDialer{client: qmpClient},
		guestAgentDialer:    &fakeGuestAgentDialer{client: guestAgent},
		sshRetryDelay:       time.Hour,
		shutdownDelay:       10 * time.Millisecond,
		qmpRetryDelay:       0,
		qmpConnectTimeout:   time.Millisecond,
		qmpQuitTimeout:      time.Millisecond,
		qmpMigrationTimeout: time.Second,
	}

	launchDone := make(chan error, 1)
	go func() {
		launchDone <- manager.launchWithOptions(context.Background(), cfg, nil, LaunchOptions{Resume: ResumeModeNo, SSH: false})
	}()

	select {
	case <-writeStarted:
	case err := <-launchDone:
		t.Fatalf("launch returned before guest write started: %v", err)
	case <-time.After(time.Second):
		t.Fatal("guest write did not start")
	}

	controlSocketPath, err := cfg.ResolvedControlSocketPath()
	if err != nil {
		t.Fatalf("resolve control socket: %v", err)
	}
	rpcCtx, cancelRPC := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancelRPC()
	rpcDone := make(chan error, 1)
	go func() {
		resp, err := control.Dial(controlSocketPath).Suspend(rpcCtx, control.SuspendRequest{})
		if err == nil && !resp.Saved {
			err = errors.New("suspend response was not saved")
		}
		rpcDone <- err
	}()

	select {
	case err := <-rpcDone:
		t.Fatalf("control suspend returned before guest write completed: %v", err)
	case <-time.After(testNoReturnTimeout):
	}
	qmpClient.mu.Lock()
	migrateCalls := qmpClient.migrateCalls
	qmpClient.mu.Unlock()
	if migrateCalls != 0 {
		t.Fatalf("control suspend migrated during guest provisioning, got %d calls", migrateCalls)
	}

	close(allowWrite)
	select {
	case err := <-rpcDone:
		if err != nil {
			t.Fatalf("control suspend: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("control suspend did not return after guest provisioning")
	}
	select {
	case err := <-launchDone:
		if err != nil {
			t.Fatalf("launch: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("launch did not return after control suspend")
	}
	qmpClient.mu.Lock()
	migrateCalls = qmpClient.migrateCalls
	qmpClient.mu.Unlock()
	if migrateCalls != 1 {
		t.Fatalf("expected one migration after guest provisioning, got %d", migrateCalls)
	}
}

func TestManagerLaunchHandlesDuplicateSuspendDuringActiveSessionWithoutForwardingJobControl(t *testing.T) {
	tmpDir := t.TempDir()
	cfg := validManifest(tmpDir)
	cfg.Paths.LockPath = filepath.Join(tmpDir, "virtle.lock")
	cfg.QEMU.QMP.SocketPath = "qmp.sock"
	cfg.Volumes[0].AutoCreate = false

	signalCh := make(chan os.Signal, 8)
	runner := &launchRunner{
		onStart: func(name string, cmd *exec.Cmd) {
			if name == "ssh" && !containsString(cmd.Args, "true") {
				signalCh <- syscall.SIGTSTP
				signalCh <- syscall.SIGTSTP
			}
		},
	}
	qmpClient := &fakeQMPClient{
		status: "running",
		onQuit: func() {
			runner.exitQEMU(nil)
		},
	}
	manager := &manager{
		logger:              slog.New(slog.DiscardHandler),
		locker:              &fileLocker{},
		runner:              runner,
		socketWaiter:        &fakeSocketWaiter{callback: func(paths []string) error { return nil }},
		qmpDialer:           &fakeQMPDialer{client: qmpClient},
		sshRetryDelay:       0,
		shutdownDelay:       10 * time.Millisecond,
		qmpRetryDelay:       0,
		qmpConnectTimeout:   time.Millisecond,
		qmpQuitTimeout:      time.Millisecond,
		qmpMigrationTimeout: time.Second,
		signals:             signalCh,
	}

	if err := manager.launch(context.Background(), cfg, nil); err != nil {
		t.Fatalf("launch: %v", err)
	}

	state, err := launch.ReadSuspendState(cfg)
	if err != nil {
		t.Fatalf("read suspend state: %v", err)
	}
	if state.Status != "saved" || state.CID != 3 {
		t.Fatalf("unexpected suspend state: %+v", state)
	}
	if qmpClient.migrateCalls != 1 {
		t.Fatalf("expected one migration over launch-owned qmp, got %d", qmpClient.migrateCalls)
	}
	if len(runner.sshArgs()) != 1 {
		t.Fatalf("expected one active ssh session, got %d ssh starts", len(runner.sshArgs()))
	}
	for _, signal := range runner.processSignals() {
		if signal.sig == syscall.SIGTSTP || signal.sig == syscall.SIGSTOP || signal.sig == syscall.SIGCONT {
			t.Fatalf("unexpected job-control signal forwarded to %s: %v", signal.name, signal.sig)
		}
	}
}

func TestManagerLaunchUsesExternalVirtioFSSocketWithoutManagingDaemon(t *testing.T) {
	tmpDir := t.TempDir()
	cfg := validManifest(tmpDir)
	cfg.Paths.LockPath = filepath.Join(tmpDir, "virtle.lock")
	cfg.QEMU.Devices.Block[0].ImagePath = "root.img"
	cfg.Volumes[0].AutoCreate = false
	externalSocket := filepath.Join(tmpDir, "virtiofs-nix-store.sock")
	listener, err := net.Listen("unix", externalSocket)
	if err != nil {
		t.Fatalf("listen on external socket: %v", err)
	}
	defer listener.Close()
	cfg.QEMU.Devices.VirtioFS[0].SocketPath = externalSocket
	cfg.Run = nil
	cfg.CleanupFiles = nil

	cancelCtx, cancel := context.WithCancel(context.Background())
	defer cancel()

	runner := &launchRunner{cancel: cancel}
	qmpClient := &fakeQMPClient{
		onQuit: func() {
			runner.exitQEMU(nil)
		},
	}
	waiter := &fakeSocketWaiter{
		callback: func(paths []string) error {
			return nil
		},
	}
	manager := &manager{
		logger:            slog.New(slog.DiscardHandler),
		locker:            &fileLocker{},
		runner:            runner,
		socketWaiter:      waiter,
		qmpDialer:         &fakeQMPDialer{client: qmpClient},
		sshRetryDelay:     0,
		shutdownDelay:     10 * time.Millisecond,
		qmpConnectTimeout: time.Millisecond,
		qmpQuitTimeout:    time.Millisecond,
	}

	err = manager.launch(cancelCtx, cfg, nil)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context cancellation, got %v", err)
	}

	if containsString(runner.startedNames(), "virtiofsd-workspace") {
		t.Fatalf("unexpected managed virtiofsd start for external socket: %v", runner.startedNames())
	}
	if _, err := os.Stat(externalSocket); err != nil {
		t.Fatalf("expected external socket path to be left alone: %v", err)
	}
	if len(waiter.paths) == 0 || !reflect.DeepEqual(waiter.paths[0], []string{externalSocket}) {
		t.Fatalf("expected virtiofs readiness wait to use external socket, got %v", waiter.paths)
	}
}

func TestManagerLaunchRejectsMissingExternalVirtioFSSocket(t *testing.T) {
	tmpDir := t.TempDir()
	cfg := validManifest(tmpDir)
	cfg.Paths.LockPath = filepath.Join(tmpDir, "virtle.lock")
	cfg.QEMU.Devices.Block[0].ImagePath = "root.img"
	cfg.Volumes[0].AutoCreate = false
	externalSocket := filepath.Join(tmpDir, "missing-virtiofs.sock")
	cfg.QEMU.Devices.VirtioFS[0].SocketPath = externalSocket
	cfg.Run = nil
	cfg.CleanupFiles = nil

	runner := &launchRunner{}
	manager := &manager{
		logger: slog.New(slog.DiscardHandler),
		locker: &fileLocker{},
		runner: runner,
	}

	err := manager.launch(context.Background(), cfg, nil)
	if err == nil || !strings.Contains(err.Error(), "external virtiofs socket") || !strings.Contains(err.Error(), "does not exist") {
		t.Fatalf("expected missing external socket error, got %v", err)
	}
	if len(runner.startedNames()) != 0 {
		t.Fatalf("expected launch to fail before starting processes, got starts %v", runner.startedNames())
	}
}

func TestManagerLaunchSkipsVirtioFSReadinessWhenNoVirtioFSDevices(t *testing.T) {
	tmpDir := t.TempDir()
	cfg := validManifest(tmpDir)
	cfg.Paths.LockPath = filepath.Join(tmpDir, "virtle.lock")
	cfg.QEMU.Devices.VirtioFS = nil
	cfg.QEMU.Devices.Block = nil
	cfg.Volumes = nil
	cfg.Run = nil

	cancelCtx, cancel := context.WithCancel(context.Background())
	defer cancel()

	runner := &launchRunner{cancel: cancel}
	qmpClient := &fakeQMPClient{
		onQuit: func() {
			runner.exitQEMU(nil)
		},
	}
	waiter := &fakeSocketWaiter{
		callback: func(paths []string) error {
			return nil
		},
	}
	manager := &manager{
		logger:            slog.New(slog.DiscardHandler),
		locker:            &fileLocker{},
		runner:            runner,
		socketWaiter:      waiter,
		qmpDialer:         &fakeQMPDialer{client: qmpClient},
		sshRetryDelay:     0,
		shutdownDelay:     10 * time.Millisecond,
		qmpConnectTimeout: time.Millisecond,
		qmpQuitTimeout:    time.Millisecond,
	}

	err := manager.launch(cancelCtx, cfg, nil)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context cancellation, got %v", err)
	}

	if got, want := runner.startedNames(), []string{"qemu-system-x86_64", "ssh"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("unexpected start order: got %v want %v", got, want)
	}
	if got, want := waiter.calls, 2; got != want {
		t.Fatalf("unexpected waiter calls: got %d want %d", got, want)
	}
	qmpSocket := filepath.Join(tmpDir, "qmp.sock")
	sshReadySocket := filepath.Join(tmpDir, "ready.sock")
	if got, want := waiter.paths, [][]string{{qmpSocket}, {sshReadySocket}}; !reflect.DeepEqual(got, want) {
		t.Fatalf("unexpected socket waits: got %v want %v", got, want)
	}
	if containsString(runner.qemuArgs(), "vhost-user-fs") {
		t.Fatalf("expected qemu args to omit virtiofs devices: %v", runner.qemuArgs())
	}
	if containsString(runner.qemuArgs(), "virtio-blk") {
		t.Fatalf("expected qemu args to omit block devices: %v", runner.qemuArgs())
	}
}

func TestManagerLaunchWithOnlyNinePShareDoesNotWaitForVirtioFS(t *testing.T) {
	tmpDir := t.TempDir()
	cfg := validManifest(tmpDir)
	cfg.Paths.LockPath = filepath.Join(tmpDir, "virtle.lock")
	cfg.QEMU.Devices.VirtioFS = nil
	cfg.QEMU.Devices.Block = nil
	cfg.QEMU.Devices.NineP = []manifest.QEMUNinePShare{
		{
			ID:            "fs9p0",
			SourcePath:    "shared",
			Tag:           "shared",
			SecurityModel: "mapped",
			Transport:     "pci",
		},
	}
	cfg.Volumes = nil
	cfg.Run = nil

	cancelCtx, cancel := context.WithCancel(context.Background())
	defer cancel()

	runner := &launchRunner{cancel: cancel}
	qmpClient := &fakeQMPClient{
		onQuit: func() {
			runner.exitQEMU(nil)
		},
	}
	waiter := &fakeSocketWaiter{
		callback: func(paths []string) error {
			return nil
		},
	}
	manager := &manager{
		logger:            slog.New(slog.DiscardHandler),
		locker:            &fileLocker{},
		runner:            runner,
		socketWaiter:      waiter,
		qmpDialer:         &fakeQMPDialer{client: qmpClient},
		sshRetryDelay:     0,
		shutdownDelay:     10 * time.Millisecond,
		qmpConnectTimeout: time.Millisecond,
		qmpQuitTimeout:    time.Millisecond,
	}

	err := manager.launch(cancelCtx, cfg, nil)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context cancellation, got %v", err)
	}

	if got, want := runner.startedNames(), []string{"qemu-system-x86_64", "ssh"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("unexpected start order: got %v want %v", got, want)
	}
	qmpSocket := filepath.Join(tmpDir, "qmp.sock")
	sshReadySocket := filepath.Join(tmpDir, "ready.sock")
	if got, want := waiter.paths, [][]string{{qmpSocket}, {sshReadySocket}}; !reflect.DeepEqual(got, want) {
		t.Fatalf("unexpected socket waits: got %v want %v", got, want)
	}
	if containsString(runner.qemuArgs(), "vhost-user-fs") {
		t.Fatalf("expected qemu args to omit virtiofs devices: %v", runner.qemuArgs())
	}
	if !containsString(runner.qemuArgs(), "virtio-9p-pci,fsdev=fs9p0,mount_tag=shared") {
		t.Fatalf("expected qemu args to include 9p device: %v", runner.qemuArgs())
	}
}

func TestSaveSuspendStateConnectedStopsMigratesAndWritesSavedState(t *testing.T) {
	tmpDir := t.TempDir()
	cfg := validManifest(tmpDir)
	cfg.QEMU.QMP.SocketPath = "qmp.sock"

	qmpClient := &fakeQMPClient{status: "running"}
	manager := &manager{qmpConnectTimeout: time.Millisecond}

	qmpSocketPath := filepath.Join(tmpDir, "qmp.sock")
	if err := manager.saveSuspendStateConnected(context.Background(), cfg, qmpSocketPath, qmpClient, 7, nil); err != nil {
		t.Fatalf("suspend: %v", err)
	}

	qmpClient.mu.Lock()
	queryStatusCalls := qmpClient.queryStatusCalls
	stopCalls := qmpClient.stopCalls
	migrateCalls := qmpClient.migrateCalls
	queryMigrateCalls := qmpClient.queryMigrateCalls
	migratePath := qmpClient.migratePath
	status := qmpClient.status
	qmpClient.mu.Unlock()

	if queryStatusCalls != 1 {
		t.Fatalf("expected query-status once, got %d", queryStatusCalls)
	}
	if stopCalls != 1 {
		t.Fatalf("expected stop once, got %d", stopCalls)
	}
	if migrateCalls != 1 {
		t.Fatalf("expected migrate once, got %d", migrateCalls)
	}
	if queryMigrateCalls == 0 {
		t.Fatal("expected query-migrate polling")
	}
	if migratePath != launch.VMStatePath(cfg) {
		t.Fatalf("unexpected migrate path: got %q want %q", migratePath, launch.VMStatePath(cfg))
	}
	if status != "paused" {
		t.Fatalf("expected paused status, got %q", status)
	}

	statePath := launch.SuspendStatePath(cfg)
	data, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatalf("read suspend state: %v", err)
	}
	var state launch.SuspendState
	if err := json.Unmarshal(data, &state); err != nil {
		t.Fatalf("decode suspend state: %v", err)
	}
	if state.HostName != cfg.Identity.HostName {
		t.Fatalf("unexpected state host: got %q want %q", state.HostName, cfg.Identity.HostName)
	}
	if state.QMPSocketPath != qmpSocketPath {
		t.Fatalf("unexpected state qmp socket: got %q", state.QMPSocketPath)
	}
	if state.VMStatePath != launch.VMStatePath(cfg) {
		t.Fatalf("unexpected vm state path: got %q", state.VMStatePath)
	}
	if state.CID != 7 {
		t.Fatalf("unexpected state cid: got %d", state.CID)
	}
	if state.Status != "saved" {
		t.Fatalf("unexpected state status: got %q", state.Status)
	}
}

func TestLaunchSuspendHandlerSaveAndExitIsIdempotent(t *testing.T) {
	tmpDir := t.TempDir()
	cfg := validManifest(tmpDir)
	cfg.QEMU.QMP.SocketPath = "qmp.sock"

	qmpClient := &fakeQMPClient{status: "running"}
	manager := &manager{
		logger:              slog.New(slog.DiscardHandler),
		qmpConnectTimeout:   time.Millisecond,
		qmpMigrationTimeout: time.Second,
	}
	handler := newLaunchSuspendHandler(manager, cfg, filepath.Join(tmpDir, "qmp.sock"), qmpClient, 7, nil, nil)

	if err := handler.saveAndExit(context.Background()); !errors.Is(err, launch.ErrSavedSuspendExit) {
		t.Fatalf("first suspend returned %v, want launch.ErrSavedSuspendExit", err)
	}
	if err := handler.saveAndExit(context.Background()); !errors.Is(err, launch.ErrSavedSuspendExit) {
		t.Fatalf("second suspend returned %v, want launch.ErrSavedSuspendExit", err)
	}

	qmpClient.mu.Lock()
	queryStatusCalls := qmpClient.queryStatusCalls
	stopCalls := qmpClient.stopCalls
	migrateCalls := qmpClient.migrateCalls
	qmpClient.mu.Unlock()

	if queryStatusCalls != 1 {
		t.Fatalf("expected query-status once, got %d", queryStatusCalls)
	}
	if stopCalls != 1 {
		t.Fatalf("expected stop once, got %d", stopCalls)
	}
	if migrateCalls != 1 {
		t.Fatalf("expected migrate once, got %d", migrateCalls)
	}
}

type testSuspendControlHandler struct {
	fakeControlCore
	onSuspend func() error
}

func (h *testSuspendControlHandler) Suspend(context.Context, control.SuspendRequest) (control.SuspendResponse, error) {
	if h.onSuspend != nil {
		if err := h.onSuspend(); err != nil {
			return control.SuspendResponse{}, err
		}
	}
	return control.SuspendResponse{Saved: true, VMStatePath: "/tmp/vm-state"}, nil
}

func TestManagerSuspendControlSocketWaitsForLaunchExit(t *testing.T) {
	tmpDir := t.TempDir()
	cfg := validManifest(tmpDir)
	cfg.Paths.LockPath = filepath.Join(tmpDir, "virtle.lock")
	if err := launch.WriteLaunchPID(cfg, 12345); err != nil {
		t.Fatalf("write launch pid: %v", err)
	}
	controlSocketPath, err := cfg.ResolvedControlSocketPath()
	if err != nil {
		t.Fatalf("resolve control socket: %v", err)
	}

	allowRemove := make(chan struct{})
	removeDone := make(chan error, 1)
	suspendCalled := make(chan struct{})
	startTestControlServerAt(t, controlSocketPath, &testSuspendControlHandler{
		onSuspend: func() error {
			close(suspendCalled)
			go func() {
				<-allowRemove
				removeDone <- launch.RemoveLaunchPID(cfg, 12345)
			}()
			return nil
		},
	})

	done := make(chan error, 1)
	go func() {
		done <- (&manager{}).suspend(context.Background(), cfg)
	}()

	select {
	case <-suspendCalled:
	case err := <-done:
		t.Fatalf("suspend returned before control handler ran: %v", err)
	case <-time.After(time.Second):
		t.Fatal("control suspend was not called")
	}

	select {
	case err := <-done:
		t.Fatalf("suspend returned before launch pid removal: %v", err)
	case <-time.After(testNoReturnTimeout):
	}

	close(allowRemove)
	select {
	case err := <-removeDone:
		if err != nil {
			t.Fatalf("remove launch pid: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("launch pid was not removed")
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("suspend: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("suspend did not return after launch pid removal")
	}
}

func TestManagerSuspendSignalsLaunchAndWaitsForSavedStateAndExit(t *testing.T) {
	tmpDir := t.TempDir()
	cfg := validManifest(tmpDir)
	if err := launch.WriteLaunchPID(cfg, 12345); err != nil {
		t.Fatalf("write launch pid: %v", err)
	}
	releaseLock := acquireTestLaunchLock(t, cfg, 12345)
	defer releaseLock()

	dialer := &fakeQMPDialer{client: &fakeQMPClient{status: "running"}}
	signaler := &fakePIDSignaler{
		onSignal: func(pid int, sig os.Signal) error {
			if pid != 12345 {
				t.Fatalf("unexpected pid: got %d want 12345", pid)
			}
			if sig != syscall.SIGTSTP {
				t.Fatalf("unexpected signal: got %v want %v", sig, syscall.SIGTSTP)
			}
			if err := launch.WriteSuspendStateData(cfg, launch.SuspendState{
				QMPSocketPath: filepath.Join(tmpDir, "qmp.sock"),
				VMStatePath:   launch.VMStatePath(cfg),
				CID:           3,
				Status:        "saved",
			}); err != nil {
				return err
			}
			return launch.RemoveLaunchPID(cfg, 12345)
		},
	}
	manager := &manager{
		logger:              slog.New(slog.DiscardHandler),
		qmpDialer:           dialer,
		qmpConnectTimeout:   100 * time.Millisecond,
		qmpMigrationTimeout: time.Second,
		pidSignaler:         signaler,
	}

	if err := manager.suspend(context.Background(), cfg); err != nil {
		t.Fatalf("suspend: %v", err)
	}

	if dialer.attempts != 0 {
		t.Fatalf("expected no external qmp dial attempts, got %d", dialer.attempts)
	}
	if !reflect.DeepEqual(signaler.signals, []pidSignal{{pid: 12345, sig: syscall.SIGTSTP}}) {
		t.Fatalf("unexpected signals: got %v", signaler.signals)
	}
	state, err := launch.ReadSuspendState(cfg)
	if err != nil {
		t.Fatalf("read suspend state: %v", err)
	}
	if state.Status != "saved" || state.CID != 3 || state.VMStatePath == "" {
		t.Fatalf("unexpected saved state: %+v", state)
	}
}

func TestManagerSuspendSignalsActiveLaunchWhenSavedStateAlreadyExists(t *testing.T) {
	tmpDir := t.TempDir()
	cfg := validManifest(tmpDir)
	if err := launch.WriteSuspendStateData(cfg, launch.SuspendState{
		QMPSocketPath: filepath.Join(tmpDir, "old-qmp.sock"),
		VMStatePath:   launch.VMStatePath(cfg),
		CID:           3,
		Status:        "saved",
	}); err != nil {
		t.Fatalf("write saved suspend state: %v", err)
	}
	if err := launch.WriteLaunchPID(cfg, 12345); err != nil {
		t.Fatalf("write launch pid: %v", err)
	}
	releaseLock := acquireTestLaunchLock(t, cfg, 12345)
	defer releaseLock()

	signaler := &fakePIDSignaler{
		onSignal: func(pid int, sig os.Signal) error {
			if pid != 12345 {
				t.Fatalf("unexpected pid: got %d want 12345", pid)
			}
			if sig != syscall.SIGTSTP {
				t.Fatalf("unexpected signal: got %v want %v", sig, syscall.SIGTSTP)
			}
			return launch.RemoveLaunchPID(cfg, 12345)
		},
	}
	manager := &manager{
		logger:              slog.New(slog.DiscardHandler),
		qmpMigrationTimeout: time.Second,
		pidSignaler:         signaler,
	}

	if err := manager.suspend(context.Background(), cfg); err != nil {
		t.Fatalf("suspend: %v", err)
	}

	if !reflect.DeepEqual(signaler.signals, []pidSignal{{pid: 12345, sig: syscall.SIGTSTP}}) {
		t.Fatalf("unexpected signals: got %v", signaler.signals)
	}
}

func TestManagerSuspendPreservesExistingSavedStateWithoutSignal(t *testing.T) {
	tmpDir := t.TempDir()
	cfg := validManifest(tmpDir)
	if err := launch.WriteSuspendStateData(cfg, launch.SuspendState{
		QMPSocketPath: filepath.Join(tmpDir, "qmp.sock"),
		VMStatePath:   launch.VMStatePath(cfg),
		CID:           3,
		Status:        "saved",
	}); err != nil {
		t.Fatalf("write saved suspend state: %v", err)
	}

	signaler := &fakePIDSignaler{}
	manager := &manager{
		logger:      slog.New(slog.DiscardHandler),
		qmpDialer:   &fakeQMPDialer{},
		pidSignaler: signaler,
	}

	if err := manager.suspend(context.Background(), cfg); err != nil {
		t.Fatalf("suspend: %v", err)
	}

	if len(signaler.signals) != 0 {
		t.Fatalf("expected no signal for repeated suspend, got %v", signaler.signals)
	}
}

func TestEffectiveSuspendSignalTimeoutIncludesMigrationAndTeardown(t *testing.T) {
	tmpDir := t.TempDir()
	cfg := validManifest(tmpDir)
	cfg.Run = append(cfg.Run, manifest.Run{
		Exec: []string{"/tmp/virtiofsd-cache"},
	})

	manager := &manager{
		logger:              slog.New(slog.DiscardHandler),
		shutdownDelay:       4 * time.Second,
		qmpQuitTimeout:      3 * time.Second,
		qmpMigrationTimeout: 2 * time.Second,
	}

	got := manager.effectiveSuspendSignalTimeout(cfg)
	want := defaultLaunchSignalTimeout + 2*time.Second + 3*time.Second + 4*4*time.Second
	if got != want {
		t.Fatalf("unexpected suspend signal timeout: got %s want %s", got, want)
	}
}

func TestManagerSuspendMissingPIDReportsLaunchError(t *testing.T) {
	tmpDir := t.TempDir()
	cfg := validManifest(tmpDir)

	err := (&manager{pidSignaler: &fakePIDSignaler{}}).suspend(context.Background(), cfg)
	if err == nil {
		t.Fatal("expected missing pid error")
	}
	if !strings.Contains(err.Error(), "launch pid file") || !strings.Contains(err.Error(), "does not exist") {
		t.Fatalf("unexpected missing pid error: %v", err)
	}
}

func TestManagerLaunchResumeForceMissingSavedStateReportsRestoreError(t *testing.T) {
	tmpDir := t.TempDir()
	cfg := validManifest(tmpDir)

	err := (&manager{}).launchWithOptions(context.Background(), cfg, nil, LaunchOptions{Resume: ResumeModeForce, SSH: true})
	if err == nil {
		t.Fatal("expected missing saved state error")
	}
	if !strings.Contains(err.Error(), "no saved suspend state") {
		t.Fatalf("unexpected missing saved state error: %v", err)
	}
}

func TestManagerLaunchResumeForceNonSavedStateReportsRestoreError(t *testing.T) {
	tmpDir := t.TempDir()
	cfg := validManifest(tmpDir)
	if err := launch.WriteSuspendStateData(cfg, launch.SuspendState{
		QMPSocketPath: filepath.Join(tmpDir, "qmp.sock"),
		Status:        "paused",
	}); err != nil {
		t.Fatalf("write initial suspend state: %v", err)
	}

	err := (&manager{}).launchWithOptions(context.Background(), cfg, nil, LaunchOptions{Resume: ResumeModeForce, SSH: true})
	if err == nil {
		t.Fatal("expected non-saved state error")
	}
	if !strings.Contains(err.Error(), "not saved") {
		t.Fatalf("unexpected non-saved state error: %v", err)
	}
}

func TestManagerLaunchResumeAutoFreshLaunchesWithoutSavedState(t *testing.T) {
	tmpDir := t.TempDir()
	cfg := validManifest(tmpDir)
	cfg.Paths.LockPath = filepath.Join(tmpDir, "virtle.lock")
	cfg.Volumes[0].AutoCreate = false

	runner := &launchRunner{finishInteractiveSSH: true}
	qmpClient := &fakeQMPClient{
		onQuit: func() {
			runner.exitQEMU(nil)
		},
	}
	manager := &manager{
		logger:            slog.New(slog.DiscardHandler),
		locker:            &fileLocker{},
		runner:            runner,
		socketWaiter:      &fakeSocketWaiter{callback: func(paths []string) error { return nil }},
		qmpDialer:         &fakeQMPDialer{client: qmpClient},
		sshRetryDelay:     0,
		shutdownDelay:     10 * time.Millisecond,
		qmpConnectTimeout: time.Millisecond,
		qmpQuitTimeout:    time.Millisecond,
	}

	if err := manager.launchWithOptions(context.Background(), cfg, nil, LaunchOptions{Resume: ResumeModeAuto, SSH: true}); err != nil {
		t.Fatalf("launch: %v", err)
	}
	if containsString(runner.qemuArgs(), "-incoming") {
		t.Fatalf("expected fresh qemu launch without incoming migration: %v", runner.qemuArgs())
	}
	if qmpClient.migrateIncomingCalls != 0 || qmpClient.contCalls != 0 {
		t.Fatalf("unexpected restore qmp calls: migrate-incoming=%d cont=%d", qmpClient.migrateIncomingCalls, qmpClient.contCalls)
	}
}

func TestManagerLaunchResumeForceRestoresAndRemovesSavedState(t *testing.T) {
	tmpDir := t.TempDir()
	cfg := validManifest(tmpDir)
	cfg.Paths.LockPath = filepath.Join(tmpDir, "virtle.lock")
	cfg.Volumes[0].AutoCreate = false
	statePath := launch.VMStatePath(cfg)
	if err := os.MkdirAll(filepath.Dir(statePath), 0o755); err != nil {
		t.Fatalf("create state dir: %v", err)
	}
	if err := os.WriteFile(statePath, []byte("saved state"), 0o644); err != nil {
		t.Fatalf("write vm state: %v", err)
	}
	if err := launch.WriteSuspendStateData(cfg, launch.SuspendState{
		QMPSocketPath: filepath.Join(tmpDir, "qmp.sock"),
		VMStatePath:   statePath,
		CID:           3,
		Status:        "saved",
	}); err != nil {
		t.Fatalf("write suspend state: %v", err)
	}

	runner := &launchRunner{finishInteractiveSSH: true}
	qmpClient := &fakeQMPClient{
		status: "paused",
		onQuit: func() {
			runner.exitQEMU(nil)
		},
	}
	manager := &manager{
		logger:              slog.New(slog.DiscardHandler),
		locker:              &fileLocker{},
		runner:              runner,
		socketWaiter:        &fakeSocketWaiter{callback: func(paths []string) error { return nil }},
		qmpDialer:           &fakeQMPDialer{client: qmpClient},
		sshRetryDelay:       0,
		shutdownDelay:       10 * time.Millisecond,
		qmpConnectTimeout:   time.Millisecond,
		qmpQuitTimeout:      time.Millisecond,
		qmpMigrationTimeout: time.Second,
	}

	if err := manager.launchWithOptions(context.Background(), cfg, nil, LaunchOptions{Resume: ResumeModeForce, SSH: true}); err != nil {
		t.Fatalf("launch resume: %v", err)
	}
	if !containsString(runner.qemuArgs(), "-incoming") || !containsString(runner.qemuArgs(), "defer") {
		t.Fatalf("expected incoming qemu launch: %v", runner.qemuArgs())
	}
	if qmpClient.migrateIncomingCalls != 1 || qmpClient.contCalls != 1 {
		t.Fatalf("unexpected restore qmp calls: migrate-incoming=%d cont=%d", qmpClient.migrateIncomingCalls, qmpClient.contCalls)
	}
	if qmpClient.migrateIncomingPath != statePath {
		t.Fatalf("unexpected migrate-incoming path: got %q want %q", qmpClient.migrateIncomingPath, statePath)
	}
	if _, err := os.Stat(statePath); !os.IsNotExist(err) {
		t.Fatalf("expected vm state removal, stat err: %v", err)
	}
	if _, err := os.Stat(launch.SuspendStatePath(cfg)); !os.IsNotExist(err) {
		t.Fatalf("expected suspend state removal, stat err: %v", err)
	}
}

func TestManagerLaunchResumeForceSavesSuspendDuringRestoredSession(t *testing.T) {
	tmpDir := t.TempDir()
	cfg := validManifest(tmpDir)
	cfg.Paths.LockPath = filepath.Join(tmpDir, "virtle.lock")
	cfg.Volumes[0].AutoCreate = false
	statePath := launch.VMStatePath(cfg)
	if err := os.MkdirAll(filepath.Dir(statePath), 0o755); err != nil {
		t.Fatalf("create state dir: %v", err)
	}
	if err := os.WriteFile(statePath, []byte("saved state"), 0o644); err != nil {
		t.Fatalf("write vm state: %v", err)
	}
	if err := launch.WriteSuspendStateData(cfg, launch.SuspendState{
		QMPSocketPath: filepath.Join(tmpDir, "qmp.sock"),
		VMStatePath:   statePath,
		CID:           3,
		Status:        "saved",
	}); err != nil {
		t.Fatalf("write suspend state: %v", err)
	}

	signalCh := make(chan os.Signal, 8)
	runner := &launchRunner{
		onStart: func(name string, cmd *exec.Cmd) {
			if name == "ssh" && !containsString(cmd.Args, "true") {
				signalCh <- syscall.SIGTSTP
			}
		},
	}
	qmpClient := &fakeQMPClient{
		status: "paused",
		onQuit: func() {
			runner.exitQEMU(nil)
		},
	}
	manager := &manager{
		logger:              slog.New(slog.DiscardHandler),
		locker:              &fileLocker{},
		runner:              runner,
		socketWaiter:        &fakeSocketWaiter{callback: func(paths []string) error { return nil }},
		qmpDialer:           &fakeQMPDialer{client: qmpClient},
		sshRetryDelay:       0,
		shutdownDelay:       10 * time.Millisecond,
		qmpConnectTimeout:   time.Millisecond,
		qmpQuitTimeout:      time.Millisecond,
		qmpMigrationTimeout: time.Second,
		signals:             signalCh,
	}

	if err := manager.launchWithOptions(context.Background(), cfg, nil, LaunchOptions{Resume: ResumeModeForce, SSH: true}); err != nil {
		t.Fatalf("launch resume: %v", err)
	}
	if qmpClient.migrateIncomingCalls != 1 || qmpClient.contCalls != 1 || qmpClient.migrateCalls != 1 {
		t.Fatalf("unexpected qmp calls: migrate-incoming=%d cont=%d migrate=%d", qmpClient.migrateIncomingCalls, qmpClient.contCalls, qmpClient.migrateCalls)
	}
	readState, err := launch.ReadSuspendState(cfg)
	if err != nil {
		t.Fatalf("read new suspend state: %v", err)
	}
	if readState.Status != "saved" || readState.CID != 3 {
		t.Fatalf("unexpected new suspend state: %+v", readState)
	}
}

func TestManagerLaunchResumeCancellationDuringActiveSessionIsNotSuspend(t *testing.T) {
	tmpDir := t.TempDir()
	cfg := validManifest(tmpDir)
	cfg.Paths.LockPath = filepath.Join(tmpDir, "virtle.lock")
	cfg.Volumes[0].AutoCreate = false
	statePath := launch.VMStatePath(cfg)
	if err := os.MkdirAll(filepath.Dir(statePath), 0o755); err != nil {
		t.Fatalf("create state dir: %v", err)
	}
	if err := os.WriteFile(statePath, []byte("saved state"), 0o644); err != nil {
		t.Fatalf("write vm state: %v", err)
	}
	if err := launch.WriteSuspendStateData(cfg, launch.SuspendState{
		QMPSocketPath: filepath.Join(tmpDir, "qmp.sock"),
		VMStatePath:   statePath,
		CID:           3,
		Status:        "saved",
	}); err != nil {
		t.Fatalf("write suspend state: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	runner := &launchRunner{
		onStart: func(name string, cmd *exec.Cmd) {
			if name == "ssh" && !containsString(cmd.Args, "true") {
				cancel()
			}
		},
	}
	qmpClient := &fakeQMPClient{
		status: "paused",
		onQuit: func() {
			runner.exitQEMU(nil)
		},
	}
	manager := &manager{
		logger:              slog.New(slog.DiscardHandler),
		locker:              &fileLocker{},
		runner:              runner,
		socketWaiter:        &fakeSocketWaiter{callback: func(paths []string) error { return nil }},
		qmpDialer:           &fakeQMPDialer{client: qmpClient},
		sshRetryDelay:       0,
		shutdownDelay:       10 * time.Millisecond,
		qmpConnectTimeout:   time.Millisecond,
		qmpQuitTimeout:      time.Millisecond,
		qmpMigrationTimeout: time.Second,
	}

	err := manager.launchWithOptions(ctx, cfg, nil, LaunchOptions{Resume: ResumeModeForce, SSH: true})
	if err == nil {
		t.Fatal("expected resume cancellation error")
	}
	if launch.IsSavedSuspendExit(err) {
		t.Fatalf("cancellation was misreported as suspend: %v", err)
	}
	if qmpClient.migrateCalls != 0 {
		t.Fatalf("unexpected new suspend migration, got %d", qmpClient.migrateCalls)
	}
}

func TestManagerLaunchResumeForcePreservesStateWhenSessionStartFails(t *testing.T) {
	tmpDir := t.TempDir()
	cfg := validManifest(tmpDir)
	cfg.Paths.LockPath = filepath.Join(tmpDir, "virtle.lock")
	cfg.Volumes[0].AutoCreate = false
	statePath := launch.VMStatePath(cfg)
	if err := os.MkdirAll(filepath.Dir(statePath), 0o755); err != nil {
		t.Fatalf("create state dir: %v", err)
	}
	if err := os.WriteFile(statePath, []byte("saved state"), 0o644); err != nil {
		t.Fatalf("write vm state: %v", err)
	}
	if err := launch.WriteSuspendStateData(cfg, launch.SuspendState{
		QMPSocketPath: filepath.Join(tmpDir, "qmp.sock"),
		VMStatePath:   statePath,
		CID:           3,
		Status:        "saved",
	}); err != nil {
		t.Fatalf("write suspend state: %v", err)
	}

	runner := &launchRunner{failInteractiveSSH: true}
	qmpClient := &fakeQMPClient{
		status: "paused",
		onQuit: func() {
			runner.exitQEMU(nil)
		},
	}
	manager := &manager{
		logger:              slog.New(slog.DiscardHandler),
		locker:              &fileLocker{},
		runner:              runner,
		socketWaiter:        &fakeSocketWaiter{callback: func(paths []string) error { return nil }},
		qmpDialer:           &fakeQMPDialer{client: qmpClient},
		sshRetryDelay:       0,
		shutdownDelay:       10 * time.Millisecond,
		qmpConnectTimeout:   time.Millisecond,
		qmpQuitTimeout:      time.Millisecond,
		qmpMigrationTimeout: time.Second,
	}

	err := manager.launchWithOptions(context.Background(), cfg, nil, LaunchOptions{Resume: ResumeModeForce, SSH: true})
	if err == nil {
		t.Fatal("expected resume saved to fail")
	}
	if !strings.Contains(err.Error(), "session start failed") {
		t.Fatalf("unexpected resume error: %v", err)
	}
	if _, err := os.Stat(statePath); err != nil {
		t.Fatalf("expected saved vm state to remain: %v", err)
	}
	readState, err := launch.ReadSuspendState(cfg)
	if err != nil {
		t.Fatalf("expected suspend state to remain: %v", err)
	}
	if readState.Status != "saved" || readState.CID != 3 {
		t.Fatalf("unexpected preserved suspend state: %+v", readState)
	}
}

func TestWaitForSessionReturnsNilWhenSavedStateExistsOnCancellation(t *testing.T) {
	tmpDir := t.TempDir()
	cfg := validManifest(tmpDir)
	if err := launch.WriteSuspendStateData(cfg, launch.SuspendState{
		QMPSocketPath: filepath.Join(tmpDir, "qmp.sock"),
		VMStatePath:   launch.VMStatePath(cfg),
		CID:           3,
		Status:        "saved",
	}); err != nil {
		t.Fatalf("write saved suspend state: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	session := (&executortest.Process{OverrideName: "ssh"}).Process()

	err := (&manager{}).waitForSession(ctx, session, newTestLaunchLifecycle(), nil, "", executor.Group{})
	if err == nil {
		t.Fatal("expected active session cancellation error")
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("unexpected wait for session error: %v", err)
	}
}

func TestAllocateCIDSkipsHostUnavailableIDs(t *testing.T) {
	tmpDir := t.TempDir()
	manifest := &manifest.Manifest{
		Paths: manifest.Paths{
			WorkingDir: tmpDir,
			LockPath:   filepath.Join(tmpDir, "virtle.lock"),
		},
		VSock: manifest.VSock{
			CIDRange: manifest.VSockCIDRange{
				Start: 7,
				End:   8,
			},
		},
	}

	checker := &fakeVSockCIDChecker{unavailable: map[int]bool{7: true}}
	cid, err := launch.AcquireCID(manifest, nil, checker)
	if err != nil {
		t.Fatalf("allocate cid: %v", err)
	}

	if cid != 8 {
		t.Fatalf("unexpected cid: got %d want 8", cid)
	}
	if got := checker.checked; !reflect.DeepEqual(got, []int{7, 8}) {
		t.Fatalf("unexpected checked cids: got %v want [7 8]", got)
	}
}

func TestAllocateCIDReturnsHostCheckError(t *testing.T) {
	tmpDir := t.TempDir()
	manifest := &manifest.Manifest{
		Paths: manifest.Paths{
			WorkingDir: tmpDir,
			LockPath:   filepath.Join(tmpDir, "virtle.lock"),
		},
		VSock: manifest.VSock{
			CIDRange: manifest.VSockCIDRange{
				Start: 7,
				End:   7,
			},
		},
	}

	_, err := launch.AcquireCID(manifest, nil, &fakeVSockCIDChecker{
		err: errors.New("probe failed"),
	})
	if err == nil || !strings.Contains(err.Error(), "probe failed") {
		t.Fatalf("expected probe failure, got %v", err)
	}
}

func TestBuildQEMUCommandUsesTypedConfigAndRuntimeCID(t *testing.T) {
	cfg := validManifest("/tmp/work")
	cfg.QEMU.Console = manifest.QEMUConsoleOff

	spec, err := buildQEMUCommand(cfg, 42, false)
	if err != nil {
		t.Fatalf("build qemu command: %v", err)
	}

	if spec.Path != "/bin/qemu-system-x86_64" {
		t.Fatalf("unexpected qemu path: got %q want %q", spec.Path, "/bin/qemu-system-x86_64")
	}
	if !containsString(commandArgs(spec), "-name") || !containsString(commandArgs(spec), "agent-sandbox") {
		t.Fatalf("expected qemu args to include the guest name: %v", commandArgs(spec))
	}
	if !containsString(commandArgs(spec), "guest-cid=42") {
		t.Fatalf("expected qemu args to include the runtime cid: %v", commandArgs(spec))
	}
	if !containsString(commandArgs(spec), "unix:/tmp/work/qmp.sock,server,nowait") {
		t.Fatalf("expected qemu args to include the qmp socket: %v", commandArgs(spec))
	}
	if containsString(commandArgs(spec), "qga0") {
		t.Fatalf("expected qemu args to omit guest agent device when socket is unset: %v", commandArgs(spec))
	}
	if !containsString(commandArgs(spec), "memory-backend-memfd,id=mem,size=1024M,share=on") {
		t.Fatalf("expected qemu args to include the shared memory backend: %v", commandArgs(spec))
	}
	if !containsString(commandArgs(spec), "-nographic") {
		t.Fatalf("expected headless qemu args to include -nographic: %v", commandArgs(spec))
	}
	if !commandProcessGroup(spec) {
		t.Fatal("expected qemu to run in its own process group")
	}
}

func TestBuildQEMUCommandAddsPCIEHotplugPorts(t *testing.T) {
	manifest := validManifest("/tmp/work")
	manifest.QEMU.Hotplug.PCIEPorts = 2

	spec, err := buildQEMUCommand(manifest, 42, false)
	if err != nil {
		t.Fatalf("build qemu command: %v", err)
	}

	for _, want := range []string{
		"pcie-root-port,id=pcie.hotplug.0,bus=pcie.0,chassis=1,slot=1",
		"pcie-root-port,id=pcie.hotplug.1,bus=pcie.0,chassis=2,slot=2",
	} {
		portIndex := indexStringContaining(commandArgs(spec), want)
		if portIndex == -1 {
			t.Fatalf("expected qemu args to include hotplug port %q: %v", want, commandArgs(spec))
		}
		rngIndex := indexStringContaining(commandArgs(spec), "virtio-rng-pci")
		if rngIndex == -1 {
			t.Fatalf("expected qemu args to include pci rng device: %v", commandArgs(spec))
		}
		if portIndex > rngIndex {
			t.Fatalf("expected hotplug port %q before auto-addressed rng device: %v", want, commandArgs(spec))
		}
	}
}

type testHotplugControlHandler struct {
	fakeControlCore
	requests []control.HotplugRequest
}

func (h *testHotplugControlHandler) Hotplug(ctx context.Context, req control.HotplugRequest) (control.HotplugResponse, error) {
	h.requests = append(h.requests, req)
	return control.HotplugResponse{ID: req.ID, Detach: req.Detach}, nil
}

func TestManagerHotplugUsesControlSocket(t *testing.T) {
	tmpDir := t.TempDir()
	cfg := validManifest(tmpDir)
	cfg.Persistence.StateDir = ".virtle"
	cfg.Paths.RuntimeDir = manifest.RuntimeDir{Mode: manifest.RuntimeDirPath, Path: ".virtle"}

	controlSocketPath, err := cfg.ResolvedControlSocketPath()
	if err != nil {
		t.Fatalf("resolve control socket: %v", err)
	}
	handler := &testHotplugControlHandler{}
	startTestControlServerAt(t, controlSocketPath, handler)

	if err := (&manager{}).hotplug(context.Background(), cfg, "cache", true); err != nil {
		t.Fatalf("hotplug: %v", err)
	}
	if got, want := handler.requests, []control.HotplugRequest{{ID: "cache", Detach: true}}; !reflect.DeepEqual(got, want) {
		t.Fatalf("unexpected control requests: got %#v want %#v", got, want)
	}
}

func TestLaunchRuntimeRegistersHotplugAtControlPeriphery(t *testing.T) {
	tmpDir := t.TempDir()
	cfg := validManifest(tmpDir)
	cfg.Persistence.StateDir = ".virtle"
	cfg.Paths.RuntimeDir = manifest.RuntimeDir{Mode: manifest.RuntimeDirPath, Path: ".virtle"}
	cfg.QEMU.Hotplug.PCIEPorts = 1
	cfg.Hotplug = []hotplug.Device{
		{
			Kind: hotplug.KindNet,
			ID:   "vpn",
			Net:  hotplug.Net{Backend: "user", MAC: "02:02:00:00:00:10"},
		},
	}

	runner := &launchRunner{}
	qmp := &fakeQMPClient{}
	manager := &manager{
		locker:            &fileLocker{},
		runner:            runner,
		qmpDialer:         &fakeQMPDialer{client: qmp},
		socketWaiter:      &fakeSocketWaiter{},
		logger:            slog.New(slog.DiscardHandler),
		qmpConnectTimeout: time.Second,
		qmpRetryDelay:     time.Millisecond,
	}
	plan, err := manager.planLaunch(launch.Spec{Manifest: cfg, Options: LaunchOptions{Resume: ResumeModeNo, SSH: false}})
	if err != nil {
		t.Fatalf("plan launch: %v", err)
	}

	running, err := manager.startWithPlan(context.Background(), plan)
	if err != nil {
		t.Fatalf("start runtime: %v", err)
	}
	defer running.Close()

	_, err = control.Dial(plan.Paths.ControlSocket).Hotplug(context.Background(), control.HotplugRequest{ID: "vpn"})
	if err != nil {
		t.Fatalf("control hotplug: %v", err)
	}
	if got := strings.Join(qmp.rawCommands, "\n"); !strings.Contains(got, `"execute":"netdev_add"`) {
		t.Fatalf("expected netdev_add command, got %#v", qmp.rawCommands)
	}
}

func TestLaunchRuntimeRegistersGuestRPCsAtControlPeriphery(t *testing.T) {
	tmpDir := t.TempDir()
	cfg := validManifest(tmpDir)
	cfg.Persistence.StateDir = ".virtle"
	cfg.Paths.RuntimeDir = manifest.RuntimeDir{Mode: manifest.RuntimeDirPath, Path: ".virtle"}
	cfg.Paths.LockPath = filepath.Join(tmpDir, "virtle.lock")
	cfg.QEMU.QMP.SocketPath = "qmp.sock"
	cfg.QEMU.GuestAgent.SocketPath = "qga.sock"
	cfg.QEMU.SSHReady.SocketPath = ""
	cfg.Volumes[0].AutoCreate = false

	guestAgent := &fakeGuestAgentClient{
		execStatuses: []qga.ExecStatus{{
			Exited:  true,
			OutData: "cm9vdCBpbml0CmFnZW50IHZpcnRsZQo=",
		}, {
			Exited:   true,
			ExitCode: 3,
			OutData:  "ZXhlYy1vdXQK",
			ErrData:  "ZXhlYy1lcnIK",
		}},
		readPayloads: map[string][]string{
			"/tmp/message": {"aGVs", "bG8="},
		},
	}
	runner := &launchRunner{}
	manager := &manager{
		locker:            &fileLocker{},
		runner:            runner,
		qmpDialer:         &fakeQMPDialer{client: &fakeQMPClient{}},
		guestAgentDialer:  &fakeGuestAgentDialer{client: guestAgent},
		socketWaiter:      &fakeSocketWaiter{},
		logger:            slog.New(slog.DiscardHandler),
		qmpConnectTimeout: time.Second,
		qmpRetryDelay:     time.Millisecond,
	}
	plan, err := manager.planLaunch(launch.Spec{Manifest: cfg, Options: LaunchOptions{Resume: ResumeModeNo, SSH: false}})
	if err != nil {
		t.Fatalf("plan launch: %v", err)
	}

	running, err := manager.startWithPlan(context.Background(), plan)
	if err != nil {
		t.Fatalf("start runtime: %v", err)
	}
	defer running.Close()

	client := control.Dial(plan.Paths.ControlSocket)
	psResp, err := client.GuestPS(context.Background(), control.GuestPSRequest{})
	if err != nil {
		t.Fatalf("control guest ps: %v", err)
	}
	if psResp.ProcessList != "USER COMMAND\nagent virtle\nroot init" {
		t.Fatalf("unexpected process list: %q", psResp.ProcessList)
	}
	if got, want := len(guestAgent.execs), 1; got != want {
		t.Fatalf("guest exec count: got %d want %d", got, want)
	}
	exec := guestAgent.execs[0]
	if exec.path != guestPSPath || !reflect.DeepEqual(exec.args, []string{"-eo", "user=,comm="}) || !exec.captureOutput {
		t.Fatalf("unexpected ps exec: %#v", exec)
	}

	execResp, err := client.GuestExec(context.Background(), control.GuestExecRequest{
		Path:          "/bin/sh",
		Args:          []string{"-c", "echo hi"},
		CaptureOutput: true,
	})
	if err != nil {
		t.Fatalf("control guest exec: %v", err)
	}
	if !execResp.Exited || execResp.ExitCode != 3 || execResp.OutData != "ZXhlYy1vdXQK" || execResp.ErrData != "ZXhlYy1lcnIK" {
		t.Fatalf("unexpected guest exec response: %#v", execResp)
	}
	if got, want := len(guestAgent.execs), 2; got != want {
		t.Fatalf("guest exec count after guest-exec: got %d want %d", got, want)
	}
	exec = guestAgent.execs[1]
	if exec.path != "/bin/sh" || !reflect.DeepEqual(exec.args, []string{"-c", "echo hi"}) || !exec.captureOutput {
		t.Fatalf("unexpected guest-exec call: %#v", exec)
	}

	readResp, err := client.GuestRead(context.Background(), control.GuestReadRequest{Path: "/tmp/message"})
	if err != nil {
		t.Fatalf("control guest read: %v", err)
	}
	if readResp.Path != "/tmp/message" || readResp.DataBase64 != "aGVsbG8=" {
		t.Fatalf("unexpected guest read response: %#v", readResp)
	}

	writeResp, err := client.GuestWrite(context.Background(), control.GuestWriteRequest{Path: "/tmp/message", DataBase64: "dXBkYXRlZA=="})
	if err != nil {
		t.Fatalf("control guest write: %v", err)
	}
	if writeResp.Path != "/tmp/message" {
		t.Fatalf("unexpected guest write response: %#v", writeResp)
	}
	if got := guestAgent.writes["/tmp/message"]; got != "dXBkYXRlZA==" {
		t.Fatalf("unexpected guest write payload: %q", got)
	}
}

func TestLaunchRuntimeGuestPSMapsFailureToFailedPrecondition(t *testing.T) {
	tmpDir := t.TempDir()
	cfg := validManifest(tmpDir)
	cfg.Persistence.StateDir = ".virtle"
	cfg.Paths.RuntimeDir = manifest.RuntimeDir{Mode: manifest.RuntimeDirPath, Path: ".virtle"}
	cfg.Paths.LockPath = filepath.Join(tmpDir, "virtle.lock")
	cfg.QEMU.QMP.SocketPath = "qmp.sock"
	cfg.QEMU.GuestAgent.SocketPath = ""
	cfg.QEMU.SSHReady.SocketPath = ""
	cfg.Volumes[0].AutoCreate = false

	runner := &launchRunner{}
	manager := &manager{
		locker:            &fileLocker{},
		runner:            runner,
		qmpDialer:         &fakeQMPDialer{client: &fakeQMPClient{}},
		socketWaiter:      &fakeSocketWaiter{},
		logger:            slog.New(slog.DiscardHandler),
		qmpConnectTimeout: time.Second,
		qmpRetryDelay:     time.Millisecond,
	}
	plan, err := manager.planLaunch(launch.Spec{Manifest: cfg, Options: LaunchOptions{Resume: ResumeModeNo, SSH: false}})
	if err != nil {
		t.Fatalf("plan launch: %v", err)
	}

	running, err := manager.startWithPlan(context.Background(), plan)
	if err != nil {
		t.Fatalf("start runtime: %v", err)
	}
	defer running.Close()

	_, err = control.Dial(plan.Paths.ControlSocket).GuestPS(context.Background(), control.GuestPSRequest{})
	var rpcErr *control.RPCError
	if !errors.As(err, &rpcErr) {
		t.Fatalf("error type: got %T", err)
	}
	if rpcErr.Code != control.ErrFailedPrecondition {
		t.Fatalf("code: got %s want %s", rpcErr.Code, control.ErrFailedPrecondition)
	}
}

func TestBuildQEMUCommandAddsGraphicsArgs(t *testing.T) {
	tests := []struct {
		name string
		qemu manifest.QEMUGraphics
		want []string
	}{
		{
			name: "gtk",
			qemu: manifest.QEMUGraphics{Backend: "gtk"},
			want: []string{"-display", "gtk,gl=off", "virtio-vga", "qemu-xhci", "usb-tablet", "usb-kbd"},
		},
		{
			name: "cocoa",
			qemu: manifest.QEMUGraphics{Backend: "cocoa"},
			want: []string{"-display", "cocoa", "virtio-gpu", "qemu-xhci", "usb-tablet", "usb-kbd"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			manifest := validManifest("/tmp/work")
			manifest.QEMU.Knobs.NoGraphic = false
			manifest.QEMU.Graphics = tt.qemu

			spec, err := buildQEMUCommand(manifest, 42, false)
			if err != nil {
				t.Fatalf("build qemu command: %v", err)
			}

			if containsString(commandArgs(spec), "-nographic") {
				t.Fatalf("expected graphical qemu args to omit -nographic: %v", commandArgs(spec))
			}
			for _, want := range tt.want {
				if !containsString(commandArgs(spec), want) {
					t.Fatalf("expected qemu args to include %q: %v", want, commandArgs(spec))
				}
			}
		})
	}
}

func TestBuildQEMUCommandPreservesPassthroughGraphicsArgs(t *testing.T) {
	cfg := validManifest("/tmp/work")
	cfg.QEMU.Knobs.NoGraphic = false
	cfg.QEMU.PassthroughArgs = []string{"-display", "sdl", "-device", "virtio-vga"}

	spec, err := buildQEMUCommand(cfg, 42, false)
	if err != nil {
		t.Fatalf("build qemu command: %v", err)
	}

	if containsString(commandArgs(spec), "-nographic") {
		t.Fatalf("expected passthrough graphical qemu args to omit -nographic: %v", commandArgs(spec))
	}
	for _, want := range cfg.QEMU.PassthroughArgs {
		if !containsString(commandArgs(spec), want) {
			t.Fatalf("expected qemu args to include passthrough arg %q: %v", want, commandArgs(spec))
		}
	}
}

func TestBuildQEMUCommandUsesRuntimeCPUCountWhenOmitted(t *testing.T) {
	cfg := validManifest("/tmp/work")
	cfg.QEMU.SMP.CPUs = manifest.CPUCount{}

	spec, err := buildQEMUCommand(cfg, 42, false)
	if err != nil {
		t.Fatalf("build qemu command: %v", err)
	}

	smpIndex := indexString(commandArgs(spec), "-smp")
	if smpIndex == -1 || smpIndex+1 >= len(commandArgs(spec)) {
		t.Fatalf("expected qemu args to include -smp: %v", commandArgs(spec))
	}
	if got, want := commandArgs(spec)[smpIndex+1], fmt.Sprintf("%d", runtime.NumCPU()); got != want {
		t.Fatalf("unexpected runtime cpu count: got %q want %q", got, want)
	}
}

func TestBuildQEMUCommandAddsSerialConsoleArgsOnlyWhenEnabled(t *testing.T) {
	tests := []struct {
		name            string
		console         manifest.QEMUConsole
		wantChardev     string
		wantInteractive bool
	}{
		{
			name: "disabled",
		},
		{
			name:        "print",
			console:     manifest.QEMUConsolePrint,
			wantChardev: "stdio,id=stdio,signal=off",
		},
		{
			name:            "console",
			console:         manifest.QEMUConsoleInteractive,
			wantChardev:     "stdio,id=stdio,mux=on,signal=off",
			wantInteractive: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := validManifest("/tmp/work")
			cfg.QEMU.Console = tt.console

			spec, err := buildQEMUCommand(cfg, 42, false)
			if err != nil {
				t.Fatalf("build qemu command: %v", err)
			}
			for _, chardev := range []string{
				"stdio,id=stdio,signal=off",
				"stdio,id=stdio,mux=on,signal=off",
			} {
				if got, want := containsString(commandArgs(spec), chardev), chardev == tt.wantChardev; got != want {
					t.Fatalf("unexpected stdio chardev %q presence: got %v want %v args=%v", chardev, got, want, commandArgs(spec))
				}
			}
			wantConsoleArgs := tt.wantChardev != ""
			if got := containsString(commandArgs(spec), "chardev:stdio"); got != wantConsoleArgs {
				t.Fatalf("unexpected serial console presence: got %v want %v args=%v", got, wantConsoleArgs, commandArgs(spec))
			}
			if got := spec.Stdin == os.Stdin; got != tt.wantInteractive {
				t.Fatalf("unexpected stdin passthrough: got %v want %v", got, tt.wantInteractive)
			}
			if got, want := commandProcessGroup(spec), !tt.wantInteractive; got != want {
				t.Fatalf("unexpected process group isolation: got %v want %v", got, want)
			}
		})
	}
}

func TestBuildQEMUCommandAddsGuestAgentDevice(t *testing.T) {
	manifest := validManifest("/tmp/work")
	manifest.QEMU.GuestAgent.SocketPath = "qga.sock"
	manifest.QEMU.SSHReady.SocketPath = "ready.sock"

	spec, err := buildQEMUCommand(manifest, 42, false)
	if err != nil {
		t.Fatalf("build qemu command: %v", err)
	}

	if !containsString(commandArgs(spec), "socket,path=/tmp/work/qga.sock,server=on,wait=off,id=qga0") {
		t.Fatalf("expected qemu args to include guest agent chardev: %v", commandArgs(spec))
	}
	if !containsString(commandArgs(spec), "virtio-serial-pci,id=qga0-serial") {
		t.Fatalf("expected qemu args to include guest agent serial device: %v", commandArgs(spec))
	}
	if !containsString(commandArgs(spec), "virtserialport,chardev=qga0,name=org.qemu.guest_agent.0") {
		t.Fatalf("expected qemu args to include guest agent port: %v", commandArgs(spec))
	}
	if !containsString(commandArgs(spec), "socket,path=/tmp/work/ready.sock,server=on,wait=off,id=ready_char") {
		t.Fatalf("expected qemu args to include ssh readiness chardev: %v", commandArgs(spec))
	}
	if !containsString(commandArgs(spec), "virtio-serial-pci,id=ready-serial") {
		t.Fatalf("expected qemu args to include ssh readiness serial device: %v", commandArgs(spec))
	}
	if !containsString(commandArgs(spec), "virtserialport,chardev=ready_char,name=virtle.ready") {
		t.Fatalf("expected qemu args to include ssh readiness port: %v", commandArgs(spec))
	}
}

func TestBuildQEMUCommandOmitsSSHReadyDeviceWhenSocketEmpty(t *testing.T) {
	cfg := validManifest("/tmp/work")
	cfg.QEMU.SSHReady.SocketPath = ""

	spec, err := buildQEMUCommand(cfg, 42, false)
	if err != nil {
		t.Fatalf("build qemu command: %v", err)
	}

	if containsString(commandArgs(spec), "ready_char") {
		t.Fatalf("expected qemu args to omit ssh readiness chardev: %v", commandArgs(spec))
	}
	if containsString(commandArgs(spec), "virtio-serial-pci,id=ready-serial") {
		t.Fatalf("expected qemu args to omit ssh readiness serial device: %v", commandArgs(spec))
	}
	if containsString(commandArgs(spec), "virtserialport,chardev=ready_char,name=virtle.ready") {
		t.Fatalf("expected qemu args to omit ssh readiness port: %v", commandArgs(spec))
	}
}

func TestBuildQEMUCommandAddsNinePDevice(t *testing.T) {
	cfg := validManifest("/tmp/work")
	cfg.QEMU.Devices.NineP = []manifest.QEMUNinePShare{
		{
			ID:            "fs9p0",
			SourcePath:    "shares/cache",
			Tag:           "cache",
			SecurityModel: "none",
			ReadOnly:      true,
			Transport:     "pci",
		},
	}

	spec, err := buildQEMUCommand(cfg, 42, false)
	if err != nil {
		t.Fatalf("build qemu command: %v", err)
	}

	if !containsString(commandArgs(spec), "local,id=fs9p0,path=/tmp/work/shares/cache,security_model=none,readonly=on") {
		t.Fatalf("expected qemu args to include resolved 9p fsdev: %v", commandArgs(spec))
	}
	if !containsString(commandArgs(spec), "virtio-9p-pci,fsdev=fs9p0,mount_tag=cache") {
		t.Fatalf("expected qemu args to include 9p device: %v", commandArgs(spec))
	}
}

func TestBuildQEMUCommandPreservesOrderedMountDevices(t *testing.T) {
	cfg := validManifest("/tmp/work")
	cfg.QEMU.Devices.Mounts = []manifest.QEMUMountDevice{
		{
			Type: manifest.MountTypeNineP,
			NineP: &manifest.QEMUNinePShare{
				ID:            "fs9p0",
				SourcePath:    "shares/cache",
				Tag:           "cache",
				SecurityModel: "none",
				Transport:     "pci",
			},
		},
		{
			Type: manifest.MountTypeVirtioFS,
			VirtioFS: &manifest.QEMUVirtioFSShare{
				ID:         "fs0",
				SocketPath: "fs.sock",
				Tag:        "workspace",
				Transport:  "pci",
			},
		},
		{
			Type: manifest.MountTypeImage,
			Block: &manifest.QEMUBlockDevice{
				ID:        "vda",
				ImagePath: "root.img",
				Format:    "qcow2",
				AIO:       "threads",
				Transport: "pci",
			},
		},
	}

	spec, err := buildQEMUCommand(cfg, 42, false)
	if err != nil {
		t.Fatalf("build qemu command: %v", err)
	}

	ninePIndex := indexStringContaining(commandArgs(spec), "local,id=fs9p0,path=/tmp/work/shares/cache")
	virtioFSIndex := indexStringContaining(commandArgs(spec), "vhost-user-fs-pci,chardev=char-fs0,tag=workspace")
	blockIndex := indexStringContaining(commandArgs(spec), "id=vda,format=qcow2,file=/tmp/work/root.img")
	if ninePIndex == -1 || virtioFSIndex == -1 || blockIndex == -1 {
		t.Fatalf("expected qemu args to include all ordered mount devices: %v", commandArgs(spec))
	}
	if !(ninePIndex < virtioFSIndex && virtioFSIndex < blockIndex) {
		t.Fatalf("expected mount args in manifest order, got indexes 9p=%d virtiofs=%d block=%d args=%v", ninePIndex, virtioFSIndex, blockIndex, commandArgs(spec))
	}
}

func TestBuildQEMUCommandAllowsInitrdApplianceWithoutStorageDevices(t *testing.T) {
	manifest := validManifest("/tmp/work")
	manifest.QEMU.Memory.Backend = "default"
	manifest.QEMU.Memory.Shared = false
	manifest.QEMU.Devices.VirtioFS = nil
	manifest.QEMU.Devices.Block = nil
	manifest.QEMU.Devices.Network = nil
	manifest.Volumes = nil
	manifest.Run = nil

	spec, err := buildQEMUCommand(manifest, 42, false)
	if err != nil {
		t.Fatalf("build qemu command: %v", err)
	}

	if containsString(commandArgs(spec), "vhost-user-fs") {
		t.Fatalf("expected qemu args to omit virtiofs devices: %v", commandArgs(spec))
	}
	if containsString(commandArgs(spec), "virtio-blk") {
		t.Fatalf("expected qemu args to omit block devices: %v", commandArgs(spec))
	}
	if containsString(commandArgs(spec), "-netdev") || containsString(commandArgs(spec), "virtio-net") {
		t.Fatalf("expected qemu args to omit network devices: %v", commandArgs(spec))
	}
	if !containsString(commandArgs(spec), "virtio-rng-pci") {
		t.Fatalf("expected qemu args to retain rng device: %v", commandArgs(spec))
	}
	if !containsString(commandArgs(spec), "-qmp") {
		t.Fatalf("expected qemu args to retain qmp socket: %v", commandArgs(spec))
	}
	if !containsString(commandArgs(spec), "guest-cid=42") {
		t.Fatalf("expected qemu args to retain vsock device: %v", commandArgs(spec))
	}
}

func TestBuildQEMUCommandUsesRuntimeDirForRelativeQMP(t *testing.T) {
	runtimeDir := t.TempDir()
	setXDGTestRuntimeDir(t, runtimeDir)

	cfg := validManifest("/tmp/work")
	cfg.Paths.RuntimeDir = manifest.RuntimeDir{Mode: manifest.RuntimeDirXDG}
	cfg.QEMU.GuestAgent.SocketPath = "qga.sock"

	spec, err := buildQEMUCommand(cfg, 42, false)
	if err != nil {
		t.Fatalf("build qemu command: %v", err)
	}

	wantQMP := filepath.Join(runtimeDir, "agentspace", cfg.Identity.HostName, "qmp.sock")
	if !containsString(commandArgs(spec), "unix:"+wantQMP+",server,nowait") {
		t.Fatalf("expected qemu args to include runtime qmp socket %q: %v", wantQMP, commandArgs(spec))
	}
	wantQGA := filepath.Join(runtimeDir, "agentspace", cfg.Identity.HostName, "qga.sock")
	if !containsString(commandArgs(spec), "socket,path="+wantQGA+",server=on,wait=off,id=qga0") {
		t.Fatalf("expected qemu args to include runtime guest agent socket %q: %v", wantQGA, commandArgs(spec))
	}
	wantReady := filepath.Join(runtimeDir, "agentspace", cfg.Identity.HostName, "ready.sock")
	if !containsString(commandArgs(spec), "socket,path="+wantReady+",server=on,wait=off,id=ready_char") {
		t.Fatalf("expected qemu args to include runtime ssh readiness socket %q: %v", wantReady, commandArgs(spec))
	}
}

func TestStartRunsUsesNamedVirtioFSRunEnv(t *testing.T) {
	runtimeDir := t.TempDir()
	setXDGTestRuntimeDir(t, runtimeDir)

	cfg := validManifest(t.TempDir())
	cfg.Paths.RuntimeDir = manifest.RuntimeDir{Mode: manifest.RuntimeDirXDG}
	wantSocket := filepath.Join(runtimeDir, "agentspace", cfg.Identity.HostName, "fs.sock")
	cfg.Run[0].Vars["Socket"] = wantSocket

	runner := &launchRunner{}
	manager := &manager{
		logger: slog.New(slog.DiscardHandler),
		runner: runner,
	}

	if _, err := manager.startRuns(3, cfg); err != nil {
		t.Fatalf("start runs: %v", err)
	}

	if got := runner.virtiofsEnv()["virtiofsd-workspace"]; !containsString(got, "VIRTIOFSD_SOCKET="+wantSocket) {
		t.Fatalf("expected virtiofs run env to contain resolved socket path %q: %v", wantSocket, got)
	}
	if !runner.processGroups()["virtiofsd-workspace"] {
		t.Fatal("expected virtiofs run to run in its own process group")
	}
}

func assertLaunchStatsLog(t *testing.T, logs string, want []string, unwanted []string) {
	t.Helper()

	if !strings.Contains(logs, "stats: ") {
		t.Fatalf("expected launch stats log, got %q", logs)
	}
	for _, field := range want {
		if !strings.Contains(logs, field) {
			t.Fatalf("expected launch stats field %q in logs %q", field, logs)
		}
	}
	for _, field := range unwanted {
		if strings.Contains(logs, field) {
			t.Fatalf("unexpected launch stats field %q in logs %q", field, logs)
		}
	}
}

func validManifest(workingDir string) *manifest.Manifest {
	return &manifest.Manifest{
		Identity: manifest.Identity{HostName: "agent-sandbox"},
		Paths: manifest.Paths{
			WorkingDir: workingDir,
			LockPath:   "/tmp/virtle.lock",
		},
		SSH: manifest.SSH{
			Argv: []string{"/bin/ssh"},
			User: "agent",
		},
		QEMU: manifest.QEMU{
			BinaryPath: "/bin/qemu-system-x86_64",
			Name:       "agent-sandbox",
			Machine: manifest.QEMUMachine{
				Type:    "microvm",
				Options: []string{"accel=kvm:tcg"},
			},
			CPU: manifest.QEMUCPU{
				Model:     "host",
				EnableKVM: true,
			},
			Memory: manifest.QEMUMemory{
				Size:    1024,
				Backend: "memfd",
				Shared:  true,
			},
			Kernel: manifest.QEMUKernel{
				Path:       "/tmp/vmlinuz",
				InitrdPath: "/tmp/initrd",
				Params:     "panic=-1",
			},
			SMP: manifest.QEMUSMP{
				CPUs: manifest.ExplicitCPUs(2),
			},
			Console: manifest.QEMUConsolePrint,
			Knobs: manifest.QEMUKnobs{
				NoDefaults:   true,
				NoUserConfig: true,
				NoReboot:     true,
				NoGraphic:    true,
			},
			QMP: manifest.QEMUQMP{
				SocketPath: "qmp.sock",
			},
			SSHReady: manifest.QEMUSSHReady{
				SocketPath: "ready.sock",
			},
			Devices: manifest.QEMUDevices{
				RNG: manifest.QEMURNGDevice{
					ID:        "rng0",
					Transport: "pci",
				},
				VirtioFS: []manifest.QEMUVirtioFSShare{
					{
						ID:         "fs0",
						SocketPath: "fs.sock",
						Tag:        "workspace",
						Transport:  "pci",
					},
				},
				Block: []manifest.QEMUBlockDevice{
					{
						ID:        "vda",
						ImagePath: "root.img",
						AIO:       "threads",
						Transport: "pci",
					},
				},
				Network: []manifest.QEMUNetDevice{
					{
						ID:         "net0",
						Backend:    "user",
						MacAddress: "02:02:00:00:00:01",
						Transport:  "pci",
					},
				},
				VSOCK: manifest.QEMUVSOCKDevice{
					ID:        "vsock0",
					Transport: "pci",
				},
			},
		},
		Volumes: []manifest.Volume{
			{
				ImagePath:  "root.img",
				Size:       256,
				FSType:     "ext4",
				AutoCreate: true,
			},
		},
		Run: []manifest.Run{
			{
				Exec: []string{"/tmp/virtiofsd-workspace", "--socket-path={{.Socket}}", "--shared-dir={{.MountSource}}", "--tag={{.MountTag}}"},
				Env:  []string{"VIRTIOFSD_SOCKET={{.Socket}}"},
				Vars: map[string]any{
					"Socket":      filepath.Join(workingDir, "fs.sock"),
					"MountTag":    "workspace",
					"MountSource": workingDir,
				},
			},
		},
		CleanupFiles: []string{"fs.sock"},
	}
}

func validManifestWithBalloon(workingDir string) *manifest.Manifest {
	manifest := validManifest(workingDir)
	manifest.QEMU.Devices.Balloon = &balloon.Device{
		ID:        "balloon0",
		Transport: "pci",
	}
	return manifest
}

type launchRunner struct {
	base                      *executortest.Runner
	mu                        sync.Mutex
	interactiveStarts         int
	cancel                    context.CancelFunc
	cancelDelay               time.Duration
	failInteractiveSSH        bool
	finishInteractiveSSH      bool
	finishInteractiveSSHDelay time.Duration
	transientSSHFailures      int
	transientSSHOutputs       []string
	authSSHFailures           int
	startErrors               map[string]error
	qemu                      *executortest.Process
	onStart                   func(name string, cmd *exec.Cmd)
}

func (r *launchRunner) Start(cmd *exec.Cmd) (*executor.Process, error) {
	r.ensureBase()
	r.base.StartErrors = r.startErrors
	r.base.OnStart = r.startProcess

	return r.base.Start(cmd)
}

func (r *launchRunner) ensureBase() {
	if r.base == nil {
		r.base = &executortest.Runner{}
	}
}

func (r *launchRunner) startProcess(start executortest.Start) (*executortest.Process, error) {
	name := start.Name
	if r.onStart != nil {
		r.onStart(name, start.Cmd)
	}
	switch {
	case strings.HasPrefix(name, "qemu-system"):
		process := &executortest.Process{OverrideName: name}
		r.mu.Lock()
		r.qemu = process
		r.mu.Unlock()
		return process, nil
	case name == "ssh":
		r.mu.Lock()
		r.interactiveStarts++
		interactiveStarts := r.interactiveStarts
		r.mu.Unlock()
		if interactiveStarts <= r.authSSHFailures {
			if start.Cmd.Stderr != nil {
				_, _ = io.WriteString(start.Cmd.Stderr, "agent@vsock/3: Permission denied (publickey).\n")
			}
			return &executortest.Process{OverrideName: name, Exited: true, WaitErr: errors.New("exit status 255")}, nil
		}
		if interactiveStarts <= r.transientSSHFailures {
			if start.Cmd.Stderr != nil {
				output := "ssh: connect to host vsock/3 port 22: Connection refused\n"
				if index := interactiveStarts - 1; index < len(r.transientSSHOutputs) {
					output = r.transientSSHOutputs[index]
				}
				_, _ = io.WriteString(start.Cmd.Stderr, output)
			}
			return &executortest.Process{OverrideName: name, Exited: true, WaitErr: errors.New("exit status 255")}, nil
		}
		if r.failInteractiveSSH {
			return nil, errors.New("session start failed")
		}
		if r.finishInteractiveSSH {
			process := &executortest.Process{OverrideName: name}
			go func() {
				if r.finishInteractiveSSHDelay > 0 {
					time.Sleep(r.finishInteractiveSSHDelay)
				}
				process.Complete(nil)
			}()
			return process, nil
		}

		process := &executortest.Process{OverrideName: name}
		go func() {
			if r.cancelDelay > 0 {
				time.Sleep(r.cancelDelay)
			}
			if r.cancel != nil {
				r.cancel()
			}
		}()
		return process, nil
	default:
		return nil, nil
	}
}

func (r *launchRunner) starts() []executortest.Start {
	r.ensureBase()
	return r.base.Starts()
}

func (r *launchRunner) startedNames() []string {
	r.ensureBase()
	return r.base.StartedNames()
}

func (r *launchRunner) signalNames() []string {
	r.ensureBase()
	return r.base.SignalNames()
}

func (r *launchRunner) processSignals() []processSignal {
	r.ensureBase()
	signals := r.base.ProcessSignals()
	processSignals := make([]processSignal, 0, len(signals))
	for _, signal := range signals {
		processSignals = append(processSignals, processSignal{name: signal.Name, sig: signal.Signal})
	}
	return processSignals
}

func (r *launchRunner) qemuArgs() []string {
	return r.firstArgs(func(start executortest.Start) bool {
		return strings.HasPrefix(start.Name, "qemu-system")
	})
}

func (r *launchRunner) qemuEnv() []string {
	return r.firstEnv(func(start executortest.Start) bool {
		return strings.HasPrefix(start.Name, "qemu-system")
	})
}

func (r *launchRunner) sshArgs() [][]string {
	var args [][]string
	for _, start := range r.starts() {
		if start.Name == "ssh" {
			args = append(args, append([]string(nil), start.Args...))
		}
	}
	return args
}

func (r *launchRunner) runArgs() map[string][]string {
	values := make(map[string][]string)
	for _, start := range r.starts() {
		if start.Name != "ssh" && !strings.HasPrefix(start.Name, "qemu-system") && !strings.HasPrefix(start.Name, "virtiofsd") {
			values[start.Name] = append([]string(nil), start.Args...)
		}
	}
	return values
}

func (r *launchRunner) runEnv() map[string][]string {
	values := make(map[string][]string)
	for _, start := range r.starts() {
		if start.Name != "ssh" && !strings.HasPrefix(start.Name, "qemu-system") && !strings.HasPrefix(start.Name, "virtiofsd") {
			values[start.Name] = append([]string(nil), start.EnvAdditions...)
		}
	}
	return values
}

func (r *launchRunner) virtiofsEnv() map[string][]string {
	values := make(map[string][]string)
	for _, start := range r.starts() {
		if strings.HasPrefix(start.Name, "virtiofsd") {
			values[start.Name] = append([]string(nil), start.EnvAdditions...)
		}
	}
	return values
}

func (r *launchRunner) processGroups() map[string]bool {
	values := make(map[string]bool)
	for _, start := range r.starts() {
		values[start.Name] = start.ProcessGroup
	}
	return values
}

func (r *launchRunner) processDirs() map[string]string {
	values := make(map[string]string)
	for _, start := range r.starts() {
		values[start.Name] = start.Dir
	}
	return values
}

func (r *launchRunner) firstArgs(match func(executortest.Start) bool) []string {
	for _, start := range r.starts() {
		if match(start) {
			return append([]string(nil), start.Args...)
		}
	}
	return nil
}

func (r *launchRunner) firstEnv(match func(executortest.Start) bool) []string {
	for _, start := range r.starts() {
		if match(start) {
			return append([]string(nil), start.EnvAdditions...)
		}
	}
	return nil
}

func commandArgs(cmd *exec.Cmd) []string {
	if cmd == nil || len(cmd.Args) == 0 {
		return nil
	}
	return cmd.Args[1:]
}

func commandEnvAdditions(env []string) []string {
	environ := os.Environ()
	if len(env) < len(environ) {
		return env
	}
	for i, entry := range environ {
		if env[i] != entry {
			return env
		}
	}
	return env[len(environ):]
}

func commandProcessGroup(cmd *exec.Cmd) bool {
	return cmd != nil && cmd.SysProcAttr != nil && cmd.SysProcAttr.Setpgid
}

func (r *launchRunner) exitQEMU(err error) {
	r.mu.Lock()
	process := r.qemu
	r.mu.Unlock()
	if process == nil {
		return
	}
	process.Complete(err)
}

type fakeSocketWaiter struct {
	calls          int
	paths          [][]string
	noAutoSSHReady bool
	callback       func(paths []string) error
}

func (w *fakeSocketWaiter) Wait(ctx context.Context, socketPaths []string) error {
	w.calls++
	w.paths = append(w.paths, append([]string(nil), socketPaths...))
	if !w.noAutoSSHReady && len(socketPaths) == 1 && filepath.Base(socketPaths[0]) == "ready.sock" {
		return startFakeSSHReadySocket(socketPaths[0])
	}
	if w.callback == nil {
		return nil
	}
	return w.callback(socketPaths)
}

func startFakeSSHReadySocket(path string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	_ = os.Remove(path)
	listener, err := net.Listen("unix", path)
	if err != nil {
		return err
	}
	go func() {
		conn, err := listener.Accept()
		_ = listener.Close()
		if err != nil {
			return
		}
		defer conn.Close()
		_, _ = io.WriteString(conn, SSHReadyToken+"\n")
	}()
	return nil
}

type fakeQMPDialer struct {
	client   qmpclient.Client
	attempts int
}

func (d *fakeQMPDialer) Dial(ctx context.Context, socketPath string, timeout time.Duration) (qmpclient.Client, error) {
	d.attempts++
	return d.client, nil
}

type fakeGuestAgentDialer struct {
	client   qga.Client
	attempts int
}

func (d *fakeGuestAgentDialer) Dial(ctx context.Context, socketPath string, timeout time.Duration) (qga.Client, error) {
	d.attempts++
	return d.client, nil
}

type fakeSSHReadyDialer struct {
	data     string
	err      error
	attempts int
	record   func(string)
	block    bool
}

type fakeVSockCIDChecker struct {
	unavailable map[int]bool
	err         error
	checked     []int
}

func (c *fakeVSockCIDChecker) Available(cid int) (bool, error) {
	c.checked = append(c.checked, cid)
	if c.err != nil {
		return false, c.err
	}
	return !c.unavailable[cid], nil
}

func (d *fakeSSHReadyDialer) Dial(ctx context.Context, socketPath string, timeout time.Duration) (io.ReadCloser, error) {
	d.attempts++
	if d.record != nil {
		d.record("ssh-ready-dial:" + socketPath)
	}
	if d.err != nil {
		return nil, d.err
	}
	if d.block {
		reader, _ := io.Pipe()
		return reader, nil
	}
	if d.data == "" {
		if conn, err := net.Dial("unix", socketPath); err == nil {
			return conn, nil
		}
	}
	data := d.data
	if data == "" {
		data = SSHReadyToken
	}
	return io.NopCloser(strings.NewReader(data)), nil
}

type fakeGuestAgentClient struct {
	mu              sync.Mutex
	nextHandle      int
	handles         map[int]string
	writes          map[string]string
	readPayloads    map[string][]string
	readIndexes     map[string]int
	closes          []string
	execs           []guestExecCall
	execStatuses    []qga.ExecStatus
	execStatusCalls int
	readErr         error
	writeErr        error
	closeErr        error
	execErr         error
	execStatusErr   error
	pingErr         error
	openErr         error
	disconnects     int
	record          func(string)
}

type guestExecCall struct {
	path          string
	args          []string
	captureOutput bool
}

func (c *fakeGuestAgentClient) Ping(timeout time.Duration) error {
	if c.record != nil {
		c.record("guest-ping")
	}
	return c.pingErr
}

func (c *fakeGuestAgentClient) OpenFile(timeout time.Duration, path string) (int, error) {
	if c.record != nil {
		c.record("guest-open:" + path)
	}
	if c.openErr != nil {
		return 0, c.openErr
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	if c.handles == nil {
		c.handles = make(map[int]string)
	}
	c.nextHandle++
	c.handles[c.nextHandle] = path
	return c.nextHandle, nil
}

func (c *fakeGuestAgentClient) OpenFileRead(timeout time.Duration, path string) (int, error) {
	if c.record != nil {
		c.record("guest-open-read:" + path)
	}
	if c.openErr != nil {
		return 0, c.openErr
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	if c.handles == nil {
		c.handles = make(map[int]string)
	}
	c.nextHandle++
	c.handles[c.nextHandle] = path
	return c.nextHandle, nil
}

func (c *fakeGuestAgentClient) ReadFile(timeout time.Duration, handle int, count int) (string, bool, error) {
	c.mu.Lock()
	path := c.handles[handle]
	index := c.readIndexes[path]
	payloads := c.readPayloads[path]
	if c.readIndexes == nil {
		c.readIndexes = make(map[string]int)
	}
	if index < len(payloads) {
		c.readIndexes[path] = index + 1
	}
	c.mu.Unlock()

	if c.record != nil {
		c.record("guest-read:" + path)
	}
	if c.readErr != nil {
		return "", false, c.readErr
	}
	if index >= len(payloads) {
		return "", true, nil
	}
	return payloads[index], index == len(payloads)-1, nil
}

func (c *fakeGuestAgentClient) WriteFile(timeout time.Duration, handle int, payloadBase64 string) error {
	c.mu.Lock()
	path := c.handles[handle]
	if c.writes == nil {
		c.writes = make(map[string]string)
	}
	c.writes[path] = payloadBase64
	c.mu.Unlock()

	if c.record != nil {
		c.record("guest-write:" + path)
	}
	return c.writeErr
}

func (c *fakeGuestAgentClient) CloseFile(timeout time.Duration, handle int) error {
	c.mu.Lock()
	path := c.handles[handle]
	c.closes = append(c.closes, path)
	c.mu.Unlock()

	if c.record != nil {
		c.record("guest-close:" + path)
	}
	return c.closeErr
}

func (c *fakeGuestAgentClient) Exec(timeout time.Duration, path string, args []string, captureOutput bool) (int, error) {
	c.mu.Lock()
	c.execs = append(c.execs, guestExecCall{
		path:          path,
		args:          append([]string(nil), args...),
		captureOutput: captureOutput,
	})
	pid := len(c.execs)
	c.mu.Unlock()

	if c.record != nil && path == guestChownPath && len(args) == 2 {
		c.record("guest-chown:" + args[1] + ":" + args[0])
	}
	if c.record != nil && path == guestChmodPath && len(args) == 2 {
		c.record("guest-chmod:" + args[1] + ":" + args[0])
	}
	if c.record != nil && path == guestInstallPath && len(args) > 0 {
		c.record("guest-install-dir:" + args[len(args)-1])
	}
	if c.record != nil && path == guestTestPath && len(args) > 0 {
		c.record("guest-test-dir:" + args[len(args)-1])
	}
	if c.record != nil && path == guestPSPath {
		c.record("guest-ps")
	}
	if c.execErr != nil {
		return 0, c.execErr
	}
	return pid, nil
}

func (c *fakeGuestAgentClient) ExecStatus(timeout time.Duration, pid int) (qga.ExecStatus, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.execStatusCalls++
	if c.execStatusErr != nil {
		return qga.ExecStatus{}, c.execStatusErr
	}
	if len(c.execStatuses) == 0 {
		return qga.ExecStatus{Exited: true}, nil
	}
	index := c.execStatusCalls - 1
	if index >= len(c.execStatuses) {
		index = len(c.execStatuses) - 1
	}
	return c.execStatuses[index], nil
}

func (c *fakeGuestAgentClient) Disconnect() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.disconnects++
	return nil
}

type pidSignal struct {
	pid int
	sig os.Signal
}

type processSignal struct {
	name string
	sig  os.Signal
}

type fakePIDSignaler struct {
	existsErr error
	signalErr error
	signals   []pidSignal
	onSignal  func(pid int, sig os.Signal) error
}

func (s *fakePIDSignaler) Exists(pid int) error {
	return s.existsErr
}

func (s *fakePIDSignaler) Signal(pid int, sig os.Signal) error {
	s.signals = append(s.signals, pidSignal{pid: pid, sig: sig})
	if s.onSignal != nil {
		return s.onSignal(pid, sig)
	}
	return s.signalErr
}

func acquireTestLaunchLock(t *testing.T, manifest *manifest.Manifest, pid int) func() {
	t.Helper()

	path := manifest.ResolvedLockPath()
	return acquireTestLockFile(t, path, pid)
}

func acquireTestLockFile(t *testing.T, path string, pid int) func() {
	t.Helper()

	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("create lock directory: %v", err)
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR|os.O_TRUNC, 0o600)
	if err != nil {
		t.Fatalf("open lock: %v", err)
	}
	if err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		file.Close()
		t.Fatalf("flock lock: %v", err)
	}
	if _, err := file.WriteString(fmt.Sprintf("%d\n", pid)); err != nil {
		_ = syscall.Flock(int(file.Fd()), syscall.LOCK_UN)
		file.Close()
		t.Fatalf("write lock pid: %v", err)
	}

	return func() {
		_ = syscall.Flock(int(file.Fd()), syscall.LOCK_UN)
		_ = file.Close()
	}
}

type fakeQMPClient struct {
	mu                       sync.Mutex
	quitCalls                int
	stopCalls                int
	contCalls                int
	migrateCalls             int
	migrateIncomingCalls     int
	queryMigrateCalls        int
	queryStatusCalls         int
	disconnectCalls          int
	rawCommands              []string
	deviceDelWaits           []string
	status                   string
	migrationStatus          string
	migratePath              string
	migrateIncomingPath      string
	onQuit                   func()
	onStop                   func()
	onCont                   func()
	onEnableBalloonStats     func()
	listQOMProperties        map[string][]fakeQOMProperty
	listQOMPropertiesErr     map[string]error
	enableBalloonStatsErr    error
	queryBalloonActualBytes  int64
	queryBalloonErr          error
	readBalloonStats         map[string]int64
	readBalloonStatsErr      error
	readBalloonStatsDelay    time.Duration
	readBalloonStatsComplete time.Time
	readBalloonStatsUpdated  time.Time
	setBalloonLogicalSizes   []int64
	setBalloonErr            error
}

func (c *fakeQMPClient) RunRaw(timeout time.Duration, command string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.rawCommands = append(c.rawCommands, command)
	return nil
}

func (c *fakeQMPClient) DeviceDelAndWait(timeout time.Duration, id string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.rawCommands = append(c.rawCommands, `{"execute":"device_del","arguments":{"id":"`+id+`"}}`)
	c.deviceDelWaits = append(c.deviceDelWaits, id)
	return nil
}

func (c *fakeQMPClient) Quit(timeout time.Duration) error {
	c.mu.Lock()
	c.quitCalls++
	onQuit := c.onQuit
	c.mu.Unlock()

	if onQuit != nil {
		onQuit()
	}
	return nil
}

func (c *fakeQMPClient) Stop(timeout time.Duration) error {
	c.mu.Lock()
	c.stopCalls++
	c.status = "paused"
	onStop := c.onStop
	c.mu.Unlock()
	if onStop != nil {
		onStop()
	}
	return nil
}

func (c *fakeQMPClient) Cont(timeout time.Duration) error {
	c.mu.Lock()
	c.contCalls++
	c.status = "running"
	onCont := c.onCont
	c.mu.Unlock()
	if onCont != nil {
		onCont()
	}
	return nil
}

func (c *fakeQMPClient) QueryStatus(timeout time.Duration) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.queryStatusCalls++
	if c.status == "" {
		c.status = "running"
	}
	return c.status, nil
}

func (c *fakeQMPClient) MigrateToFile(timeout time.Duration, path string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.migrateCalls++
	c.migratePath = path
	c.migrationStatus = "completed"
	return nil
}

func (c *fakeQMPClient) MigrateIncoming(timeout time.Duration, path string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.migrateIncomingCalls++
	c.migrateIncomingPath = path
	c.migrationStatus = "completed"
	return nil
}

func (c *fakeQMPClient) QueryMigrate(timeout time.Duration) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.queryMigrateCalls++
	if c.migrationStatus == "" {
		c.migrationStatus = "completed"
	}
	return c.migrationStatus, nil
}

func (c *fakeQMPClient) readCompletionTime() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.readBalloonStatsComplete
}

func (c *fakeQMPClient) quitCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.quitCalls
}

func (c *fakeQMPClient) Disconnect() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.disconnectCalls++
	return nil
}

func (c *fakeQMPClient) withDefaultBalloonPath(path string) *fakeQMPClient {
	c.listQOMProperties = map[string][]fakeQOMProperty{
		path: {
			{Name: "guest-stats", Type: "dict"},
			{Name: "guest-stats-polling-interval", Type: "int"},
		},
	}
	return c
}

func (c *fakeQMPClient) WithRaw(timeout time.Duration, fn func(*rawQMP.Monitor) error) error {
	return fn(rawQMP.NewMonitor(&fakeMonitor{handler: c.handleQMP}))
}

type fakeQOMProperty struct {
	Name string
	Type string
}

type fakeMonitor struct {
	handler func(message map[string]any) (map[string]any, error)
}

func (m *fakeMonitor) Connect() error {
	return nil
}

func (m *fakeMonitor) Disconnect() error {
	return nil
}

func (m *fakeMonitor) Run(command []byte) ([]byte, error) {
	var message map[string]any
	if err := json.Unmarshal(command, &message); err != nil {
		return nil, err
	}

	response := map[string]any{"return": map[string]any{}}
	var err error
	if m.handler != nil {
		response, err = m.handler(message)
		if err != nil {
			return nil, err
		}
	}
	return json.Marshal(response)
}

func (m *fakeMonitor) Events(context.Context) (<-chan doQMP.Event, error) {
	return nil, doQMP.ErrEventsNotSupported
}

func (c *fakeQMPClient) handleQMP(message map[string]any) (map[string]any, error) {
	command, _ := message["execute"].(string)
	args, _ := message["arguments"].(map[string]any)

	switch command {
	case "query-status":
		status, err := c.QueryStatus(time.Second)
		if err != nil {
			return nil, err
		}
		return map[string]any{
			"return": map[string]any{
				"running":    status == "running",
				"singlestep": false,
				"status":     status,
			},
		}, nil
	case "stop":
		if err := c.Stop(time.Second); err != nil {
			return nil, err
		}
		return map[string]any{"return": map[string]any{}}, nil
	case "cont":
		if err := c.Cont(time.Second); err != nil {
			return nil, err
		}
		return map[string]any{"return": map[string]any{}}, nil
	case "quit":
		c.mu.Lock()
		c.quitCalls++
		onQuit := c.onQuit
		c.mu.Unlock()
		if onQuit != nil {
			onQuit()
		}
		return map[string]any{"return": map[string]any{}}, nil
	case "migrate":
		uri, _ := args["uri"].(string)
		c.mu.Lock()
		c.migrateCalls++
		c.migratePath = strings.TrimPrefix(uri, "file:")
		c.migrationStatus = "completed"
		c.mu.Unlock()
		return map[string]any{"return": map[string]any{}}, nil
	case "migrate-incoming":
		uri, _ := args["uri"].(string)
		c.mu.Lock()
		c.migrateIncomingCalls++
		c.migrateIncomingPath = strings.TrimPrefix(uri, "file:")
		c.migrationStatus = "completed"
		c.mu.Unlock()
		return map[string]any{"return": map[string]any{}}, nil
	case "query-migrate":
		status, err := c.QueryMigrate(time.Second)
		if err != nil {
			return nil, err
		}
		return map[string]any{"return": map[string]any{"status": status}}, nil
	case "query-balloon":
		c.mu.Lock()
		actualBytes := c.queryBalloonActualBytes
		err := c.queryBalloonErr
		c.mu.Unlock()
		if err != nil {
			return nil, err
		}
		if actualBytes == 0 {
			actualBytes = 512 * testMiB
		}
		return map[string]any{"return": map[string]any{"actual": actualBytes}}, nil
	case "balloon":
		value, _ := args["value"].(float64)
		c.mu.Lock()
		c.setBalloonLogicalSizes = append(c.setBalloonLogicalSizes, int64(value))
		err := c.setBalloonErr
		c.mu.Unlock()
		if err != nil {
			return nil, err
		}
		return map[string]any{"return": map[string]any{}}, nil
	case "qom-set":
		property, _ := args["property"].(string)
		if property == "guest-stats-polling-interval" {
			c.mu.Lock()
			onEnable := c.onEnableBalloonStats
			err := c.enableBalloonStatsErr
			c.mu.Unlock()
			if onEnable != nil {
				onEnable()
			}
			if err != nil {
				return nil, err
			}
		}
		return map[string]any{"return": map[string]any{}}, nil
	case "qom-get":
		c.mu.Lock()
		delay := c.readBalloonStatsDelay
		err := c.readBalloonStatsErr
		snapshot := mapsClone(c.readBalloonStats)
		updated := c.readBalloonStatsUpdated
		c.mu.Unlock()

		if delay > 0 {
			time.Sleep(delay)
		}

		c.mu.Lock()
		c.readBalloonStatsComplete = time.Now()
		c.mu.Unlock()

		if err != nil {
			return nil, err
		}
		if len(snapshot) == 0 {
			snapshot = map[string]int64{
				"stat-available-memory": 768 * testMiB,
			}
		}
		if updated.IsZero() {
			updated = time.Now()
		}
		return map[string]any{
			"return": map[string]any{
				"stats":       snapshot,
				"last-update": updated.Unix(),
			},
		}, nil
	case "qom-list":
		path, _ := args["path"].(string)
		c.mu.Lock()
		err := c.listQOMPropertiesErr[path]
		props, ok := c.listQOMProperties[path]
		c.mu.Unlock()
		if err != nil {
			return nil, err
		}
		if !ok {
			return nil, errors.New("unexpected qom-list path")
		}
		entries := make([]map[string]any, 0, len(props))
		for _, prop := range props {
			entries = append(entries, map[string]any{
				"name": prop.Name,
				"type": prop.Type,
			})
		}
		return map[string]any{"return": entries}, nil
	default:
		return nil, errors.New("unexpected qmp command")
	}
}

func mapsClone(src map[string]int64) map[string]int64 {
	if src == nil {
		return nil
	}
	dst := make(map[string]int64, len(src))
	for key, value := range src {
		dst[key] = value
	}
	return dst
}

func containsString(values []string, needle string) bool {
	for _, value := range values {
		if strings.Contains(value, needle) {
			return true
		}
	}
	return false
}

func containsGuestExec(calls []guestExecCall, path string, argNeedle string) bool {
	for _, call := range calls {
		if call.path != path {
			continue
		}
		for _, arg := range call.args {
			if strings.Contains(arg, argNeedle) {
				return true
			}
		}
	}
	return false
}

func indexString(values []string, needle string) int {
	for i, value := range values {
		if value == needle {
			return i
		}
	}
	return -1
}

func indexStringContaining(values []string, needle string) int {
	for i, value := range values {
		if strings.Contains(value, needle) {
			return i
		}
	}
	return -1
}

func stringPtr(value string) *string {
	return &value
}

func intPtr(value int) *int {
	return &value
}

func boolPtr(value bool) *bool {
	return &value
}

func setXDGTestRuntimeDir(t *testing.T, runtimeDir string) {
	t.Helper()

	original, hadOriginal := os.LookupEnv("XDG_RUNTIME_DIR")
	if err := os.Setenv("XDG_RUNTIME_DIR", runtimeDir); err != nil {
		t.Fatalf("set XDG_RUNTIME_DIR: %v", err)
	}
	xdg.Reload()

	t.Cleanup(func() {
		var err error
		if hadOriginal {
			err = os.Setenv("XDG_RUNTIME_DIR", original)
		} else {
			err = os.Unsetenv("XDG_RUNTIME_DIR")
		}
		if err != nil {
			t.Fatalf("restore XDG_RUNTIME_DIR: %v", err)
		}
		xdg.Reload()
	})
}
