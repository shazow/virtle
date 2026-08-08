package launch

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"syscall"
	"testing"
)

// runGuestDirScript executes the directory install script under sh the same
// way the manager does: sh -c <script> sh <target> <owner> <mode>.
func runGuestDirScript(target, owner, mode string) error {
	cmd := exec.Command("sh", "-c", GuestDirectoryInstallScript(), "sh", target, owner, mode)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("%v: %s", err, out)
	}
	return nil
}

// statUIDGID reports the uid:gid of a path, or fails the test.
func statUIDGID(t *testing.T, path string) (uint32, uint32) {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat %q: %v", path, err)
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		t.Fatalf("stat %q: unexpected type %T", path, info.Sys())
	}
	return st.Uid, st.Gid
}

func TestGuestDirectoryInstallScriptCreatesMissingTree(t *testing.T) {
	base := t.TempDir()
	baseMode, err := os.Stat(base)
	if err != nil {
		t.Fatalf("stat base: %v", err)
	}
	target := filepath.Join(base, "a", "b", "c", "d")
	if err := runGuestDirScript(target, "", "0700"); err != nil {
		t.Fatalf("install dirs: %v", err)
	}
	for _, rel := range []string{"a", "a/b", "a/b/c", "a/b/c/d"} {
		dir := filepath.Join(base, rel)
		mode, err := os.Stat(dir)
		if err != nil {
			t.Fatalf("stat %q: %v", dir, err)
		}
		if got := mode.Mode().Perm(); got != 0o700 {
			t.Fatalf("mode of %q: got %o want %o", dir, got, 0o700)
		}
	}
	// The pre-existing ancestor must be left alone.
	if mode, err := os.Stat(base); err != nil || mode.Mode() != baseMode.Mode() {
		t.Fatalf("base dir changed: mode=%v err=%v", mode.Mode(), err)
	}
}

func TestGuestDirectoryInstallScriptAppliesModeToEachCreatedLevel(t *testing.T) {
	base := t.TempDir()
	target := filepath.Join(base, "a", "b")
	// dirMode is the mode InstallGuestFileDirectory hands the script after
	// converting a file mode: 0640 becomes 0750.
	if err := runGuestDirScript(target, "", "0750"); err != nil {
		t.Fatalf("install dirs: %v", err)
	}
	// A single deep install(1) would mode only the final directory; every
	// level must carry the requested mode.
	for _, rel := range []string{"a", "a/b"} {
		mode, err := os.Stat(filepath.Join(base, rel))
		if err != nil {
			t.Fatalf("stat %q: %v", rel, err)
		}
		if got := mode.Mode().Perm(); got != 0o750 {
			t.Fatalf("mode of %q: got %o want %o", rel, got, 0o750)
		}
	}
}

func TestGuestDirectoryInstallScriptLeavesExistingDirsUntouched(t *testing.T) {
	base := t.TempDir()
	existing := filepath.Join(base, "a")
	if err := os.Mkdir(existing, 0o755); err != nil {
		t.Fatalf("mkdir %q: %v", existing, err)
	}
	target := filepath.Join(existing, "b", "c")
	if err := runGuestDirScript(target, "", "0700"); err != nil {
		t.Fatalf("install dirs: %v", err)
	}
	if mode, err := os.Stat(existing); err != nil || mode.Mode().Perm() != 0o755 {
		t.Fatalf("existing dir changed: mode=%o err=%v", mode.Mode().Perm(), err)
	}
	for _, rel := range []string{"a/b", "a/b/c"} {
		mode, err := os.Stat(filepath.Join(base, rel))
		if err != nil {
			t.Fatalf("stat %q: %v", rel, err)
		}
		if got := mode.Mode().Perm(); got != 0o700 {
			t.Fatalf("mode of %q: got %o want %o", rel, got, 0o700)
		}
	}
}

func TestGuestDirectoryInstallScriptNoopForExistingTree(t *testing.T) {
	base := t.TempDir()
	target := filepath.Join(base, "a", "b")
	if err := os.MkdirAll(target, 0o700); err != nil {
		t.Fatalf("mkdir all: %v", err)
	}
	if err := runGuestDirScript(target, "", "0700"); err != nil {
		t.Fatalf("install dirs on existing tree: %v", err)
	}
	if mode, err := os.Stat(target); err != nil || mode.Mode().Perm() != 0o700 {
		t.Fatalf("existing tree changed: mode=%o err=%v", mode.Mode().Perm(), err)
	}
}

func TestGuestDirectoryInstallScriptEmptyOwnerAndMode(t *testing.T) {
	base := t.TempDir()
	target := filepath.Join(base, "a", "b")
	if err := runGuestDirScript(target, "", ""); err != nil {
		t.Fatalf("install dirs with empty owner/mode: %v", err)
	}
	for _, rel := range []string{"a", "a/b"} {
		if info, err := os.Stat(filepath.Join(base, rel)); err != nil || !info.IsDir() {
			t.Fatalf("%q: not a directory (err=%v)", rel, err)
		}
	}
}

func TestGuestDirectoryInstallScriptAppliesOwner(t *testing.T) {
	current, err := user.Current()
	if err != nil {
		t.Skipf("cannot resolve current user: %v", err)
	}
	group, err := user.LookupGroupId(current.Gid)
	if err != nil {
		t.Skipf("cannot resolve current group: %v", err)
	}
	base := t.TempDir()
	target := filepath.Join(base, "a", "b")
	owner := fmt.Sprintf("%s:%s", current.Username, group.Name)
	if err := runGuestDirScript(target, owner, "0750"); err != nil {
		t.Fatalf("install dirs with owner %q: %v", owner, err)
	}
	wantUID, wantGID := uint32(os.Geteuid()), uint32(os.Getegid())
	for _, rel := range []string{"a", "a/b"} {
		uid, gid := statUIDGID(t, filepath.Join(base, rel))
		if uid != wantUID || gid != wantGID {
			t.Fatalf("ownership of %q: got %d:%d want %d:%d", rel, uid, gid, wantUID, wantGID)
		}
	}
}

func TestGuestDirectoryInstallScriptFailsOnUnknownOwner(t *testing.T) {
	base := t.TempDir()
	target := filepath.Join(base, "a", "b")
	if err := runGuestDirScript(target, "no-such-user-virtle-test", "0750"); err == nil {
		t.Fatalf("expected install -o with unknown owner to fail")
	}
}

func TestGuestDirectoryInstallScriptPathWithSpaces(t *testing.T) {
	base := t.TempDir()
	target := filepath.Join(base, "with space", "deep dir", "leaf")
	if err := runGuestDirScript(target, "", "0755"); err != nil {
		t.Fatalf("install dirs into path with spaces: %v", err)
	}
	if info, err := os.Stat(target); err != nil || !info.IsDir() {
		t.Fatalf("target %q not created (err=%v)", target, err)
	}
}

func TestInstallGuestFileDirectoryDelegatesToInstaller(t *testing.T) {
	var gotGuestDir, gotOwner, gotMode string
	calls := 0
	installer := GuestDirectoryInstaller{
		InstallTree: func(_ context.Context, guestDir, owner, mode string) error {
			calls++
			gotGuestDir, gotOwner, gotMode = guestDir, owner, mode
			return nil
		},
	}

	if err := InstallGuestFileDirectory(context.Background(), installer, "/var/lib/virtle/config.json", "agent:users", "0640"); err != nil {
		t.Fatalf("install guest directory: %v", err)
	}
	if calls != 1 {
		t.Fatalf("installer calls: got %d want 1", calls)
	}
	if gotGuestDir != "/var/lib/virtle" || gotOwner != "agent:users" || gotMode != "0750" {
		t.Fatalf("install tree args: got (%q, %q, %q) want (/var/lib/virtle, agent:users, 0750)", gotGuestDir, gotOwner, gotMode)
	}
}

func TestInstallGuestFileDirectoryNoopsForRootOrCurrentDirectory(t *testing.T) {
	called := false
	installer := GuestDirectoryInstaller{
		InstallTree: func(context.Context, string, string, string) error {
			called = true
			return nil
		},
	}
	for _, guestPath := range []string{"file", "/file"} {
		t.Run(guestPath, func(t *testing.T) {
			called = false
			if err := InstallGuestFileDirectory(context.Background(), installer, guestPath, "", ""); err != nil {
				t.Fatalf("install guest directory: %v", err)
			}
			if called {
				t.Fatalf("expected no installer calls for %q", guestPath)
			}
		})
	}
}

func TestInstallGuestFileDirectoryRequiresInstaller(t *testing.T) {
	err := InstallGuestFileDirectory(context.Background(), GuestDirectoryInstaller{}, "/etc/virtle/config.json", "", "")
	if err == nil {
		t.Fatalf("expected missing installer to fail")
	}
}

func TestInstallGuestFileDirectoryPropagatesInstallerError(t *testing.T) {
	installErr := errors.New("install failed")
	err := InstallGuestFileDirectory(context.Background(), GuestDirectoryInstaller{
		InstallTree: func(context.Context, string, string, string) error {
			return installErr
		},
	}, "/etc/virtle/config.json", "", "")
	if !errors.Is(err, installErr) {
		t.Fatalf("install error: got %v want %v", err, installErr)
	}
}

func TestGuestDirectoryMode(t *testing.T) {
	tests := []struct {
		mode string
		want string
	}{
		{mode: "0644", want: "0755"},
		{mode: "0600", want: "0700"},
		{mode: "0640", want: "0750"},
		{mode: "0000", want: "0000"},
		{mode: "0755", want: "0755"},
		{mode: "0400", want: "0500"},
		{mode: "666", want: "777"},
		{mode: "malformed", want: "malformed"},
		{mode: "12", want: "12"},
		{mode: "", want: ""},
	}
	for _, tt := range tests {
		t.Run(tt.mode, func(t *testing.T) {
			if got := guestDirectoryMode(tt.mode); got != tt.want {
				t.Fatalf("guestDirectoryMode(%q): got %q want %q", tt.mode, got, tt.want)
			}
		})
	}
}
