package launch

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"

	"github.com/shazow/virtle/backend/qemu/internal/qga"
)

// runGuestDirScriptIn executes the exact guest command that the script
// installer issues, from workDir (or the current directory when empty), so
// the tests exercise the same invocation production wires to the guest.
func runGuestDirScriptIn(workDir, target, owner, mode string) error {
	installer := ScriptGuestDirectoryInstaller(func(_ context.Context, _ string, path string, args []string) error {
		cmd := exec.Command(path, args...)
		cmd.Dir = workDir
		if out, err := cmd.CombinedOutput(); err != nil {
			return fmt.Errorf("%v: %s", err, out)
		}
		return nil
	})
	return installer.InstallTree(context.Background(), target, owner, mode)
}

func runGuestDirScript(target, owner, mode string) error {
	return runGuestDirScriptIn("", target, owner, mode)
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

func TestGuestDirectoryInstallScriptCreatesTreeFromFirstComponent(t *testing.T) {
	// A target whose very first path component is missing exercises the same
	// walk as creating a tree directly under the guest's / (which the tests
	// cannot do unprivileged): every level from the top must be created.
	base := t.TempDir()
	if err := runGuestDirScriptIn(base, "x/y/z", "", "0755"); err != nil {
		t.Fatalf("install dirs from first component: %v", err)
	}
	if info, err := os.Stat(filepath.Join(base, "x", "y", "z")); err != nil || !info.IsDir() {
		t.Fatalf("target not created (err=%v)", err)
	}
}

func TestGuestDirectoryInstallScriptLeavesConcurrentlyCreatedDirsUntouched(t *testing.T) {
	// Simulate another guest process winning the creation race: a mkdir shim
	// on PATH creates the directory with a different mode before running the
	// real mkdir, so the script's mkdir loses with EEXIST and must treat the
	// directory as existing instead of applying the requested mode to it.
	realMkdir, err := exec.LookPath("mkdir")
	if err != nil {
		t.Skipf("cannot resolve mkdir: %v", err)
	}
	shimDir := t.TempDir()
	shim := fmt.Sprintf("#!/bin/sh\nfor last; do :; done\n%q -m 0755 \"$last\"\nexec %q \"$@\"\n", realMkdir, realMkdir)
	if err := os.WriteFile(filepath.Join(shimDir, "mkdir"), []byte(shim), 0o755); err != nil {
		t.Fatalf("write mkdir shim: %v", err)
	}

	base := t.TempDir()
	target := filepath.Join(base, "a", "b")
	installer := ScriptGuestDirectoryInstaller(func(_ context.Context, _ string, path string, args []string) error {
		cmd := exec.Command(path, args...)
		cmd.Env = append(os.Environ(), "PATH="+shimDir+":"+os.Getenv("PATH"))
		if out, err := cmd.CombinedOutput(); err != nil {
			return fmt.Errorf("%v: %s", err, out)
		}
		return nil
	})
	if err := installer.InstallTree(context.Background(), target, "", "0700"); err != nil {
		t.Fatalf("install dirs against racing mkdir: %v", err)
	}
	for _, rel := range []string{"a", "a/b"} {
		info, err := os.Stat(filepath.Join(base, rel))
		if err != nil {
			t.Fatalf("stat %q: %v", rel, err)
		}
		if got := info.Mode().Perm(); got != 0o755 {
			t.Fatalf("mode of %q: got %o want %o (directories the script did not create must stay untouched)", rel, got, 0o755)
		}
	}
}

func TestGuestDirectoryInstallScriptFailsWhenComponentIsFile(t *testing.T) {
	base := t.TempDir()
	blocker := filepath.Join(base, "a")
	if err := os.WriteFile(blocker, nil, 0o644); err != nil {
		t.Fatalf("write blocker file: %v", err)
	}
	if err := runGuestDirScript(filepath.Join(blocker, "b"), "", "0755"); err == nil {
		t.Fatalf("expected install through file component to fail")
	}
}

func TestGuestDirectoryInstallScriptAppliesGroupOnlyOwner(t *testing.T) {
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
	if err := runGuestDirScript(target, ":"+group.Name, "0750"); err != nil {
		t.Fatalf("install dirs with group-only owner: %v", err)
	}
	_, gid := statUIDGID(t, target)
	if gid != uint32(os.Getegid()) {
		t.Fatalf("group of %q: got %d want %d", target, gid, os.Getegid())
	}
}

func TestScriptGuestDirectoryInstallerFallsBackToPathShell(t *testing.T) {
	// Guests whose agent PATH has no shell fail to start /bin/sh only when the
	// guest keeps its shell elsewhere; the PATH-resolved name must still run.
	var tried []string
	installer := ScriptGuestDirectoryInstaller(func(_ context.Context, _ string, path string, args []string) error {
		tried = append(tried, path)
		if path != "sh" {
			return &qga.ExecStartError{Path: path, Err: errors.New("No such file or directory")}
		}
		if len(args) < 3 || args[0] != "-c" || args[2] != path {
			t.Fatalf("script args: got %q", args)
		}
		return nil
	})

	if err := installer.InstallTree(context.Background(), "/var/lib/virtle", "", "0750"); err != nil {
		t.Fatalf("install dirs with fallback shell: %v", err)
	}
	if want := []string{"/bin/sh", "sh"}; !slices.Equal(tried, want) {
		t.Fatalf("shells tried: got %v want %v", tried, want)
	}
}

func TestScriptGuestDirectoryInstallerKeepsScriptFailure(t *testing.T) {
	// The script ran and failed: retrying under another shell would only
	// repeat the failure and hide its error.
	scriptErr := errors.New("mkdir: permission denied")
	calls := 0
	installer := ScriptGuestDirectoryInstaller(func(context.Context, string, string, []string) error {
		calls++
		return scriptErr
	})

	err := installer.InstallTree(context.Background(), "/var/lib/virtle", "", "")
	if !errors.Is(err, scriptErr) {
		t.Fatalf("install error: got %v want %v", err, scriptErr)
	}
	if calls != 1 {
		t.Fatalf("runner calls: got %d want 1", calls)
	}
}

func TestScriptGuestDirectoryInstallerReportsMissingShell(t *testing.T) {
	// No candidate started: the error names every shell tried and keeps each
	// guest-agent failure so the guest-side cause stays visible.
	installer := ScriptGuestDirectoryInstaller(func(_ context.Context, _ string, path string, _ []string) error {
		return fmt.Errorf("install dirs %q: %w", "/var/lib/virtle", &qga.ExecStartError{Path: path, Err: errors.New("No such file or directory")})
	})

	err := installer.InstallTree(context.Background(), "/var/lib/virtle", "", "")
	if err == nil {
		t.Fatalf("expected install without any guest shell to fail")
	}
	if !GuestCommandNotStarted(err) {
		t.Fatalf("install error must stay recognizable as a start failure: %v", err)
	}
	for _, want := range []string{"/bin/sh", "sh", "guest agent's PATH", "No such file or directory"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("install error %q does not mention %q", err.Error(), want)
		}
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
