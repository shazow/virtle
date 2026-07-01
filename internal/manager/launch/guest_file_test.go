package launch

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/shazow/virtle/internal/manifest"
)

func TestGuestFilePayloadRejectsHostSymlinkWhenFollowLinksFalse(t *testing.T) {
	tmpDir := t.TempDir()
	targetPath := filepath.Join(tmpDir, "target")
	if err := os.WriteFile(targetPath, []byte("target content"), 0o644); err != nil {
		t.Fatalf("write target fixture: %v", err)
	}
	linkPath := filepath.Join(tmpDir, "link")
	if err := os.Symlink(targetPath, linkPath); err != nil {
		t.Fatalf("create symlink fixture: %v", err)
	}

	_, err := guestFilePayloadBase64(manifest.ResolvedWriteFile{
		GuestPath:   "/etc/from-link",
		Content:     manifest.WriteFileContent{Kind: manifest.WriteFileContentPath, Path: linkPath},
		FollowLinks: false,
	})
	if err == nil || !strings.Contains(err.Error(), "followLinks is false") {
		t.Fatalf("expected followLinks symlink error, got %v", err)
	}

	payload, err := guestFilePayloadBase64(manifest.ResolvedWriteFile{
		GuestPath:   "/etc/from-link",
		Content:     manifest.WriteFileContent{Kind: manifest.WriteFileContentPath, Path: linkPath},
		FollowLinks: true,
	})
	if err != nil {
		t.Fatalf("expected followLinks=true to read symlink target: %v", err)
	}
	if got, want := payload, "dGFyZ2V0IGNvbnRlbnQ="; got != want {
		t.Fatalf("unexpected symlink target payload: got %q want %q", got, want)
	}
}

func TestGuestInstallDirectoryArgs(t *testing.T) {
	tests := []struct {
		name     string
		chown    string
		mode     string
		expected []string
	}{
		{
			name:     "nil chown",
			expected: []string{"-d", "/etc/virtle"},
		},
		{
			name:     "empty chown",
			chown:    "",
			expected: []string{"-d", "/etc/virtle"},
		},
		{
			name:     "user and group",
			chown:    "agent:users",
			expected: []string{"-d", "-o", "agent", "-g", "users", "/etc/virtle"},
		},
		{
			name:     "user only",
			chown:    "agent",
			expected: []string{"-d", "-o", "agent", "/etc/virtle"},
		},
		{
			name:     "group only",
			chown:    ":users",
			expected: []string{"-d", "-g", "users", "/etc/virtle"},
		},
		{
			name:     "mode",
			mode:     "0640",
			expected: []string{"-d", "-m", "0750", "/etc/virtle"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := guestInstallDirectoryArgs("/etc/virtle", tt.chown, tt.mode); !reflect.DeepEqual(got, tt.expected) {
				t.Fatalf("unexpected install args: got %#v want %#v", got, tt.expected)
			}
		})
	}
}

func TestWriteGuestFilesSkipsExistingNoOverwriteFile(t *testing.T) {
	var events []string
	err := WriteGuestFiles(context.Background(), []manifest.ResolvedWriteFile{
		{
			GuestPath: "/etc/virtle/existing",
			Overwrite: false,
			Content: manifest.WriteFileContent{
				Kind: manifest.WriteFileContentText,
				Text: "ignored",
			},
		},
	}, GuestFileWriter{
		PathExists: func(context.Context, string) (bool, error) {
			events = append(events, "exists")
			return true, nil
		},
		InstallDirectory: func(context.Context, manifest.ResolvedWriteFile) error {
			t.Fatalf("install should not run for skipped file")
			return nil
		},
		WriteFile: func(context.Context, string, string) error {
			t.Fatalf("write should not run for skipped file")
			return nil
		},
		SkipExisting: func(guestPath string) {
			events = append(events, "skip:"+guestPath)
		},
	})
	if err != nil {
		t.Fatalf("write guest files: %v", err)
	}
	if want := []string{"exists", "skip:/etc/virtle/existing"}; !reflect.DeepEqual(events, want) {
		t.Fatalf("events: got %#v want %#v", events, want)
	}
}

func TestWriteGuestFilesWritesAndAppliesMetadataInOrder(t *testing.T) {
	var events []string
	err := WriteGuestFiles(context.Background(), []manifest.ResolvedWriteFile{
		{
			GuestPath: "/etc/virtle/config",
			Chown:     "agent:users",
			Mode:      "0640",
			Overwrite: true,
			Content: manifest.WriteFileContent{
				Kind: manifest.WriteFileContentText,
				Text: "hello",
			},
		},
	}, GuestFileWriter{
		PathExists: func(context.Context, string) (bool, error) {
			t.Fatalf("exists should not run for overwrite file")
			return false, nil
		},
		InstallDirectory: func(_ context.Context, file manifest.ResolvedWriteFile) error {
			events = append(events, "install:"+file.GuestPath+":"+file.Chown+":"+file.Mode)
			return nil
		},
		WriteFile: func(_ context.Context, guestPath string, payloadBase64 string) error {
			events = append(events, "write:"+guestPath+":"+payloadBase64)
			return nil
		},
		Chown: func(_ context.Context, guestPath string, owner string) error {
			events = append(events, "chown:"+guestPath+":"+owner)
			return nil
		},
		Chmod: func(_ context.Context, guestPath string, mode string) error {
			events = append(events, "chmod:"+guestPath+":"+mode)
			return nil
		},
		Wrote: func(guestPath string) {
			events = append(events, "wrote:"+guestPath)
		},
	})
	if err != nil {
		t.Fatalf("write guest files: %v", err)
	}
	want := []string{
		"install:/etc/virtle/config:agent:users:0640",
		"write:/etc/virtle/config:aGVsbG8=",
		"chown:/etc/virtle/config:agent:users",
		"chmod:/etc/virtle/config:0640",
		"wrote:/etc/virtle/config",
	}
	if !reflect.DeepEqual(events, want) {
		t.Fatalf("events: got %#v want %#v", events, want)
	}
}

func TestWriteGuestFilesWrapsStage(t *testing.T) {
	writeErr := errors.New("write failed")
	err := WriteGuestFiles(context.Background(), []manifest.ResolvedWriteFile{
		{
			GuestPath: "/etc/virtle/config",
			Overwrite: true,
			Content: manifest.WriteFileContent{
				Kind: manifest.WriteFileContentText,
				Text: "hello",
			},
		},
	}, GuestFileWriter{
		InstallDirectory: func(context.Context, manifest.ResolvedWriteFile) error {
			return nil
		},
		WriteFile: func(context.Context, string, string) error {
			return writeErr
		},
	})
	var stageErr *StageError
	if !errors.As(err, &stageErr) || stageErr.Stage != "guest file write" || !errors.Is(err, writeErr) {
		t.Fatalf("stage err: got %v", err)
	}
}

func TestMountWorkspaceCWDInstallsAndMountsTarget(t *testing.T) {
	var events []string
	err := MountWorkspaceCWD(context.Background(), &manifest.Manifest{
		Paths: manifest.Paths{
			WorkingDir: "/home/agent/workspace/project",
		},
		Workspace: manifest.Workspace{
			GuestDir: "/workspace",
		},
	}, WorkspaceCWDMounter{
		InstallDir: func(_ context.Context, target string, args []string) error {
			events = append(events, "install:"+target+":"+strings.Join(args, ","))
			return nil
		},
		MountBind: func(_ context.Context, source string, target string, args []string) error {
			events = append(events, "mount:"+source+":"+target+":"+strings.Join(args, ","))
			return nil
		},
		Mounted: func(source string, target string) {
			events = append(events, "mounted:"+source+":"+target)
		},
	})
	if err != nil {
		t.Fatalf("mount workspace cwd: %v", err)
	}
	want := []string{
		"install:/workspace/project:-d,/workspace,/workspace/project",
		"mount:/mnt/cwd:/workspace/project:--bind,/mnt/cwd,/workspace/project",
		"mounted:/mnt/cwd:/workspace/project",
	}
	if !reflect.DeepEqual(events, want) {
		t.Fatalf("events: got %#v want %#v", events, want)
	}
}

func TestMountWorkspaceCWDWrapsStage(t *testing.T) {
	err := MountWorkspaceCWD(context.Background(), &manifest.Manifest{
		Paths: manifest.Paths{WorkingDir: "/home/agent/project"},
	}, WorkspaceCWDMounter{})
	var stageErr *StageError
	if !errors.As(err, &stageErr) || stageErr.Stage != "workspace cwd mount" {
		t.Fatalf("stage err: got %v", err)
	}
}

func TestMountWorkspaceCWDValidatesInputs(t *testing.T) {
	tests := []struct {
		name           string
		manifest       manifest.Manifest
		expectedSubstr string
	}{
		{
			name: "missing guest dir",
			manifest: manifest.Manifest{
				Paths: manifest.Paths{WorkingDir: "/home/agent/project"},
			},
			expectedSubstr: "workspace.guest_dir is required",
		},
		{
			name: "root working dir",
			manifest: manifest.Manifest{
				Paths:     manifest.Paths{WorkingDir: "/"},
				Workspace: manifest.Workspace{GuestDir: "/workspace"},
			},
			expectedSubstr: "derive workspace cwd name",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := MountWorkspaceCWD(context.Background(), &tt.manifest, WorkspaceCWDMounter{
				InstallDir: func(context.Context, string, []string) error {
					t.Fatalf("install should not run for invalid input")
					return nil
				},
				MountBind: func(context.Context, string, string, []string) error {
					t.Fatalf("mount should not run for invalid input")
					return nil
				},
			})
			if err == nil || !strings.Contains(err.Error(), tt.expectedSubstr) {
				t.Fatalf("expected %q error, got %v", tt.expectedSubstr, err)
			}
		})
	}
}

func TestGuestWriteBackFilesFiltersEnabledFiles(t *testing.T) {
	files := []manifest.ResolvedWriteFile{
		{GuestPath: "/guest/a", WriteBack: true},
		{GuestPath: "/guest/b", WriteBack: false},
		{GuestPath: "/guest/c", WriteBack: true},
	}
	got := GuestWriteBackFiles(files)
	want := []manifest.ResolvedWriteFile{files[0], files[2]}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("write-back files: got %#v want %#v", got, want)
	}
}

func TestWriteBackGuestFilesReadsGuestAndWritesHost(t *testing.T) {
	tmpDir := t.TempDir()
	hostPath := filepath.Join(tmpDir, "host")
	if err := os.WriteFile(hostPath, []byte("original"), 0o644); err != nil {
		t.Fatalf("write host fixture: %v", err)
	}
	var events []string
	err := WriteBackGuestFiles(context.Background(), []manifest.ResolvedWriteFile{
		{
			GuestPath: "/var/lib/virtle/host",
			Content: manifest.WriteFileContent{
				Kind: manifest.WriteFileContentPath,
				Path: hostPath,
			},
		},
	}, GuestFileWriteBacker{
		ReadFile: func(_ context.Context, guestPath string) ([]byte, error) {
			events = append(events, "read:"+guestPath)
			return []byte("guest content"), nil
		},
		WriteHostFile: func(path string, data []byte) error {
			events = append(events, "write:"+path+":"+string(data))
			return nil
		},
		Wrote: func(guestPath string, hostPath string) {
			events = append(events, "wrote:"+guestPath+":"+hostPath)
		},
	})
	if err != nil {
		t.Fatalf("write back guest files: %v", err)
	}
	want := []string{
		"read:/var/lib/virtle/host",
		"write:" + hostPath + ":guest content",
		"wrote:/var/lib/virtle/host:" + hostPath,
	}
	if !reflect.DeepEqual(events, want) {
		t.Fatalf("events: got %#v want %#v", events, want)
	}
}

func TestWriteBackGuestFilesFailsWhenHostPathIsMissing(t *testing.T) {
	tmpDir := t.TempDir()
	hostPath := filepath.Join(tmpDir, "host")
	err := WriteBackGuestFiles(context.Background(), []manifest.ResolvedWriteFile{
		{
			GuestPath: "/var/lib/virtle/host",
			Content: manifest.WriteFileContent{
				Kind: manifest.WriteFileContentPath,
				Path: hostPath,
			},
		},
	}, GuestFileWriteBacker{
		ReadFile: func(context.Context, string) ([]byte, error) {
			return []byte("guest content"), nil
		},
		WriteHostFile: WriteHostFileAtomic,
	})
	if !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("expected missing host path error, got %v", err)
	}
	if _, statErr := os.Stat(hostPath); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("write-back should not create missing host path; stat error = %v", statErr)
	}
}

func TestWriteBackGuestFilesRejectsFilesWithoutHostPath(t *testing.T) {
	err := WriteBackGuestFiles(context.Background(), []manifest.ResolvedWriteFile{
		{
			GuestPath: "/var/lib/virtle/text",
			Content: manifest.WriteFileContent{
				Kind: manifest.WriteFileContentText,
				Text: "content",
			},
		},
	}, GuestFileWriteBacker{
		ReadFile: func(context.Context, string) ([]byte, error) {
			return []byte("guest content"), nil
		},
		WriteHostFile: func(string, []byte) error {
			t.Fatalf("host write should not run without host path")
			return nil
		},
	})
	if err == nil || !strings.Contains(err.Error(), "has no host path") {
		t.Fatalf("expected missing host path error, got %v", err)
	}
}

func TestWriteBackGuestFilesWrapsHostWriteError(t *testing.T) {
	tmpDir := t.TempDir()
	hostPath := filepath.Join(tmpDir, "host")
	if err := os.WriteFile(hostPath, []byte("original"), 0o644); err != nil {
		t.Fatalf("write host fixture: %v", err)
	}
	writeErr := errors.New("disk full")
	err := WriteBackGuestFiles(context.Background(), []manifest.ResolvedWriteFile{
		{
			GuestPath: "/var/lib/virtle/host",
			Content: manifest.WriteFileContent{
				Kind: manifest.WriteFileContentPath,
				Path: hostPath,
			},
		},
	}, GuestFileWriteBacker{
		ReadFile: func(context.Context, string) ([]byte, error) {
			return []byte("guest content"), nil
		},
		WriteHostFile: func(string, []byte) error {
			return writeErr
		},
	})
	if !errors.Is(err, writeErr) || !strings.Contains(err.Error(), "write host file") {
		t.Fatalf("expected wrapped host write error, got %v", err)
	}
}

func TestWriteBackGuestFilesWrapsStage(t *testing.T) {
	readErr := errors.New("read failed")
	err := WriteBackGuestFiles(context.Background(), []manifest.ResolvedWriteFile{
		{
			GuestPath: "/var/lib/virtle/host",
			Content: manifest.WriteFileContent{
				Kind: manifest.WriteFileContentPath,
				Path: "/tmp/virtle-host",
			},
		},
	}, GuestFileWriteBacker{
		ReadFile: func(context.Context, string) ([]byte, error) {
			return nil, readErr
		},
	})
	var stageErr *StageError
	if !errors.As(err, &stageErr) || stageErr.Stage != "guest file write-back" || !errors.Is(err, readErr) {
		t.Fatalf("stage err: got %v", err)
	}
}

func TestInstallGuestFileDirectoryNoopsForRootOrCurrentDirectory(t *testing.T) {
	called := false
	installer := GuestDirectoryInstaller{
		Exists: func(context.Context, string) (bool, error) {
			called = true
			return false, nil
		},
		Install: func(context.Context, string, []string) error {
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
				t.Fatalf("expected no installer callbacks for %q", guestPath)
			}
		})
	}
}

func TestInstallGuestFileDirectoryCreatesMissingAncestorsTopDown(t *testing.T) {
	existing := map[string]bool{
		"/var": true,
	}
	var checked []string
	var installed []struct {
		dir  string
		args []string
	}
	installer := GuestDirectoryInstaller{
		Exists: func(_ context.Context, guestDir string) (bool, error) {
			checked = append(checked, guestDir)
			return existing[guestDir], nil
		},
		Install: func(_ context.Context, guestDir string, args []string) error {
			installed = append(installed, struct {
				dir  string
				args []string
			}{dir: guestDir, args: args})
			return nil
		},
	}

	err := InstallGuestFileDirectory(context.Background(), installer, "/var/lib/virtle/config.json", "agent:users", "0640")
	if err != nil {
		t.Fatalf("install guest directory: %v", err)
	}
	if want := []string{"/var/lib/virtle", "/var/lib", "/var"}; !reflect.DeepEqual(checked, want) {
		t.Fatalf("checked dirs: got %#v want %#v", checked, want)
	}
	expectedInstalled := []struct {
		dir  string
		args []string
	}{
		{dir: "/var/lib", args: []string{"-d", "-o", "agent", "-g", "users", "-m", "0750", "/var/lib"}},
		{dir: "/var/lib/virtle", args: []string{"-d", "-o", "agent", "-g", "users", "-m", "0750", "/var/lib/virtle"}},
	}
	if !reflect.DeepEqual(installed, expectedInstalled) {
		t.Fatalf("installed dirs: got %#v want %#v", installed, expectedInstalled)
	}
}

func TestInstallGuestFileDirectoryPropagatesCallbackErrors(t *testing.T) {
	existsErr := errors.New("exists failed")
	err := InstallGuestFileDirectory(context.Background(), GuestDirectoryInstaller{
		Exists: func(context.Context, string) (bool, error) {
			return false, existsErr
		},
		Install: func(context.Context, string, []string) error {
			t.Fatalf("install should not be called after exists failure")
			return nil
		},
	}, "/etc/virtle/config.json", "", "")
	if !errors.Is(err, existsErr) {
		t.Fatalf("exists error: got %v want %v", err, existsErr)
	}

	installErr := errors.New("install failed")
	err = InstallGuestFileDirectory(context.Background(), GuestDirectoryInstaller{
		Exists: func(_ context.Context, guestDir string) (bool, error) {
			return guestDir == "/etc", nil
		},
		Install: func(context.Context, string, []string) error {
			return installErr
		},
	}, "/etc/virtle/config.json", "", "")
	if !errors.Is(err, installErr) {
		t.Fatalf("install error: got %v want %v", err, installErr)
	}
}

func TestWriteBackHostPathFollowsHostSymlinkWhenEnabled(t *testing.T) {
	tmpDir := t.TempDir()
	targetPath := filepath.Join(tmpDir, "target-file")
	if err := os.WriteFile(targetPath, []byte("original"), 0o644); err != nil {
		t.Fatalf("write target fixture: %v", err)
	}
	linkPath := filepath.Join(tmpDir, "host-link")
	if err := os.Symlink(targetPath, linkPath); err != nil {
		t.Fatalf("create symlink fixture: %v", err)
	}

	got, err := writeBackHostPath(manifest.ResolvedWriteFile{
		GuestPath:   "/var/lib/virtle/host",
		Content:     manifest.WriteFileContent{Kind: manifest.WriteFileContentPath, Path: linkPath},
		FollowLinks: true,
	})
	if err != nil {
		t.Fatalf("write-back host path: %v", err)
	}
	if got != targetPath {
		t.Fatalf("host path: got %q want %q", got, targetPath)
	}
}

func TestWriteHostFileAtomicPreservesExistingMode(t *testing.T) {
	tmpDir := t.TempDir()
	hostPath := filepath.Join(tmpDir, "file")
	if err := os.WriteFile(hostPath, []byte("old"), 0o600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	if err := WriteHostFileAtomic(hostPath, []byte("new")); err != nil {
		t.Fatalf("write atomic: %v", err)
	}
	data, err := os.ReadFile(hostPath)
	if err != nil {
		t.Fatalf("read host path: %v", err)
	}
	if got, want := string(data), "new"; got != want {
		t.Fatalf("data: got %q want %q", got, want)
	}
	info, err := os.Stat(hostPath)
	if err != nil {
		t.Fatalf("stat host path: %v", err)
	}
	if got, want := info.Mode().Perm(), os.FileMode(0o600); got != want {
		t.Fatalf("mode: got %s want %s", got, want)
	}
}
