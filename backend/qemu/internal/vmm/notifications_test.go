package vmm

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/shazow/virtle/backend/qemu/internal/launch"
	"github.com/shazow/virtle/internal/executor"
	"github.com/shazow/virtle/internal/manifest"
)

func TestCommandNotifierHonorsStateAllowlistAndPassesEnv(t *testing.T) {
	tmpDir := t.TempDir()
	recordPath := filepath.Join(tmpDir, "notify.json")
	cfg := validManifest(tmpDir)
	cfg.Notifications = manifest.Notifications{
		Command: notificationHookCommand(t, recordPath, "--flag"),
		States:  []string{notifyStateRuntimeResume},
	}
	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))
	notifier := newCommandNotifier(cfg, logger, &executor.Runner{Logger: logger})

	notifier.Notify(context.Background(), notifyStateRuntimeSuspend, "ignored", nil)
	if _, err := os.Stat(recordPath); !os.IsNotExist(err) {
		t.Fatalf("expected allowlist to suppress suspend notification, stat err=%v", err)
	}

	notifier.Notify(context.Background(), notifyStateRuntimeResume, "Restored", map[string]string{
		"vmStatePath": "/tmp/work/state",
		"cid":         "7",
	})
	record := readNotificationHookRecord(t, recordPath)
	if got, want := notificationHookArgs(record.Args), []string{"--flag"}; !slices.Equal(got, want) {
		t.Fatalf("unexpected command args: got %v want %v", got, want)
	}
	if got, want := record.Dir, tmpDir; got != want {
		t.Fatalf("unexpected command dir: got %q want %q", got, want)
	}
	for _, want := range []string{
		"VIRTLE_NOTIFY_STATE",
		"VIRTLE_NOTIFY_MESSAGE",
		"VIRTLE_NOTIFY_CONTEXT_CID",
		"VIRTLE_NOTIFY_CONTEXT_VM_STATE_PATH",
	} {
		if record.Env[want] == "" {
			t.Fatalf("expected env %q in %#v", want, record.Env)
		}
	}
	if got, want := record.Env["VIRTLE_NOTIFY_STATE"], notifyStateRuntimeResume; got != want {
		t.Fatalf("unexpected state env: got %q want %q", got, want)
	}
}

func TestCommandNotifierKeepsCommandPathAndUsesAbsoluteWorkingDir(t *testing.T) {
	tmpDir := t.TempDir()
	t.Chdir(tmpDir)
	if err := os.Mkdir("work", 0o755); err != nil {
		t.Fatalf("create work dir: %v", err)
	}
	exe, err := os.Executable()
	if err != nil {
		t.Fatalf("resolve test executable: %v", err)
	}
	notifyPath := filepath.Join(tmpDir, "notify")
	if err := os.Symlink(exe, notifyPath); err != nil {
		t.Fatalf("symlink notify command: %v", err)
	}
	t.Setenv("PATH", tmpDir)
	recordPath := filepath.Join(tmpDir, "notify.json")

	cfg := validManifest("work")
	cfg.Notifications.Command = notificationHookCommandWithPath(recordPath, "notify")
	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))
	notifier := newCommandNotifier(cfg, logger, &executor.Runner{Logger: logger})

	notifier.Notify(context.Background(), notifyStateRuntimeResume, "Restored", nil)

	record := readNotificationHookRecord(t, recordPath)
	if got, want := record.Dir, filepath.Join(tmpDir, "work"); got != want {
		t.Fatalf("unexpected command dir: got %q want %q", got, want)
	}
	if got, want := filepath.Base(record.Path), "notify"; got != want {
		t.Fatalf("unexpected command path: got %q want basename %q", record.Path, want)
	}
}

func TestCommandNotifierRendersExecTemplates(t *testing.T) {
	tmpDir := t.TempDir()
	recordPath := filepath.Join(tmpDir, "notify.json")
	cfg := validManifest(tmpDir)
	cfg.Notifications.Command = notificationHookCommand(t, recordPath, "{{.Message}}", "{{.cid}}", "{{.Env.USER}}")
	cfg.Notifications.Command.Env = append(cfg.Notifications.Command.Env, "CUSTOM=1", "STATE={{.State}}", "MESSAGE={{.Message}}", "CID={{.cid}}")
	t.Setenv("USER", "template-user")
	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))
	notifier := newCommandNotifier(cfg, logger, &executor.Runner{Logger: logger})

	notifier.Notify(context.Background(), notifyStateRuntimeResume, "Restored", map[string]string{
		"cid": "7",
	})

	record := readNotificationHookRecord(t, recordPath)
	if got, want := notificationHookArgs(record.Args), []string{"Restored", "7", "template-user"}; !slices.Equal(got, want) {
		t.Fatalf("unexpected command args: got %v want %v", got, want)
	}
	for key, want := range map[string]string{"CUSTOM": "1", "STATE": "runtime:resume", "MESSAGE": "Restored", "CID": "7"} {
		if got := record.Env[key]; got != want {
			t.Fatalf("env %s: got %q want %q", key, got, want)
		}
	}
}

func TestCommandNotifierLogsAndIgnoresHookFailure(t *testing.T) {
	tmpDir := t.TempDir()
	recordPath := filepath.Join(tmpDir, "notify.json")
	cfg := validManifest(tmpDir)
	cfg.Notifications.Command = notificationHookCommand(t, recordPath)
	cfg.Notifications.Command.Env = append(cfg.Notifications.Command.Env, "VIRTLE_NOTIFY_EXIT=1")
	var logs bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logs, nil))
	notifier := newCommandNotifier(cfg, logger, &executor.Runner{Logger: logger})

	notifier.Notify(context.Background(), notifyStateRuntimeResume, "Restored", nil)

	_ = readNotificationHookRecord(t, recordPath)
	if !strings.Contains(logs.String(), "notification hook failed") || !strings.Contains(logs.String(), "state=runtime:resume") {
		t.Fatalf("expected hook failure log, got %q", logs.String())
	}
}

func TestSaveSuspendStateConnectedNotifiesAfterSavedStateWrite(t *testing.T) {
	tmpDir := t.TempDir()
	cfg := validManifest(tmpDir)
	cfg.QEMU.QMP.SocketPath = "qmp.sock"
	qmpSocketPath := filepath.Join(tmpDir, "qmp.sock")
	qmpClient := &fakeQMPClient{status: "running"}
	manager := &manager{qmpConnectTimeout: time.Millisecond}

	notifier := &recordingNotifier{
		onNotify: func() {
			if _, err := os.Stat(launch.SuspendStatePath(cfg)); err != nil {
				t.Fatalf("expected suspend state to exist before notification: %v", err)
			}
		},
	}
	manager.launchManifest = cfg
	if err := manager.saveSuspendStateConnected(context.Background(), qmpSocketPath, qmpClient, 7, notifier); err != nil {
		t.Fatalf("suspend: %v", err)
	}

	if got, want := len(notifier.calls), 1; got != want {
		t.Fatalf("expected one notification, got %d", got)
	}
	call := notifier.calls[0]
	if call.state != notifyStateRuntimeSuspend {
		t.Fatalf("unexpected notification state: got %q", call.state)
	}
	if call.values["vm_state_path"] != launch.VMStatePath(cfg) || call.values["cid"] != "7" {
		t.Fatalf("unexpected notification values: %#v", call.values)
	}
}

func TestLaunchResumeNotifiesAfterMigrationAndContinue(t *testing.T) {
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

	runner := &launchRunner{finishInteractiveSSH: true}
	qmpClient := &fakeQMPClient{
		status: "paused",
		onQuit: func() {
			runner.exitQEMU(nil)
		},
	}
	notifier := &recordingNotifier{
		onNotify: func() {
			qmpClient.mu.Lock()
			defer qmpClient.mu.Unlock()
			if qmpClient.migrateIncomingCalls != 1 || qmpClient.contCalls != 1 {
				t.Fatalf("notification fired before restore completed: migrate-incoming=%d cont=%d", qmpClient.migrateIncomingCalls, qmpClient.contCalls)
			}
		},
	}
	manager := &manager{
		locker:              &fileLocker{},
		runner:              runner,
		socketWaiter:        &fakeSocketWaiter{callback: func(paths []string) error { return nil }},
		qmpDialer:           &fakeQMPDialer{client: qmpClient},
		logger:              slog.New(slog.NewTextHandler(os.Stderr, nil)),
		shutdownDelay:       10 * time.Millisecond,
		qmpConnectTimeout:   time.Millisecond,
		qmpQuitTimeout:      time.Millisecond,
		qmpMigrationTimeout: time.Second,
		notifier:            notifier,
	}

	if err := manager.launchWithOptions(context.Background(), cfg, nil, LaunchOptions{Resume: ResumeModeForce, SSH: true}); err != nil {
		t.Fatalf("launch resume: %v", err)
	}

	if got, want := len(notifier.calls), 1; got != want {
		t.Fatalf("expected one notification, got %d", got)
	}
	call := notifier.calls[0]
	if call.state != notifyStateRuntimeResume {
		t.Fatalf("unexpected notification state: got %q", call.state)
	}
	if call.values["vm_state_path"] != statePath || call.values["cid"] != "3" {
		t.Fatalf("unexpected notification values: %#v", call.values)
	}
}

type notificationHookRecord struct {
	Path string            `json:"path"`
	Args []string          `json:"args"`
	Env  map[string]string `json:"env"`
	Dir  string            `json:"dir"`
}

func TestNotificationHookChild(t *testing.T) {
	if os.Getenv("VIRTLE_NOTIFY_CHILD") != "1" {
		return
	}
	recordPath := os.Getenv("VIRTLE_NOTIFY_RECORD")
	if recordPath == "" {
		t.Fatal("missing VIRTLE_NOTIFY_RECORD")
	}
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("get cwd: %v", err)
	}
	env := make(map[string]string)
	for _, entry := range os.Environ() {
		key, value, ok := strings.Cut(entry, "=")
		if ok {
			env[key] = value
		}
	}
	record := notificationHookRecord{
		Path: os.Args[0],
		Args: append([]string(nil), os.Args[1:]...),
		Env:  env,
		Dir:  dir,
	}
	data, err := json.Marshal(record)
	if err != nil {
		t.Fatalf("encode record: %v", err)
	}
	if err := os.WriteFile(recordPath, data, 0o644); err != nil {
		t.Fatalf("write record: %v", err)
	}
	if os.Getenv("VIRTLE_NOTIFY_EXIT") != "" {
		os.Exit(1)
	}
	os.Exit(0)
}

func notificationHookCommand(t *testing.T, recordPath string, args ...string) manifest.Command {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatalf("resolve test executable: %v", err)
	}
	return notificationHookCommandWithPath(recordPath, exe, args...)
}

func notificationHookCommandWithPath(recordPath string, path string, args ...string) manifest.Command {
	commandArgs := []string{"-test.run=TestNotificationHookChild", "--"}
	commandArgs = append(commandArgs, args...)
	return manifest.Command{
		Path: path,
		Args: commandArgs,
		Env: []string{
			"VIRTLE_NOTIFY_CHILD=1",
			"VIRTLE_NOTIFY_RECORD=" + recordPath,
		},
	}
}

func readNotificationHookRecord(t *testing.T, path string) notificationHookRecord {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read notification hook record: %v", err)
	}
	var record notificationHookRecord
	if err := json.Unmarshal(data, &record); err != nil {
		t.Fatalf("decode notification hook record: %v", err)
	}
	return record
}

func notificationHookArgs(args []string) []string {
	for i, arg := range args {
		if arg == "--" {
			return append([]string(nil), args[i+1:]...)
		}
	}
	return nil
}

type recordingNotification struct {
	state   string
	message string
	values  map[string]string
}

type recordingNotifier struct {
	calls    []recordingNotification
	onNotify func()
}

func (n *recordingNotifier) Notify(ctx context.Context, state string, message string, values map[string]string) {
	if n.onNotify != nil {
		n.onNotify()
	}
	n.calls = append(n.calls, recordingNotification{
		state:   state,
		message: message,
		values:  values,
	})
}
