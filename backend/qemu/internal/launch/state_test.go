package launch

import (
	"os"
	"os/user"
	"path/filepath"
	"strconv"
	"syscall"
	"testing"
	"time"

	"github.com/shazow/virtle/internal/manifest"
)

func TestSuspendStateRoundTrip(t *testing.T) {
	cfg := testManifest(t)
	state := SuspendState{
		QMPSocketPath: "/run/qmp.sock",
		VMStatePath:   VMStatePath(cfg),
		CID:           7,
		Status:        SuspendStatusSaved,
	}

	if err := WriteSuspendStateData(cfg, state); err != nil {
		t.Fatalf("write suspend state: %v", err)
	}
	assertPathMode(t, cfg.ResolvedPersistenceStateDir(), privateDirectoryMode)
	assertPathMode(t, SuspendStatePath(cfg), privateFileMode)
	readState, err := ReadSuspendState(cfg)
	if err != nil {
		t.Fatalf("read suspend state: %v", err)
	}
	if readState.HostName != cfg.Identity.HostName || readState.Status != SuspendStatusSaved || readState.CID != 7 || readState.Timestamp.IsZero() {
		t.Fatalf("unexpected suspend state: %#v", readState)
	}
	saved, err := HasSavedSuspendState(cfg)
	if err != nil {
		t.Fatalf("has saved suspend state: %v", err)
	}
	if !saved {
		t.Fatal("expected saved suspend state")
	}
	if err := RemoveSuspendState(cfg); err != nil {
		t.Fatalf("remove suspend state: %v", err)
	}
	if _, err := os.Stat(SuspendStatePath(cfg)); !os.IsNotExist(err) {
		t.Fatalf("expected suspend state removal, got %v", err)
	}
}

func TestPrepareVMStateFileForPrivilegeDroppedQEMU(t *testing.T) {
	account, err := user.Current()
	if err != nil {
		t.Fatalf("current user: %v", err)
	}
	uid, err := strconv.Atoi(account.Uid)
	if err != nil {
		t.Fatalf("parse current uid: %v", err)
	}
	gid, err := strconv.Atoi(account.Gid)
	if err != nil {
		t.Fatalf("parse current gid: %v", err)
	}
	cfg := testManifest(t)
	cfg.QEMU.RunAsUser = account.Username
	path := VMStatePath(cfg)

	prepared, err := PrepareVMStateFile(cfg)
	if err != nil {
		t.Fatalf("prepare vm state: %v", err)
	}
	if prepared != path {
		t.Fatalf("prepared path: got %q want %q", prepared, path)
	}
	for _, expected := range []struct {
		path string
		mode os.FileMode
		uid  int
		gid  int
	}{
		{path: filepath.Dir(path), mode: searchableDirectoryMode, uid: os.Getuid(), gid: gid},
		{path: path, mode: privateFileMode, uid: uid, gid: gid},
	} {
		info, err := os.Stat(expected.path)
		if err != nil {
			t.Fatalf("stat %q: %v", expected.path, err)
		}
		if got := info.Mode().Perm(); got != expected.mode {
			t.Fatalf("mode of %q: got %o want %o", expected.path, got, expected.mode)
		}
		stat, ok := info.Sys().(*syscall.Stat_t)
		if !ok {
			t.Fatalf("stat %q has type %T", expected.path, info.Sys())
		}
		if int(stat.Uid) != expected.uid || int(stat.Gid) != expected.gid {
			t.Fatalf("ownership of %q: got %d:%d want %d:%d", expected.path, stat.Uid, stat.Gid, expected.uid, expected.gid)
		}
	}
}

func TestRemoveRestoredSuspendState(t *testing.T) {
	cfg := testManifest(t)
	vmStatePath := VMStatePath(cfg)
	if err := os.MkdirAll(filepath.Dir(vmStatePath), 0o755); err != nil {
		t.Fatalf("create vm state dir: %v", err)
	}
	if err := os.WriteFile(vmStatePath, []byte("state"), 0o644); err != nil {
		t.Fatalf("write vm state: %v", err)
	}
	if err := WriteSuspendStateData(cfg, SuspendState{Status: SuspendStatusSaved, VMStatePath: vmStatePath}); err != nil {
		t.Fatalf("write suspend state: %v", err)
	}

	err := RemoveRestoredSuspendState(&Plan{Manifest: cfg, ResumeState: &SuspendState{VMStatePath: vmStatePath}})
	if err != nil {
		t.Fatalf("remove restored state: %v", err)
	}
	if _, err := os.Stat(vmStatePath); !os.IsNotExist(err) {
		t.Fatalf("expected vm state removal, got %v", err)
	}
	if _, err := os.Stat(SuspendStatePath(cfg)); !os.IsNotExist(err) {
		t.Fatalf("expected suspend state removal, got %v", err)
	}
}

func TestLaunchPIDRoundTrip(t *testing.T) {
	cfg := testManifest(t)
	if err := WriteLaunchPID(cfg, 12345); err != nil {
		t.Fatalf("write launch pid: %v", err)
	}
	assertPathMode(t, LaunchPIDPath(cfg), privateFileMode)
	if data, err := os.ReadFile(LaunchPIDPath(cfg)); err != nil || string(data) != "12345\n" {
		t.Fatalf("launch pid file: got %q err=%v want %q", data, err, "12345\n")
	}

	if err := RemoveLaunchPID(cfg, 12345); err != nil {
		t.Fatalf("remove launch pid: %v", err)
	}
	if _, err := os.Stat(LaunchPIDPath(cfg)); !os.IsNotExist(err) {
		t.Fatalf("expected launch pid removal, got %v", err)
	}
}

func assertPathMode(t *testing.T, path string, want os.FileMode) {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat %q: %v", path, err)
	}
	if got := info.Mode().Perm(); got != want {
		t.Fatalf("mode of %q: got %o want %o", path, got, want)
	}
}

func testManifest(t *testing.T) *manifest.Manifest {
	t.Helper()

	tmpDir := t.TempDir()
	return &manifest.Manifest{
		Identity: manifest.Identity{HostName: "agent"},
		Paths: manifest.Paths{
			WorkingDir: tmpDir,
			LockPath:   filepath.Join(tmpDir, ".agentspace", "agent.lock"),
		},
		Persistence: manifest.Persistence{
			StateDir: filepath.Join(tmpDir, ".agentspace"),
		},
		QEMU: manifest.QEMU{
			BinaryPath: "/bin/qemu",
			Memory:     manifest.QEMUMemory{Size: 128},
			QMP:        manifest.QEMUQMP{SocketPath: "qmp.sock"},
			Devices: manifest.QEMUDevices{
				RNG:   manifest.QEMURNGDevice{ID: "rng0", Transport: "pci"},
				VSOCK: manifest.QEMUVSOCKDevice{ID: "vsock0", Transport: "pci"},
			},
		},
		SSH: manifest.SSH{User: "agent", RetryDelay: 500 * time.Millisecond},
	}
}
