package vmm

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
	"os/user"
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
	"github.com/shazow/virtle/backend"
	"github.com/shazow/virtle/backend/qemu/internal/launch"
	"github.com/shazow/virtle/backend/qemu/internal/qga"
	"github.com/shazow/virtle/backend/qemu/internal/qmpclient"
	"github.com/shazow/virtle/backend/qemu/limits"
	control "github.com/shazow/virtle/internal/control"
	"github.com/shazow/virtle/internal/executor"
	"github.com/shazow/virtle/internal/executor/executortest"
	"github.com/shazow/virtle/internal/manifest"
	"github.com/shazow/virtle/units"
	"github.com/shazow/virtle/vm"
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
	plan, err := manager.planLaunch(launch.Spec{Manifest: cfg})
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
		logger: debugTestLogger(&logOutput),
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

func TestManagerDefaultRunnerLogsCommandOutput(t *testing.T) {
	if os.Getenv("VMM_RUNNER_OUTPUT_CHILD") == "1" {
		fmt.Fprintln(os.Stdout, "managed output")
		os.Exit(0)
	}

	exe, err := os.Executable()
	if err != nil {
		t.Fatalf("resolve test executable: %v", err)
	}
	var logs bytes.Buffer
	manager := newManagerFromConfig(Config{Logger: debugTestLogger(&logs)})
	cmd := exec.Command(exe, "-test.run=TestManagerDefaultRunnerLogsCommandOutput")
	cmd.Env = append(os.Environ(), "VMM_RUNNER_OUTPUT_CHILD=1")
	process, err := manager.startManagedProcess(cmd)
	if err != nil {
		t.Fatalf("start command: %v", err)
	}
	if err := process.Wait(); err != nil {
		t.Fatalf("wait command: %v", err)
	}

	output := logs.String()
	for _, want := range []string{"level=DEBUG", "package=vmm", "stream=stdout", `msg="managed output"`} {
		if !strings.Contains(output, want) {
			t.Fatalf("expected manager logs to contain %q, got %q", want, output)
		}
	}
}

func TestManagerStartQEMUNilRunnerWrapsOnce(t *testing.T) {
	tmpDir := t.TempDir()
	manager := newLaunchProviderTestManager(nil)
	cfg := validProviderLaunchManifest(tmpDir)
	plan, err := manager.planLaunch(launch.Spec{Manifest: cfg})
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

	manager := &manager{}
	plan, err := manager.planLaunch(launch.Spec{Manifest: cfg, Options: LaunchOptions{Resume: ResumeModeNo}})
	if err != nil {
		t.Fatalf("plan launch: %v", err)
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
	plan, err := newManagerFromConfig(DefaultConfig()).planLaunch(launch.Spec{
		Manifest: cfg,
		Options:  LaunchOptions{Resume: ResumeModeNo},
	})
	if err != nil {
		t.Fatalf("manager plan: %v", err)
	}

	if got, want := plan.Paths.ControlSocket, filepath.Join(tmpDir, "virtle.sock"); got != want {
		t.Fatalf("unexpected control socket path: got %q want %q", got, want)
	}
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
		logger:            debugTestLogger(&logOutput),
		shutdownDelay:     10 * time.Millisecond,
		qmpRetryDelay:     0,
		qmpConnectTimeout: time.Millisecond,
		qmpQuitTimeout:    time.Millisecond,
	}

	if err := manager.launchWithOptions(context.Background(), cfg, LaunchOptions{Resume: ResumeModeNo}); err != nil {
		t.Fatalf("launch with run: %v", err)
	}

	runName := "proxy"
	if !containsString(runner.startedNames(), runName) {
		t.Fatalf("expected run process start in %v", runner.startedNames())
	}
	if got, want := runner.startedNames(), []string{"virtiofsd-workspace", runName, "qemu-system-x86_64"}; !reflect.DeepEqual(got, want) {
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
		logger:        debugTestLogger(&logOutput),
		shutdownDelay: 10 * time.Millisecond,
	}

	err := manager.launchWithOptions(context.Background(), cfg, LaunchOptions{Resume: ResumeModeNo})
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
	if !strings.Contains(logOutput.String(), `msg="launch stats"`) {
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
		logger:        debugTestLogger(&logOutput),
		shutdownDelay: 10 * time.Millisecond,
	}

	err := manager.launchWithOptions(context.Background(), cfg, LaunchOptions{Resume: ResumeModeNo})
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
		logger:            debugTestLogger(&logOutput),
		shutdownDelay:     10 * time.Millisecond,
		qmpRetryDelay:     0,
		qmpConnectTimeout: time.Millisecond,
	}

	err := manager.launchWithOptions(context.Background(), cfg, LaunchOptions{Resume: ResumeModeNo})
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

func TestCreateVolumeImageCreatesNativeExt4(t *testing.T) {
	account, err := user.Current()
	if err != nil {
		t.Fatalf("current user: %v", err)
	}
	label := "persist"
	for _, tt := range []struct {
		name      string
		sizeMiB   units.MiB
		label     string
		wantLabel string
		qemuOwned bool
	}{
		{name: "minimum-size-without-label", sizeMiB: 256, qemuOwned: true},
		{name: "minimum-size-with-label", sizeMiB: 256, label: label, wantLabel: label},
		{name: "default-home-size", sizeMiB: 4096, label: label, wantLabel: label},
	} {
		t.Run(tt.name, func(t *testing.T) {
			tmpDir := t.TempDir()
			imagePath := filepath.Join(tmpDir, "volume.img")
			runAsUser := ""
			if tt.qemuOwned {
				runAsUser = account.Username
			}

			err := launch.CreateVolumeImage(manifest.Volume{
				ImagePath:  imagePath,
				Size:       tt.sizeMiB,
				FSType:     "ext4",
				AutoCreate: true,
				Label:      tt.label,
			}, runAsUser)
			if err != nil {
				t.Fatalf("create volume image: %v", err)
			}

			info, err := os.Stat(imagePath)
			if err != nil {
				t.Fatalf("expected volume image to exist: %v", err)
			}
			if got, want := info.Size(), tt.sizeMiB.Bytes().Int64(); got != want {
				t.Fatalf("unexpected volume size: got %d want %d", got, want)
			}
			if got, want := info.Mode().Perm(), os.FileMode(0o600); got != want {
				t.Fatalf("unexpected volume mode: got %o want %o", got, want)
			}
			if tt.qemuOwned {
				stat, ok := info.Sys().(*syscall.Stat_t)
				if !ok {
					t.Fatalf("volume stat has type %T", info.Sys())
				}
				if int(stat.Uid) != os.Getuid() || int(stat.Gid) != os.Getgid() {
					t.Fatalf("volume ownership: got %d:%d want %d:%d", stat.Uid, stat.Gid, os.Getuid(), os.Getgid())
				}
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
	if err := os.WriteFile(chattrPath, []byte("#!/bin/sh\nset -eu\nstat -c '%s' \"$2\" > \"$CHATTR_LOG\"\n"), 0o755); err != nil {
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
	}, ""); err != nil {
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

	manager.launchManifest = cfg
	if err := manager.writeGuestFiles(context.Background(), nil, executor.Group{}); err != nil {
		t.Fatalf("mount workspace cwd: %v", err)
	}

	want := []guestExecCall{
		{
			path:          "install",
			args:          []string{"-d", "/home/agent/workspace", "/home/agent/workspace/agentspace"},
			env:           []string{qga.InternalCommandPathEnv},
			captureOutput: true,
		},
		{
			path:          "mount",
			args:          []string{"--bind", "/mnt/cwd", "/home/agent/workspace/agentspace"},
			env:           []string{qga.InternalCommandPathEnv},
			captureOutput: true,
		},
	}
	if !reflect.DeepEqual(guestAgent.execs, want) {
		t.Fatalf("unexpected guest execs: got %#v want %#v", guestAgent.execs, want)
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

	runner := &launchRunner{}
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
		logger:            slog.New(slog.DiscardHandler),
		shutdownDelay:     10 * time.Millisecond,
		qmpRetryDelay:     0,
		qmpConnectTimeout: 100 * time.Millisecond,
		qmpQuitTimeout:    time.Millisecond,
	}

	if err := manager.launch(context.Background(), cfg); err != nil {
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
	manager.launchManifest = cfg
	handler := newLaunchSuspendHandler(manager, filepath.Join(tmpDir, "qmp.sock"), qmpClient, 7, nil, func() bool {
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

	manager.launchManifest = cfg
	err := manager.writeBackGuestFiles(context.Background(), executor.Group{})
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

	manager.launchManifest = cfg
	if err := manager.writeBackGuestFiles(context.Background(), executor.Group{}); err != nil {
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

func TestManagerLaunchRunsGuestDirectoryInstallScript(t *testing.T) {
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

	runner := &launchRunner{}
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
		shutdownDelay:     10 * time.Millisecond,
		qmpRetryDelay:     0,
		qmpConnectTimeout: 100 * time.Millisecond,
		qmpQuitTimeout:    time.Millisecond,
	}

	if err := manager.launch(context.Background(), cfg); err != nil {
		t.Fatalf("launch: %v", err)
	}

	if got, want := guestAgent.execs, []guestExecCall{
		guestDirInstallCall(t, "/etc/virtle", "agent:users", ""),
		{
			path:          guestChownPath,
			args:          []string{"agent:users", "/etc/virtle/inline"},
			env:           []string{qga.InternalCommandPathEnv},
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

	runner := &launchRunner{}
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
		logger:            debugTestLogger(&logOutput),
		locker:            &fileLocker{},
		runner:            runner,
		socketWaiter:      &fakeSocketWaiter{callback: func(paths []string) error { return nil }},
		qmpDialer:         &fakeQMPDialer{client: qmpClient},
		guestAgentDialer:  &fakeGuestAgentDialer{client: guestAgent},
		shutdownDelay:     10 * time.Millisecond,
		qmpRetryDelay:     0,
		qmpConnectTimeout: 100 * time.Millisecond,
		qmpQuitTimeout:    time.Millisecond,
	}

	if err := manager.launch(context.Background(), cfg); err != nil {
		t.Fatalf("launch: %v", err)
	}

	if got, want := guestAgent.execs, []guestExecCall{
		{
			path:          guestTestPath,
			args:          []string{"-e", "/etc/virtle/existing"},
			env:           []string{qga.InternalCommandPathEnv},
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

	runner := &launchRunner{}
	qmpClient := &fakeQMPClient{
		onQuit: func() {
			runner.exitQEMU(nil)
		},
	}
	guestAgent := &fakeGuestAgentClient{
		execStatuses: []qga.ExecStatus{
			{Exited: true}, // sh -c dir install /etc/virtle/nested
			{Exited: true}, // chown file
			{Exited: true}, // chmod file
		},
	}
	manager := &manager{
		logger:            slog.New(slog.DiscardHandler),
		locker:            &fileLocker{},
		runner:            runner,
		socketWaiter:      &fakeSocketWaiter{callback: func(paths []string) error { return nil }},
		qmpDialer:         &fakeQMPDialer{client: qmpClient},
		guestAgentDialer:  &fakeGuestAgentDialer{client: guestAgent},
		shutdownDelay:     10 * time.Millisecond,
		qmpRetryDelay:     0,
		qmpConnectTimeout: 100 * time.Millisecond,
		qmpQuitTimeout:    time.Millisecond,
	}

	if err := manager.launch(context.Background(), cfg); err != nil {
		t.Fatalf("launch: %v", err)
	}

	if got, want := guestAgent.execs, []guestExecCall{
		guestDirInstallCall(t, "/etc/virtle/nested", "agent:users", "0750"),
		{
			path:          guestChownPath,
			args:          []string{"agent:users", "/etc/virtle/nested/new"},
			env:           []string{qga.InternalCommandPathEnv},
			captureOutput: true,
		},
		{
			path:          guestChmodPath,
			args:          []string{"0640", "/etc/virtle/nested/new"},
			env:           []string{qga.InternalCommandPathEnv},
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

	runner := &launchRunner{}
	qmpClient := &fakeQMPClient{
		onQuit: func() {
			runner.exitQEMU(nil)
		},
	}
	guestAgent := &fakeGuestAgentClient{
		execStatuses: []qga.ExecStatus{
			{Exited: true, ExitCode: 1}, // test -e /etc/virtle/new (missing)
			{Exited: true},              // sh -c dir install /etc/virtle
		},
	}
	manager := &manager{
		logger:            slog.New(slog.DiscardHandler),
		locker:            &fileLocker{},
		runner:            runner,
		socketWaiter:      &fakeSocketWaiter{callback: func(paths []string) error { return nil }},
		qmpDialer:         &fakeQMPDialer{client: qmpClient},
		guestAgentDialer:  &fakeGuestAgentDialer{client: guestAgent},
		shutdownDelay:     10 * time.Millisecond,
		qmpRetryDelay:     0,
		qmpConnectTimeout: 100 * time.Millisecond,
		qmpQuitTimeout:    time.Millisecond,
	}

	if err := manager.launch(context.Background(), cfg); err != nil {
		t.Fatalf("launch: %v", err)
	}

	if got, want := guestAgent.execs, []guestExecCall{
		{
			path:          guestTestPath,
			args:          []string{"-e", "/etc/virtle/new"},
			env:           []string{qga.InternalCommandPathEnv},
			captureOutput: true,
		},
		guestDirInstallCall(t, "/etc/virtle", "", ""),
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

	runner := &launchRunner{}
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
		shutdownDelay:     10 * time.Millisecond,
		qmpRetryDelay:     0,
		qmpConnectTimeout: 100 * time.Millisecond,
		qmpQuitTimeout:    time.Millisecond,
	}

	err := manager.launch(context.Background(), cfg)
	if err == nil {
		t.Fatal("expected launch to fail")
	}
	for _, want := range []string{"guest file write", "chown \"/etc/inline\" exited with status 1", "chown failed"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("expected error containing %q, got %v", want, err)
		}
	}
	if got, want := guestAgent.execs, []guestExecCall{
		guestDirInstallCall(t, "/etc", "agent:users", "0700"),
		{
			path:          guestChownPath,
			args:          []string{"agent:users", "/etc/inline"},
			env:           []string{qga.InternalCommandPathEnv},
			captureOutput: true,
		},
	}; !reflect.DeepEqual(got, want) {
		t.Fatalf("unexpected guest execs after chown failure: got %#v want %#v", got, want)
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

	runner := &launchRunner{}
	qmpClient := &fakeQMPClient{
		onQuit: func() {
			runner.exitQEMU(nil)
		},
	}
	guestAgent := &fakeGuestAgentClient{
		execStatuses: []qga.ExecStatus{
			{Exited: true, ExitCode: 1, ErrData: "aW5zdGFsbCBmYWlsZWQ="}, // sh -c dir install /etc/virtle
		},
	}
	manager := &manager{
		logger:            slog.New(slog.DiscardHandler),
		locker:            &fileLocker{},
		runner:            runner,
		socketWaiter:      &fakeSocketWaiter{callback: func(paths []string) error { return nil }},
		qmpDialer:         &fakeQMPDialer{client: qmpClient},
		guestAgentDialer:  &fakeGuestAgentDialer{client: guestAgent},
		shutdownDelay:     10 * time.Millisecond,
		qmpRetryDelay:     0,
		qmpConnectTimeout: 100 * time.Millisecond,
		qmpQuitTimeout:    time.Millisecond,
	}

	err := manager.launch(context.Background(), cfg)
	if err == nil {
		t.Fatal("expected launch to fail")
	}
	for _, want := range []string{"guest file write", "install dirs \"/etc/virtle\" exited with status 1", "install failed"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("expected error containing %q, got %v", want, err)
		}
	}
	if got, want := guestAgent.execs, []guestExecCall{
		guestDirInstallCall(t, "/etc/virtle", "agent:users", ""),
	}; !reflect.DeepEqual(got, want) {
		t.Fatalf("unexpected guest execs after install failure: got %#v want %#v", got, want)
	}
	if len(guestAgent.writes) != 0 {
		t.Fatalf("expected no guest writes after install failure, got %#v", guestAgent.writes)
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

	runner := &launchRunner{}
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
		shutdownDelay:     10 * time.Millisecond,
		qmpRetryDelay:     0,
		qmpConnectTimeout: 100 * time.Millisecond,
		qmpQuitTimeout:    time.Millisecond,
	}

	err := manager.launch(context.Background(), cfg)
	if err == nil {
		t.Fatal("expected launch to fail")
	}
	for _, want := range []string{"guest file write", "chmod \"/etc/inline\" exited with status 1", "chmod failed"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("expected error containing %q, got %v", want, err)
		}
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
		Version:       StateVersion,
		HostName:      cfg.Identity.HostName,
		QMPSocketPath: filepath.Join(tmpDir, "old-qmp.sock"),
		VMStatePath:   vmStatePath,
		CID:           3,
		Status:        "saved",
	}); err != nil {
		t.Fatalf("write suspend state: %v", err)
	}

	runner := &launchRunner{}
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
		shutdownDelay:       10 * time.Millisecond,
		qmpRetryDelay:       0,
		qmpConnectTimeout:   time.Millisecond,
		qmpQuitTimeout:      time.Millisecond,
		qmpMigrationTimeout: time.Millisecond,
	}

	if err := manager.launchWithOptions(context.Background(), cfg, LaunchOptions{Resume: ResumeModeForce}); err != nil {
		t.Fatalf("resume launch: %v", err)
	}
	if guestDialer.attempts != 1 {
		t.Fatalf("expected resume launch to skip guest agent writes and only request shutdown, got %d dial attempts", guestDialer.attempts)
	}
	if qmpClient.migrateIncomingCalls != 1 || qmpClient.contCalls != 1 {
		t.Fatalf("expected resume path to restore and continue, migrate=%d cont=%d", qmpClient.migrateIncomingCalls, qmpClient.contCalls)
	}
}

func TestManagerLaunchUsesExternalVirtioFSSocketWithoutManagingDaemon(t *testing.T) {
	tmpDir := t.TempDir()
	cfg := validManifest(tmpDir)
	cfg.Paths.LockPath = filepath.Join(tmpDir, "virtle.lock")
	cfg.QEMU.Devices.Block[0].ImagePath = "root.img"
	cfg.Volumes[0].AutoCreate = false
	// t.TempDir embeds this test's long name, which pushes the socket path
	// past the unix sun_path limit when TMPDIR is longer than /tmp (the nix
	// sandbox uses /build); keep the socket in a short-named directory.
	sockDir, err := os.MkdirTemp("", "virtle-sock")
	if err != nil {
		t.Fatalf("create socket dir: %v", err)
	}
	t.Cleanup(func() { os.RemoveAll(sockDir) })
	externalSocket := filepath.Join(sockDir, "virtiofs-nix-store.sock")
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

	runner := &launchRunner{}
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
		shutdownDelay:     10 * time.Millisecond,
		qmpConnectTimeout: time.Millisecond,
		qmpQuitTimeout:    time.Millisecond,
	}

	err = manager.launch(cancelCtx, cfg)
	if err != nil {
		t.Fatalf("launch: %v", err)
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

	err := manager.launch(context.Background(), cfg)
	if err == nil || !strings.Contains(err.Error(), "external virtiofs socket") || !strings.Contains(err.Error(), "does not exist") {
		t.Fatalf("expected missing external socket error, got %v", err)
	}
	if len(runner.startedNames()) != 0 {
		t.Fatalf("expected launch to fail before starting processes, got starts %v", runner.startedNames())
	}
}

func TestSaveSuspendStateConnectedStopsMigratesAndWritesSavedState(t *testing.T) {
	tmpDir := t.TempDir()
	cfg := validManifest(tmpDir)
	cfg.QEMU.QMP.SocketPath = "qmp.sock"

	qmpClient := &fakeQMPClient{status: "running"}
	manager := &manager{qmpConnectTimeout: time.Millisecond}

	qmpSocketPath := filepath.Join(tmpDir, "qmp.sock")
	manager.launchManifest = cfg
	if err := manager.saveSuspendStateConnected(context.Background(), qmpSocketPath, qmpClient, 7, nil); err != nil {
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
	if info, err := os.Stat(migratePath); err != nil {
		t.Fatalf("stat vm state: %v", err)
	} else if got, want := info.Mode().Perm(), os.FileMode(0o600); got != want {
		t.Fatalf("vm state mode: got %o want %o", got, want)
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
	manager.launchManifest = cfg
	handler := newLaunchSuspendHandler(manager, filepath.Join(tmpDir, "qmp.sock"), qmpClient, 7, nil, nil)

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
		done <- manifestBoundManager(&manager{}, cfg).suspend(context.Background())
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
				Version:       StateVersion,
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

	manager.launchManifest = cfg
	if err := manager.suspend(context.Background()); err != nil {
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
		Version:       StateVersion,
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

	manager.launchManifest = cfg
	if err := manager.suspend(context.Background()); err != nil {
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
		Version:       StateVersion,
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

	manager.launchManifest = cfg
	if err := manager.suspend(context.Background()); err != nil {
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
		qmpConnectTimeout:   time.Second,
	}

	manager.launchManifest = cfg
	got := manager.effectiveSuspendSignalTimeout()
	want := defaultLaunchSignalTimeout + 2*time.Second + 3*time.Second +
		time.Second + guestShutdownResponseTimeout + 3*4*time.Second
	if got != want {
		t.Fatalf("unexpected suspend signal timeout: got %s want %s", got, want)
	}
}

func TestManagerSuspendMissingPIDReportsLaunchError(t *testing.T) {
	tmpDir := t.TempDir()
	cfg := validManifest(tmpDir)

	err := manifestBoundManager(&manager{pidSignaler: &fakePIDSignaler{}}, cfg).suspend(context.Background())
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

	err := (&manager{}).launchWithOptions(context.Background(), cfg, LaunchOptions{Resume: ResumeModeForce})
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
		Version:       StateVersion,
		QMPSocketPath: filepath.Join(tmpDir, "qmp.sock"),
		Status:        "paused",
	}); err != nil {
		t.Fatalf("write initial suspend state: %v", err)
	}

	err := (&manager{}).launchWithOptions(context.Background(), cfg, LaunchOptions{Resume: ResumeModeForce})
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

	runner := &launchRunner{}
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
		shutdownDelay:     10 * time.Millisecond,
		qmpConnectTimeout: time.Millisecond,
		qmpQuitTimeout:    time.Millisecond,
	}

	if err := manager.launchWithOptions(context.Background(), cfg, LaunchOptions{Resume: ResumeModeAuto}); err != nil {
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
		Version:       StateVersion,
		QMPSocketPath: filepath.Join(tmpDir, "qmp.sock"),
		VMStatePath:   statePath,
		CID:           3,
		Status:        "saved",
	}); err != nil {
		t.Fatalf("write suspend state: %v", err)
	}

	runner := &launchRunner{}
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
		shutdownDelay:       10 * time.Millisecond,
		qmpConnectTimeout:   time.Millisecond,
		qmpQuitTimeout:      time.Millisecond,
		qmpMigrationTimeout: time.Second,
	}

	if err := manager.launchWithOptions(context.Background(), cfg, LaunchOptions{Resume: ResumeModeForce}); err != nil {
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

func TestStartVMQueuesControlSuspendUntilGuestProvisioningCompletes(t *testing.T) {
	tmpDir := t.TempDir()
	cfg := validManifest(tmpDir)
	cfg.Paths.LockPath = filepath.Join(tmpDir, "virtle.lock")
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
	guestAgent := &fakeGuestAgentClient{record: func(event string) {
		if strings.HasPrefix(event, "guest-write:") {
			writeStartedOnce.Do(func() { close(writeStarted) })
			<-allowWrite
		}
	}}
	runner := &launchRunner{}
	qmpClient := &fakeQMPClient{status: "running", onQuit: func() { runner.exitQEMU(nil) }}
	manager := newManagerFromConfig(Config{
		Locker:              &fileLocker{},
		Runner:              runner,
		SocketWaiter:        &fakeSocketWaiter{callback: func([]string) error { return nil }},
		QMPDialer:           &fakeQMPDialer{client: qmpClient},
		GuestAgentDialer:    &fakeGuestAgentDialer{client: guestAgent},
		Logger:              slog.New(slog.DiscardHandler),
		ShutdownDelay:       10 * time.Millisecond,
		QMPConnectTimeout:   time.Second,
		QMPQuitTimeout:      time.Second,
		QMPMigrationTimeout: time.Second,
	})
	type startResult struct {
		vm  *VM
		err error
	}
	startDone := make(chan startResult, 1)
	go func() {
		v, err := manager.startVM(context.Background(), launch.Spec{
			Manifest: cfg,
			Options:  launch.Options{HasRemoteControl: true},
		})
		startDone <- startResult{vm: v, err: err}
	}()
	<-writeStarted

	controlSocketPath, err := cfg.ResolvedControlSocketPath()
	if err != nil {
		t.Fatalf("resolve control socket: %v", err)
	}
	rpcCtx, cancelRPC := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancelRPC()
	rpcDone := make(chan error, 1)
	go func() {
		machine, err := control.Dial(rpcCtx, controlSocketPath)
		if err == nil {
			err = machine.(backend.Suspender).Suspend(rpcCtx)
		}
		rpcDone <- err
	}()

	select {
	case err := <-rpcDone:
		t.Fatalf("control suspend returned before guest write completed: %v", err)
	case <-time.After(testNoReturnTimeout):
	}
	close(allowWrite)
	result := <-startDone
	if result.err != nil {
		t.Fatalf("start VM: %v", result.err)
	}
	select {
	case <-result.vm.SuspendRequests():
	case <-time.After(time.Second):
		t.Fatal("control suspend was not queued")
	}
	if err := result.vm.HandleSuspendRequest(context.Background()); !launch.IsSavedSuspendExit(err) {
		t.Fatalf("handle suspend: %v", err)
	}
	if err := <-rpcDone; err != nil {
		t.Fatalf("control suspend: %v", err)
	}
}

func TestStartVMContextCancellationStopsMachine(t *testing.T) {
	tmpDir := t.TempDir()
	cfg := validManifest(tmpDir)
	cfg.Paths.LockPath = filepath.Join(tmpDir, "virtle.lock")
	cfg.QEMU.Devices.VirtioFS = nil
	cfg.QEMU.Devices.Block = nil
	cfg.QEMU.SSHReady.SocketPath = ""
	cfg.Volumes = nil
	cfg.Run = nil

	runner := &launchRunner{}
	ctx, cancel := context.WithCancel(context.Background())
	v, err := StartVM(ctx, cfg, StartOptions{}, Config{
		Locker:            &fileLocker{},
		Runner:            runner,
		SocketWaiter:      &fakeSocketWaiter{callback: func([]string) error { return nil }},
		QMPDialer:         &fakeQMPDialer{client: &fakeQMPClient{}},
		Logger:            slog.New(slog.DiscardHandler),
		ShutdownDelay:     10 * time.Millisecond,
		QMPConnectTimeout: time.Second,
		QMPQuitTimeout:    time.Second,
	})
	if err != nil {
		cancel()
		t.Fatalf("start VM: %v", err)
	}

	cancel()
	select {
	case <-v.Done():
	case <-time.After(time.Second):
		t.Fatal("machine did not stop after Start context cancellation")
	}
	if !errors.Is(v.Err(), context.Canceled) {
		t.Fatalf("machine error = %v, want context.Canceled", v.Err())
	}
}

func TestStartVMServicesControlSuspendAfterStart(t *testing.T) {
	tmpDir := t.TempDir()
	cfg := validManifest(tmpDir)
	cfg.Paths.LockPath = filepath.Join(tmpDir, "virtle.lock")
	cfg.QEMU.Devices.VirtioFS = nil
	cfg.QEMU.Devices.Block = nil
	cfg.QEMU.SSHReady.SocketPath = ""
	cfg.Volumes = nil
	cfg.Run = nil

	runner := &launchRunner{}
	qmpClient := &fakeQMPClient{status: "running", onQuit: func() { runner.exitQEMU(nil) }}
	v, err := StartVM(context.Background(), cfg, StartOptions{}, Config{
		Locker:              &fileLocker{},
		Runner:              runner,
		SocketWaiter:        &fakeSocketWaiter{callback: func([]string) error { return nil }},
		QMPDialer:           &fakeQMPDialer{client: qmpClient},
		Logger:              slog.New(slog.DiscardHandler),
		ShutdownDelay:       10 * time.Millisecond,
		QMPConnectTimeout:   time.Second,
		QMPQuitTimeout:      time.Second,
		QMPMigrationTimeout: time.Second,
	})
	if err != nil {
		t.Fatalf("start VM: %v", err)
	}
	defer v.Kill()

	controlPath, err := cfg.ResolvedControlSocketPath()
	if err != nil {
		t.Fatalf("resolve control socket: %v", err)
	}
	rpcCtx, cancelRPC := context.WithTimeout(context.Background(), time.Second)
	defer cancelRPC()
	remote, err := control.Dial(rpcCtx, controlPath)
	if err != nil {
		t.Fatalf("dial control socket: %v", err)
	}
	if err := remote.(backend.Suspender).Suspend(rpcCtx); err != nil {
		t.Fatalf("control suspend: %v", err)
	}
	if err := remote.Wait(rpcCtx); err != nil {
		t.Fatalf("wait for remote machine: %v", err)
	}
	if err := v.Wait(rpcCtx); err != nil {
		t.Fatalf("wait for local machine: %v", err)
	}
	if qmpClient.migrateCalls != 1 {
		t.Fatalf("migration calls = %d, want 1", qmpClient.migrateCalls)
	}
}

func TestStartVMSkipsVirtioFSReadinessWithoutVirtioFS(t *testing.T) {
	tmpDir := t.TempDir()
	cfg := validManifest(tmpDir)
	cfg.Paths.LockPath = filepath.Join(tmpDir, "virtle.lock")
	cfg.QEMU.Devices.VirtioFS = nil
	cfg.QEMU.Devices.Block = nil
	cfg.Volumes = nil
	cfg.Run = nil

	runner := &launchRunner{}
	qmpClient := &fakeQMPClient{onQuit: func() { runner.exitQEMU(nil) }}
	waiter := &fakeSocketWaiter{callback: func([]string) error { return nil }}
	manager := newManagerFromConfig(Config{
		Locker:            &fileLocker{},
		Runner:            runner,
		SocketWaiter:      waiter,
		QMPDialer:         &fakeQMPDialer{client: qmpClient},
		Logger:            slog.New(slog.DiscardHandler),
		ShutdownDelay:     10 * time.Millisecond,
		QMPConnectTimeout: time.Second,
		QMPQuitTimeout:    time.Second,
	})
	v, err := manager.startVM(context.Background(), launch.Spec{Manifest: cfg})
	if err != nil {
		t.Fatalf("start VM: %v", err)
	}
	if err := v.Shutdown(context.Background()); err != nil {
		t.Fatalf("shutdown VM: %v", err)
	}

	if got, want := runner.startedNames(), []string{"qemu-system-x86_64"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("start order = %v, want %v", got, want)
	}
	if got, want := waiter.paths, [][]string{{filepath.Join(tmpDir, "qmp.sock")}}; !reflect.DeepEqual(got, want) {
		t.Fatalf("socket waits = %v, want %v", got, want)
	}
	if containsString(runner.qemuArgs(), "vhost-user-fs") || containsString(runner.qemuArgs(), "virtio-blk") {
		t.Fatalf("unexpected storage device in QEMU args: %v", runner.qemuArgs())
	}
}

func TestStartVMWithOnlyNinePDoesNotWaitForVirtioFS(t *testing.T) {
	tmpDir := t.TempDir()
	cfg := validManifest(tmpDir)
	cfg.Paths.LockPath = filepath.Join(tmpDir, "virtle.lock")
	cfg.QEMU.Devices.VirtioFS = nil
	cfg.QEMU.Devices.Block = nil
	cfg.QEMU.Devices.NineP = []manifest.QEMUNinePShare{{
		ID: "fs9p0", SourcePath: "shared", Tag: "shared", SecurityModel: "mapped", Transport: "pci",
	}}
	cfg.Volumes = nil
	cfg.Run = nil

	runner := &launchRunner{}
	qmpClient := &fakeQMPClient{onQuit: func() { runner.exitQEMU(nil) }}
	waiter := &fakeSocketWaiter{callback: func([]string) error { return nil }}
	manager := newManagerFromConfig(Config{
		Locker:            &fileLocker{},
		Runner:            runner,
		SocketWaiter:      waiter,
		QMPDialer:         &fakeQMPDialer{client: qmpClient},
		Logger:            slog.New(slog.DiscardHandler),
		ShutdownDelay:     10 * time.Millisecond,
		QMPConnectTimeout: time.Second,
		QMPQuitTimeout:    time.Second,
	})
	v, err := manager.startVM(context.Background(), launch.Spec{Manifest: cfg})
	if err != nil {
		t.Fatalf("start VM: %v", err)
	}
	if err := v.Shutdown(context.Background()); err != nil {
		t.Fatalf("shutdown VM: %v", err)
	}

	if got, want := waiter.paths, [][]string{{filepath.Join(tmpDir, "qmp.sock")}}; !reflect.DeepEqual(got, want) {
		t.Fatalf("socket waits = %v, want %v", got, want)
	}
	if containsString(runner.qemuArgs(), "vhost-user-fs") {
		t.Fatalf("unexpected virtiofs device in QEMU args: %v", runner.qemuArgs())
	}
	if !containsString(runner.qemuArgs(), "virtio-9p-pci,fsdev=fs9p0,mount_tag=shared") {
		t.Fatalf("missing 9p device in QEMU args: %v", runner.qemuArgs())
	}
}

func TestBuildQEMUCommandUsesTypedConfigAndRuntimeCID(t *testing.T) {
	cfg := validManifest("/tmp/work")
	cfg.QEMU.Console = manifest.QEMUConsoleOff

	spec, err := buildTestQEMUCommand(cfg, 42, false)
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

func TestBuildQEMUCommandOnlyConnectsRequestedConsole(t *testing.T) {
	var console bytes.Buffer
	cfg := validManifest("/tmp/work")
	cfg.QEMU.Console = manifest.QEMUConsoleOff

	cmd, err := buildQEMUCommand(cfg, 42, false, &console)
	if err != nil {
		t.Fatalf("build headless qemu command: %v", err)
	}
	if cmd.Stdout != nil || cmd.Stderr != nil {
		t.Fatal("expected the command runner to own headless qemu output")
	}

	cfg.QEMU.Console = manifest.QEMUConsolePrint
	cmd, err = buildQEMUCommand(cfg, 42, false, &console)
	if err != nil {
		t.Fatalf("build console qemu command: %v", err)
	}
	if cmd.Stdout != &console || cmd.Stderr != &console {
		t.Fatal("expected requested qemu console output on configured foreground writer")
	}
}

func TestBuildQEMUCommandAddsPCIEHotplugPorts(t *testing.T) {
	manifest := validManifest("/tmp/work")
	manifest.QEMU.Hotplug.PCIEPorts = 2

	spec, err := buildTestQEMUCommand(manifest, 42, false)
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

	if err := manifestBoundManager(&manager{}, cfg).hotplug(context.Background(), "cache", true); err != nil {
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
	cfg.Hotplug = []manifest.HotplugDevice{
		{
			Kind: manifest.HotplugKindNet,
			ID:   "vpn",
			Net:  manifest.HotplugNet{Backend: "user", MAC: "02:02:00:00:00:10"},
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
		shutdownDelay:     time.Millisecond,
	}
	plan, err := manager.planLaunch(launch.Spec{Manifest: cfg, Options: LaunchOptions{Resume: ResumeModeNo}})
	if err != nil {
		t.Fatalf("plan launch: %v", err)
	}

	running, err := manager.startWithPlan(context.Background(), plan)
	if err != nil {
		t.Fatalf("start runtime: %v", err)
	}
	defer running.Close()

	params, err := json.Marshal(control.HotplugRequest{ID: "vpn"})
	if err == nil {
		_, err = control.Raw(context.Background(), plan.Paths.ControlSocket, "hotplug", params)
	}
	if err != nil {
		t.Fatalf("control hotplug: %v", err)
	}
	if got := strings.Join(qmp.rawCommands, "\n"); !strings.Contains(got, `"execute":"netdev_add"`) {
		t.Fatalf("expected netdev_add command, got %#v", qmp.rawCommands)
	}
}

func TestGuestExecRejectsNegativeTimeout(t *testing.T) {
	feature := (&manager{}).guestFeature("qga.sock", nil)
	_, err := feature.GuestExec(context.Background(), control.GuestExecRequest{Path: "/bin/true", Timeout: units.Duration(-5 * time.Second)})
	var rpcErr *control.RPCError
	if !errors.As(err, &rpcErr) || rpcErr.Code != control.ErrInvalidParams {
		t.Fatalf("expected invalid params error, got %v", err)
	}
}

func TestGuestFeatureMapsResourceLimit(t *testing.T) {
	err := guestFeatureError(&limits.Error{Resource: "guest command output", Limit: 42})
	var rpcErr *control.RPCError
	if !errors.As(err, &rpcErr) || rpcErr.Code != control.ErrResourceLimit {
		t.Fatalf("expected resource limit error, got %v", err)
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
		shutdownDelay:     time.Millisecond,
	}
	plan, err := manager.planLaunch(launch.Spec{Manifest: cfg, Options: LaunchOptions{Resume: ResumeModeNo, HasRemoteControl: true}})
	if err != nil {
		t.Fatalf("plan launch: %v", err)
	}

	running, err := manager.startWithPlan(context.Background(), plan)
	if err != nil {
		t.Fatalf("start runtime: %v", err)
	}
	defer running.Close()

	machine, err := control.Dial(context.Background(), plan.Paths.ControlSocket)
	if err != nil {
		t.Fatalf("control dial: %v", err)
	}
	guest, err := machine.RemoteControl()
	if err != nil {
		t.Fatalf("remote control: %v", err)
	}

	var stdout, stderr bytes.Buffer
	execCtx, cancelExec := context.WithTimeout(context.Background(), 300*time.Second)
	defer cancelExec()
	err = guest.Run(execCtx, &vm.GuestCmd{
		Path: "/bin/sh", Args: []string{"-c", "echo hi"}, Stdout: &stdout, Stderr: &stderr,
	})
	var exitErr *vm.ExitError
	if !errors.As(err, &exitErr) || exitErr.Code != 3 {
		t.Fatalf("control guest exec error: %v", err)
	}
	if stdout.String() != "exec-out\n" || stderr.String() != "exec-err\n" {
		t.Fatalf("unexpected guest exec output: stdout=%q stderr=%q", stdout.String(), stderr.String())
	}
	if got, want := len(guestAgent.execs), 1; got != want {
		t.Fatalf("guest exec count after guest-exec: got %d want %d", got, want)
	}
	exec := guestAgent.execs[0]
	if exec.path != "/bin/sh" || !reflect.DeepEqual(exec.args, []string{"-c", "echo hi"}) || !exec.captureOutput {
		t.Fatalf("unexpected guest-exec call: %#v", exec)
	}
	if exec.timeout <= 299*time.Second || exec.timeout > 300*time.Second {
		t.Fatalf("guest-exec deadline: got %s want ~300s", exec.timeout)
	}

	reader, err := guest.Open(context.Background(), "/tmp/message")
	if err != nil {
		t.Fatalf("control guest read: %v", err)
	}
	readData, err := io.ReadAll(reader)
	if err != nil || string(readData) != "hello" {
		t.Fatalf("unexpected guest read: %q, %v", readData, err)
	}

	writer, err := guest.Create(context.Background(), "/tmp/message", 0)
	if err != nil {
		t.Fatalf("control guest create: %v", err)
	}
	if _, err := io.WriteString(writer, "updated"); err != nil {
		t.Fatalf("control guest write: %v", err)
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("control guest close: %v", err)
	}
	if got := guestAgent.writes["/tmp/message"]; got != "dXBkYXRlZA==" {
		t.Fatalf("unexpected guest write payload: %q", got)
	}
}

func TestLaunchRuntimeWithoutRemoteControlOmitsGuestRPCs(t *testing.T) {
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
		shutdownDelay:     time.Millisecond,
	}
	plan, err := manager.planLaunch(launch.Spec{Manifest: cfg, Options: LaunchOptions{Resume: ResumeModeNo}})
	if err != nil {
		t.Fatalf("plan launch: %v", err)
	}

	running, err := manager.startWithPlan(context.Background(), plan)
	if err != nil {
		t.Fatalf("start runtime: %v", err)
	}
	defer running.Close()

	machine, err := control.Dial(context.Background(), plan.Paths.ControlSocket)
	if err != nil {
		t.Fatalf("control dial: %v", err)
	}
	if _, err := machine.RemoteControl(); !errors.Is(err, errors.ErrUnsupported) {
		t.Fatalf("RemoteControl error = %v, want errors.ErrUnsupported", err)
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

			spec, err := buildTestQEMUCommand(manifest, 42, false)
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

	spec, err := buildTestQEMUCommand(cfg, 42, false)
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

	spec, err := buildTestQEMUCommand(cfg, 42, false)
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

			spec, err := buildTestQEMUCommand(cfg, 42, false)
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

	spec, err := buildTestQEMUCommand(manifest, 42, false)
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

	spec, err := buildTestQEMUCommand(cfg, 42, false)
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

func TestBuildQEMUCommandOmitsVSockDeviceWhenIDEmpty(t *testing.T) {
	cfg := validManifest("/tmp/work")
	cfg.QEMU.Devices.VSOCK.ID = ""

	spec, err := buildTestQEMUCommand(cfg, 42, false)
	if err != nil {
		t.Fatalf("build qemu command: %v", err)
	}
	if indexStringContaining(commandArgs(spec), "vhost-vsock") != -1 {
		t.Fatalf("expected qemu args to omit vsock device: %v", commandArgs(spec))
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

	spec, err := buildTestQEMUCommand(cfg, 42, false)
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

	spec, err := buildTestQEMUCommand(cfg, 42, false)
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

	spec, err := buildTestQEMUCommand(manifest, 42, false)
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

	spec, err := buildTestQEMUCommand(cfg, 42, false)
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

	manager.launchManifest = cfg
	if _, err := manager.startRuns(3); err != nil {
		t.Fatalf("start runs: %v", err)
	}

	if got := runner.virtiofsEnv()["virtiofsd-workspace"]; !containsString(got, "VIRTIOFSD_SOCKET="+wantSocket) {
		t.Fatalf("expected virtiofs run env to contain resolved socket path %q: %v", wantSocket, got)
	}
	if !runner.processGroups()["virtiofsd-workspace"] {
		t.Fatal("expected virtiofs run to run in its own process group")
	}
}

func debugTestLogger(w io.Writer) *slog.Logger {
	return slog.New(slog.NewTextHandler(w, &slog.HandlerOptions{Level: slog.LevelDebug}))
}

func buildTestQEMUCommand(manifest *manifest.Manifest, cid int, incoming bool) (*exec.Cmd, error) {
	return buildQEMUCommand(manifest, cid, incoming, io.Discard)
}

func validManifest(workingDir string) *manifest.Manifest {
	return &manifest.Manifest{
		Identity: manifest.Identity{HostName: "agent-sandbox"},
		Paths: manifest.Paths{
			WorkingDir: workingDir,
			LockPath:   "/tmp/virtle.lock",
		},
		SSH: manifest.SSH{
			Argv:       []string{"/bin/ssh"},
			User:       "agent",
			RetryDelay: 500 * time.Millisecond,
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
	mf := validManifest(workingDir)
	mf.QEMU.Devices.Balloon = &manifest.BalloonDevice{
		ID:        "balloon0",
		Transport: "pci",
	}
	return mf
}

type launchRunner struct {
	base        *executortest.Runner
	mu          sync.Mutex
	startErrors map[string]error
	qemu        *executortest.Process
	onStart     func(name string, cmd *exec.Cmd)
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

func (r *launchRunner) qemuArgs() []string {
	return r.firstArgs(func(start executortest.Start) bool {
		return strings.HasPrefix(start.Name, "qemu-system")
	})
}

func (r *launchRunner) runArgs() map[string][]string {
	values := make(map[string][]string)
	for _, start := range r.starts() {
		if !strings.HasPrefix(start.Name, "qemu-system") && !strings.HasPrefix(start.Name, "virtiofsd") {
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

func commandArgs(cmd *exec.Cmd) []string {
	if cmd == nil || len(cmd.Args) == 0 {
		return nil
	}
	return cmd.Args[1:]
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
	calls    int
	paths    [][]string
	callback func(paths []string) error
}

func (w *fakeSocketWaiter) Wait(ctx context.Context, socketPaths []string) error {
	w.calls++
	w.paths = append(w.paths, append([]string(nil), socketPaths...))
	if w.callback == nil {
		return nil
	}
	return w.callback(socketPaths)
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

func TestRequestGuestShutdownFailsFastWhenAgentUnavailable(t *testing.T) {
	client := &fakeGuestAgentClient{pingErr: errors.New("no agent listening")}
	manager := &manager{
		guestAgentDialer:  &fakeGuestAgentDialer{client: client},
		qmpConnectTimeout: time.Second,
	}
	manager.launchManifest = &manifest.Manifest{}
	err := manager.requestGuestShutdown(context.Background(), "/tmp/qga.sock", nil)
	if err == nil || !strings.Contains(err.Error(), "guest agent unavailable") {
		t.Fatalf("expected unavailable error instead of a silent success, got %v", err)
	}
}

func TestRequestGuestShutdownReportsCommandFailure(t *testing.T) {
	client := &fakeGuestAgentClient{execStatuses: []qga.ExecStatus{{Exited: true, ExitCode: 127}}}
	manager := &manager{
		guestAgentDialer:  &fakeGuestAgentDialer{client: client},
		qmpConnectTimeout: time.Second,
	}
	manager.launchManifest = &manifest.Manifest{}
	exec := []string{"/bin/sh", "-c", "poweroff"}
	err := manager.requestGuestShutdown(context.Background(), "/tmp/qga.sock", exec)
	if err == nil || !strings.Contains(err.Error(), "exited with status 127") {
		t.Fatalf("expected shutdown command failure, got %v", err)
	}
	if len(client.execs) != 1 || client.execs[0].path != exec[0] || !reflect.DeepEqual(client.execs[0].args, exec[1:]) {
		t.Fatalf("unexpected guest shutdown exec: %#v", client.execs)
	}
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
	shutdownErr     error
	openErr         error
	disconnects     int
	record          func(string)
}

type guestExecCall struct {
	path          string
	args          []string
	env           []string
	captureOutput bool
	// timeout is the remaining ctx deadline observed at exec time; zero when
	// the exec ctx had no deadline.
	timeout time.Duration
}

// guestDirInstallCall returns the exact guest command that the launch
// package's directory installer issues for guestDir, so expected exec lists
// don't re-encode the installer's script or argument layout.
func guestDirInstallCall(t *testing.T, guestDir string, owner string, mode string) guestExecCall {
	t.Helper()
	var call guestExecCall
	captured := false
	installer := launch.ScriptGuestDirectoryInstaller(func(_ context.Context, _ string, path string, args []string) error {
		call = guestExecCall{
			path:          path,
			args:          args,
			env:           []string{qga.InternalCommandPathEnv},
			captureOutput: true,
		}
		captured = true
		return nil
	})
	if err := installer.InstallTree(context.Background(), guestDir, owner, mode); err != nil {
		t.Fatalf("capture guest dir install call: %v", err)
	}
	if !captured {
		t.Fatal("script installer issued no guest command")
	}
	return call
}

// guestDirInstallTarget reports the directory a guest exec installs when the
// exec matches the launch directory installer's invocation. It compares
// against a template captured through the same seam production uses, keeping
// the fake client ignorant of the installer's script and argument layout.
var guestDirInstallTarget = func() func(path string, args []string) (string, bool) {
	const (
		sentinelDir   = "\x00dir"
		sentinelOwner = "\x00owner"
		sentinelMode  = "\x00mode"
	)
	tmplPath := ""
	var tmplArgs []string
	installer := launch.ScriptGuestDirectoryInstaller(func(_ context.Context, _ string, path string, args []string) error {
		tmplPath, tmplArgs = path, args
		return nil
	})
	if err := installer.InstallTree(context.Background(), sentinelDir, sentinelOwner, sentinelMode); err != nil || tmplArgs == nil {
		panic("capture guest dir install template")
	}
	return func(path string, args []string) (string, bool) {
		if path != tmplPath || len(args) != len(tmplArgs) {
			return "", false
		}
		dir := ""
		for i, tmpl := range tmplArgs {
			switch tmpl {
			case sentinelDir:
				dir = args[i]
			case sentinelOwner, sentinelMode:
				// Varies per call.
			default:
				if args[i] != tmpl {
					return "", false
				}
			}
		}
		return dir, true
	}
}()

func (c *fakeGuestAgentClient) Ping(ctx context.Context) error {
	if c.record != nil {
		c.record("guest-ping")
	}
	return c.pingErr
}

func (c *fakeGuestAgentClient) Shutdown(ctx context.Context) error {
	if c.record != nil {
		c.record("guest-shutdown")
	}
	return c.shutdownErr
}

func (c *fakeGuestAgentClient) OpenFile(ctx context.Context, path string) (int, error) {
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

func (c *fakeGuestAgentClient) OpenFileRead(ctx context.Context, path string) (int, error) {
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

func (c *fakeGuestAgentClient) ReadFile(ctx context.Context, handle int, count int) (string, bool, error) {
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

func (c *fakeGuestAgentClient) WriteFile(ctx context.Context, handle int, payloadBase64 string) error {
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

func (c *fakeGuestAgentClient) CloseFile(ctx context.Context, handle int) error {
	c.mu.Lock()
	path := c.handles[handle]
	c.closes = append(c.closes, path)
	c.mu.Unlock()

	if c.record != nil {
		c.record("guest-close:" + path)
	}
	return c.closeErr
}

func (c *fakeGuestAgentClient) Exec(ctx context.Context, path string, args []string, env []string, captureOutput bool) (int, error) {
	remaining := time.Duration(0)
	if deadline, ok := ctx.Deadline(); ok {
		remaining = time.Until(deadline)
	}
	c.mu.Lock()
	c.execs = append(c.execs, guestExecCall{
		path:          path,
		args:          append([]string(nil), args...),
		env:           append([]string(nil), env...),
		captureOutput: captureOutput,
		timeout:       remaining,
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
	if c.record != nil {
		if dir, ok := guestDirInstallTarget(path, args); ok {
			c.record("guest-install-tree:" + dir)
		}
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

func (c *fakeGuestAgentClient) ExecStatus(ctx context.Context, pid int) (qga.ExecStatus, error) {
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

func (m *manager) launchWithOptions(ctx context.Context, manifest *manifest.Manifest, options launch.Options) error {
	options.HasRemoteControl = true
	v, err := m.startVM(ctx, launch.Spec{Manifest: manifest, Options: options})
	if err != nil {
		if launch.IsSavedSuspendExit(err) {
			return nil
		}
		return err
	}
	if err := context.Cause(ctx); err != nil {
		return errors.Join(err, v.Shutdown(context.Background()))
	}
	if err := v.CommitResume(); err != nil {
		return errors.Join(err, v.Shutdown(context.Background()))
	}
	return v.Shutdown(context.Background())
}

// launch is a test-only convenience using the default foreground options.
func (m *manager) launch(ctx context.Context, manifest *manifest.Manifest) error {
	return m.launchWithOptions(ctx, manifest, launch.Options{Resume: launch.ResumeModeNo})
}

func manifestBoundManager(m *manager, cfg *manifest.Manifest) *manager {
	m.launchManifest = cfg
	return m
}

type pidSignal struct {
	pid int
	sig os.Signal
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
	mu                      sync.Mutex
	quitCalls               int
	stopCalls               int
	contCalls               int
	migrateCalls            int
	migrateIncomingCalls    int
	queryMigrateCalls       int
	queryStatusCalls        int
	disconnectCalls         int
	rawCommands             []string
	deviceDelWaits          []string
	status                  string
	migrationStatus         string
	migratePath             string
	migrateIncomingPath     string
	onQuit                  func()
	onStop                  func()
	onCont                  func()
	onEnableBalloonStats    func()
	listQOMProperties       map[string][]fakeQOMProperty
	listQOMPropertiesErr    map[string]error
	enableBalloonStatsErr   error
	queryBalloonActualBytes int64
	queryBalloonErr         error
	readBalloonStats        map[string]int64
	readBalloonStatsErr     error
	readBalloonStatsDelay   time.Duration
	readBalloonStatsUpdated time.Time
	onReadBalloonStats      func()
	setBalloonLogicalSizes  []int64
	setBalloonErr           error
}

func (c *fakeQMPClient) RunRaw(ctx context.Context, command string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.rawCommands = append(c.rawCommands, command)
	return nil
}

func (c *fakeQMPClient) DeviceDelAndWait(ctx context.Context, id string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.rawCommands = append(c.rawCommands, `{"execute":"device_del","arguments":{"id":"`+id+`"}}`)
	c.deviceDelWaits = append(c.deviceDelWaits, id)
	return nil
}

func (c *fakeQMPClient) Quit(ctx context.Context) error {
	c.mu.Lock()
	c.quitCalls++
	onQuit := c.onQuit
	c.mu.Unlock()

	if onQuit != nil {
		onQuit()
	}
	return nil
}

func (c *fakeQMPClient) Stop(ctx context.Context) error {
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

func (c *fakeQMPClient) Cont(ctx context.Context) error {
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

func (c *fakeQMPClient) QueryStatus(ctx context.Context) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.queryStatusCalls++
	if c.status == "" {
		c.status = "running"
	}
	return c.status, nil
}

func (c *fakeQMPClient) MigrateToFile(ctx context.Context, path string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.migrateCalls++
	c.migratePath = path
	c.migrationStatus = "completed"
	return nil
}

func (c *fakeQMPClient) MigrateIncoming(ctx context.Context, path string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.migrateIncomingCalls++
	c.migrateIncomingPath = path
	c.migrationStatus = "completed"
	return nil
}

func (c *fakeQMPClient) QueryMigrate(ctx context.Context) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.queryMigrateCalls++
	if c.migrationStatus == "" {
		c.migrationStatus = "completed"
	}
	return c.migrationStatus, nil
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

func (c *fakeQMPClient) WithRaw(ctx context.Context, fn func(*rawQMP.Monitor) error) error {
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
		status, err := c.QueryStatus(context.Background())
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
		if err := c.Stop(context.Background()); err != nil {
			return nil, err
		}
		return map[string]any{"return": map[string]any{}}, nil
	case "cont":
		if err := c.Cont(context.Background()); err != nil {
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
		status, err := c.QueryMigrate(context.Background())
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
		onRead := c.onReadBalloonStats
		c.mu.Unlock()
		if onRead != nil {
			onRead()
		}

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
