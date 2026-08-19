//go:build integration

package launch

// Integration coverage for the guest directory install script, gated behind
// the "integration" build tag because it exercises the real filesystem:
//
//	go test -tags=integration ./backend/qemu/internal/launch/
//
// The `integration` flake check runs this suite in a small VM whose /bin/sh is
// dash: nix flake check
//
// The test executes the exact guest command emitted by the installer, covering
// both its absolute shell path and the script's behavior under a POSIX shell.

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// runGuestDirScriptIntegration executes the exact guest command the script
// installer issues. Unlike the unit-test helper, it also pins the absolute
// shell path required by QGA installations whose service PATH lacks sh.
func runGuestDirScriptIntegration(t *testing.T, target, owner, mode string) error {
	t.Helper()
	installer := ScriptGuestDirectoryInstaller(func(_ context.Context, _ string, path string, args []string) error {
		if path != "/bin/sh" {
			return fmt.Errorf("guest shell path = %q, want /bin/sh", path)
		}
		cmd := exec.Command(path, args...)
		if out, err := cmd.CombinedOutput(); err != nil {
			return fmt.Errorf("%v: %s", err, out)
		}
		return nil
	})
	return installer.InstallTree(context.Background(), target, owner, mode)
}

func TestIntegrationGuestDirectoryInstallScript(t *testing.T) {
	wantUID, wantGID := uint32(os.Geteuid()), uint32(os.Getegid())
	owner := fmt.Sprintf("%d:%d", wantUID, wantGID)

	base := t.TempDir()
	existing := filepath.Join(base, "existing")
	if err := os.Mkdir(existing, 0o755); err != nil {
		t.Fatalf("mkdir %q: %v", existing, err)
	}

	target := filepath.Join(base, "a", "b c", "d")
	if err := runGuestDirScriptIntegration(t, target, owner, "0750"); err != nil {
		t.Fatalf("install dirs: %v", err)
	}
	for _, rel := range []string{"a", "a/b c", "a/b c/d"} {
		dir := filepath.Join(base, rel)
		info, err := os.Stat(dir)
		if err != nil {
			t.Fatalf("stat %q: %v", dir, err)
		}
		if got := info.Mode().Perm(); got != 0o750 {
			t.Fatalf("mode of %q: got %o want %o", dir, got, 0o750)
		}
		uid, gid := statUIDGID(t, dir)
		if uid != wantUID || gid != wantGID {
			t.Fatalf("ownership of %q: got %d:%d want %d:%d", dir, uid, gid, wantUID, wantGID)
		}
	}

	// A re-run with a different mode must leave the now-existing tree alone.
	if err := runGuestDirScriptIntegration(t, target, owner, "0700"); err != nil {
		t.Fatalf("re-run install dirs: %v", err)
	}
	if info, err := os.Stat(target); err != nil || info.Mode().Perm() != 0o750 {
		t.Fatalf("re-run changed existing tree: mode=%o err=%v", info.Mode().Perm(), err)
	}

	// Directories that were already there stay untouched.
	if info, err := os.Stat(existing); err != nil || info.Mode().Perm() != 0o755 {
		t.Fatalf("pre-existing dir changed: mode=%o err=%v", info.Mode().Perm(), err)
	}

	// A regular file in the way must surface an error, not be replaced.
	blocker := filepath.Join(base, "blocker")
	if err := os.WriteFile(blocker, nil, 0o644); err != nil {
		t.Fatalf("write blocker file: %v", err)
	}
	if err := runGuestDirScriptIntegration(t, filepath.Join(blocker, "sub"), "", ""); err == nil {
		t.Fatalf("expected install through file component to fail")
	}
	if info, err := os.Stat(blocker); err != nil || !info.Mode().IsRegular() {
		t.Fatalf("blocker file changed: mode=%v err=%v", info.Mode(), err)
	}
}
