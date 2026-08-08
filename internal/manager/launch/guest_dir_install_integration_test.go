//go:build integration

package launch

// Integration coverage for the guest directory install script, gated behind
// the "integration" build tag because it exercises the real filesystem and
// requires dash:
//
//	go test -tags=integration ./internal/manager/launch/
//
// The unit tests run the script under whatever sh(1) resolves to; this test
// pins the script's POSIX claim by running it explicitly under dash.

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"testing"
)

// runGuestDirScriptDash executes the exact guest command the script installer
// issues, forcing the dash interpreter in place of the guest's sh.
func runGuestDirScriptDash(t *testing.T, target, owner, mode string) error {
	t.Helper()
	dash, err := exec.LookPath("dash")
	if err != nil {
		t.Skipf("dash not installed: %v", err)
	}
	installer := ScriptGuestDirectoryInstaller(func(_ context.Context, _ string, _ string, args []string) error {
		cmd := exec.Command(dash, args...)
		if out, err := cmd.CombinedOutput(); err != nil {
			return fmt.Errorf("%v: %s", err, out)
		}
		return nil
	})
	return installer.InstallTree(context.Background(), target, owner, mode)
}

func TestIntegrationGuestDirectoryInstallScriptUnderDash(t *testing.T) {
	owner := ""
	wantUID, wantGID := uint32(os.Geteuid()), uint32(os.Getegid())
	if current, err := user.Current(); err == nil {
		if group, err := user.LookupGroupId(current.Gid); err == nil {
			owner = current.Username + ":" + group.Name
		}
	}
	if owner == "" {
		t.Log("cannot resolve current user and group; skipping ownership assertions")
	}

	base := t.TempDir()
	existing := filepath.Join(base, "existing")
	if err := os.Mkdir(existing, 0o755); err != nil {
		t.Fatalf("mkdir %q: %v", existing, err)
	}

	target := filepath.Join(base, "a", "b c", "d")
	if err := runGuestDirScriptDash(t, target, owner, "0750"); err != nil {
		t.Fatalf("install dirs under dash: %v", err)
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
		if owner != "" {
			uid, gid := statUIDGID(t, dir)
			if uid != wantUID || gid != wantGID {
				t.Fatalf("ownership of %q: got %d:%d want %d:%d", dir, uid, gid, wantUID, wantGID)
			}
		}
	}

	// A re-run with a different mode must leave the now-existing tree alone.
	if err := runGuestDirScriptDash(t, target, owner, "0700"); err != nil {
		t.Fatalf("re-run install dirs under dash: %v", err)
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
	if err := runGuestDirScriptDash(t, filepath.Join(blocker, "sub"), "", ""); err == nil {
		t.Fatalf("expected install through file component to fail")
	}
	if info, err := os.Stat(blocker); err != nil || !info.Mode().IsRegular() {
		t.Fatalf("blocker file changed: mode=%v err=%v", info.Mode(), err)
	}
}
