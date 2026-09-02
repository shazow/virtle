package manifest

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/adrg/xdg"
	"github.com/shazow/virtle/units"
)

func writeFileText(text string) WriteFile {
	return WriteFile{Content: writeFileTextContent(text), FollowLinks: true}
}

func writeFilePath(path string) WriteFile {
	return WriteFile{Content: writeFilePathContent(path), FollowLinks: true}
}

func writeFileTextContent(text string) WriteFileContent {
	return WriteFileContent{Kind: WriteFileContentText, Text: text}
}

func writeFilePathContent(path string) WriteFileContent {
	return WriteFileContent{Kind: WriteFileContentPath, Path: path}
}

func TestLoadReadsFromReader(t *testing.T) {
	document := validDocument()
	document.QEMU.Exec = []string{"qemu-system-x86_64"}
	document.Kernel.Path = "boot/vmlinuz"
	document.Kernel.InitrdPath = "boot/initrd"
	document.Mounts = append(document.Mounts, NinePMountInput{
		MountInput: MountInput{
			SourcePath: "shares/cache",
			Tag:        "cache",
		},
		NineP: NinePInput{SecurityModel: "none"},
	})
	imageMount := document.Mounts[1].(ImageMountInput)
	imageMount.SourcePath = "images/root.img"
	document.Mounts[1] = imageMount
	virtioFSMount := document.Mounts[0].(VirtioFSMountInput)
	virtioFSMount.VirtioFS.Bin = "bin/virtiofsd-workspace"
	document.Mounts[0] = virtioFSMount

	data, err := json.Marshal(document)
	if err != nil {
		t.Fatalf("marshal manifest: %v", err)
	}

	loaded, err := loadBytes(data, "")
	if err != nil {
		t.Fatalf("load manifest: %v", err)
	}

	qemu, err := loaded.ResolvedQEMU()
	if err != nil {
		t.Fatalf("resolve qemu: %v", err)
	}
	if got, want := qemu.BinaryPath, "qemu-system-x86_64"; got != want {
		t.Fatalf("unexpected qemu binary path: got %q want %q", got, want)
	}
	if got, want := qemu.Kernel.Path, "/tmp/work/boot/vmlinuz"; got != want {
		t.Fatalf("unexpected kernel path: got %q want %q", got, want)
	}
	if got, want := qemu.Kernel.InitrdPath, "/tmp/work/boot/initrd"; got != want {
		t.Fatalf("unexpected initrd path: got %q want %q", got, want)
	}
	if got, want := qemu.Devices.Block[0].ImagePath, "/tmp/work/images/root.img"; got != want {
		t.Fatalf("unexpected block image path: got %q want %q", got, want)
	}
	if got, want := qemu.Devices.NineP[0].SourcePath, "/tmp/work/shares/cache"; got != want {
		t.Fatalf("unexpected 9p source path: got %q want %q", got, want)
	}

	runs, err := loaded.ResolvedRuns(3)
	if err != nil {
		t.Fatalf("resolve runs: %v", err)
	}
	if got, want := runs[0].Exec[0], "/tmp/work/bin/virtiofsd-workspace"; got != want {
		t.Fatalf("unexpected run command path: got %q want %q", got, want)
	}

	if got, want := loaded.ResolvedVolumes(), []Volume{
		{
			ImagePath:  "/tmp/work/images/root.img",
			Size:       256,
			FSType:     "ext4",
			AutoCreate: true,
		},
	}; !reflect.DeepEqual(got, want) {
		t.Fatalf("unexpected resolved volumes: got %#v want %#v", got, want)
	}
}

func TestLoadRejectsTrailingData(t *testing.T) {
	data, err := json.Marshal(validDocument())
	if err != nil {
		t.Fatalf("marshal manifest: %v", err)
	}

	_, err = loadBytes([]byte(string(data)+"\n{}"), "")
	if err == nil {
		t.Fatal("expected trailing data error")
	}
	if !strings.Contains(err.Error(), "unexpected trailing data") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestDocumentManagedVirtioFSDefaultBinUsesPATH(t *testing.T) {
	document := validDocument()
	mount := document.Mounts[0].(VirtioFSMountInput)
	mount.VirtioFS.Bin = ""
	mount.VirtioFS.Args = []string{"--socket-path={{.Socket}}"}
	document.Mounts[0] = mount

	manifest, err := document.Manifest()
	if err != nil {
		t.Fatalf("resolve manifest: %v", err)
	}
	if len(manifest.Run) != 1 {
		t.Fatalf("expected managed virtiofs run, got %#v", manifest.Run)
	}
	if got, want := manifest.Run[0].Exec[0], "virtiofsd"; got != want {
		t.Fatalf("unexpected virtiofs run path: got %q want %q", got, want)
	}
}

func TestKernelSerialModesResolveToQEMUConsole(t *testing.T) {
	tests := []struct {
		name                 string
		serial               string
		wantConsole          QEMUConsole
		wantResolutionErrMsg string
	}{
		{
			name:        "default off",
			wantConsole: QEMUConsoleOff,
		},
		{
			name:        "print",
			serial:      KernelSerialPrint,
			wantConsole: QEMUConsolePrint,
		},
		{
			name:        "console",
			serial:      KernelSerialConsole,
			wantConsole: QEMUConsoleInteractive,
		},
		{
			name:                 "invalid",
			serial:               "verbose",
			wantResolutionErrMsg: "manifest.kernel.serial must be one of off, print, or console",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			document := validDocument()
			document.Host = HostInput{
				OS:     "linux",
				Arch:   "x86_64",
				System: "x86_64-linux",
			}
			document.Kernel.Serial = tt.serial

			loaded, err := document.Manifest()
			if tt.wantResolutionErrMsg != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantResolutionErrMsg) {
					t.Fatalf("expected resolution error %q, got %v", tt.wantResolutionErrMsg, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("resolve manifest: %v", err)
			}

			qemu := loaded.QEMU
			if got := qemu.Console; got != tt.wantConsole {
				t.Fatalf("unexpected console mode: got %q want %q", got, tt.wantConsole)
			}
			hasKernelConsole := strings.Contains(qemu.Kernel.Params, "console=ttyS0")
			if hasKernelConsole != tt.wantConsole.Enabled() {
				t.Fatalf("unexpected kernel params: got %q want console=%v", qemu.Kernel.Params, tt.wantConsole.Enabled())
			}
		})
	}
}

func TestDocumentWriteFilesFollowLinksResolvesToManifest(t *testing.T) {
	followLinks := false
	writeBack := true
	document := validDocument()
	document.WriteFiles = []WriteFileInput{
		{
			GuestPath:   "/etc/source.conf",
			Path:        stringPtr("source.conf"),
			FollowLinks: &followLinks,
			WriteBack:   &writeBack,
		},
	}

	data, err := json.Marshal(document)
	if err != nil {
		t.Fatalf("marshal manifest: %v", err)
	}

	loaded, err := loadBytes(data, "")
	if err != nil {
		t.Fatalf("load manifest: %v", err)
	}

	files := loaded.ResolvedWriteFiles()
	if len(files) != 1 {
		t.Fatalf("expected one write file, got %#v", files)
	}
	if got := files[0].FollowLinks; got != false {
		t.Fatalf("unexpected followLinks default: got %v want false", got)
	}
	if got := files[0].WriteBack; got != true {
		t.Fatalf("unexpected writeBack value: got %v want true", got)
	}
}

func TestDocumentWriteFilesRejectsTextAndSource(t *testing.T) {
	writeBack := true
	document := validDocument()
	document.WriteFiles = []WriteFileInput{
		{
			GuestPath: "/etc/source.conf",
			Text:      stringPtr("inline"),
			Path:      stringPtr("source.conf"),
			WriteBack: &writeBack,
		},
	}

	data, err := json.Marshal(document)
	if err != nil {
		t.Fatalf("marshal manifest: %v", err)
	}

	_, err = loadBytes(data, "")
	if err == nil || !strings.Contains(err.Error(), `manifest.writeFiles["/etc/source.conf"] must set exactly one of text or path`) {
		t.Fatalf("expected exactly-one write_files validation error, got %v", err)
	}
}

func TestDocumentRunResolvesAndResolvesCommand(t *testing.T) {
	t.Setenv("DBUS_SESSION_BUS_ADDRESS", "unix:path=/run/user/1000/bus")

	document := validDocument()
	document.Workspace = WorkspaceInput{
		GuestDir: "/home/agent/workspace",
		HostDir:  "/tmp/work/.virtle/workspace",
	}
	document.Mounts = MountsInput{}
	document.Run = []RunInput{
		{
			Exec: []string{
				"xdg-dbus-proxy",
				"{{.Env.DBUS_SESSION_BUS_ADDRESS}}",
				"{{.Workspace.HostPath}}/dbus-notifications.sock",
				"--name={{.Name}}",
				"--workspace={{.Workspace.GuestPath}}",
				"--cid={{.CID}}",
			},
			Vars: map[string]any{
				"Name": "notifications",
				"Config": map[string]any{
					"workspace": map[string]any{
						"hostDir": "/tmp/work/.virtle/workspace",
					},
				},
			},
		},
	}

	loaded, err := document.Manifest()
	if err != nil {
		t.Fatalf("resolve manifest: %v", err)
	}

	runs, err := loaded.ResolvedRuns(7)
	if err != nil {
		t.Fatalf("resolve run: %v", err)
	}
	if len(runs) != 1 {
		t.Fatalf("expected one run, got %#v", runs)
	}
	if len(loaded.CleanupFiles) != 0 {
		t.Fatalf("expected generic run not to add cleanup entries, got %#v", loaded.CleanupFiles)
	}
	runJSON, err := json.Marshal(loaded.Run[0])
	if err != nil {
		t.Fatalf("marshal run: %v", err)
	}
	if strings.Contains(string(runJSON), "socketPath") || strings.Contains(string(runJSON), "cleanup") {
		t.Fatalf("generic run should not carry socket or cleanup fields: %s", runJSON)
	}
	wantExec := []string{
		"xdg-dbus-proxy",
		"unix:path=/run/user/1000/bus",
		"/tmp/work/.virtle/workspace/dbus-notifications.sock",
		"--name=notifications",
		"--workspace=/home/agent/workspace",
		"--cid=7",
	}
	if got := runs[0].Exec; !reflect.DeepEqual(got, wantExec) {
		t.Fatalf("unexpected run exec: got %#v want %#v", got, wantExec)
	}
	if got, want := runs[0].Dir, "/tmp/work"; got != want {
		t.Fatalf("unexpected run dir: got %q want %q", got, want)
	}
	for _, want := range []string{
		"CID=7",
		"NAME=notifications",
	} {
		if !slices.Contains(runs[0].Env, want) {
			t.Fatalf("expected run env %q in %#v", want, runs[0].Env)
		}
	}
	if slices.ContainsFunc(runs[0].Env, func(entry string) bool {
		return strings.HasPrefix(entry, "WORKSPACE=")
	}) {
		t.Fatalf("structured workspace should not produce scalar env in %#v", runs[0].Env)
	}
	if slices.ContainsFunc(runs[0].Env, func(entry string) bool {
		return strings.HasPrefix(entry, "CONFIG=")
	}) {
		t.Fatalf("structured vars should not be injected into env: %#v", runs[0].Env)
	}
}

func TestDocumentRunValidation(t *testing.T) {
	for _, tt := range []struct {
		name    string
		run     RunInput
		wantErr string
	}{
		{
			name: "reserved var",
			run: RunInput{
				Exec: []string{"proxy"},
				Vars: map[string]any{"Workspace": "override"},
			},
			wantErr: `vars key "Workspace" is reserved`,
		},
		{
			name: "bare workspace template",
			run: RunInput{
				Exec: []string{"proxy", "{{.Workspace}}"},
			},
			wantErr: `uses {{.Workspace}}; use {{.Workspace.GuestPath}} or {{.Workspace.HostPath}}`,
		},
		{
			name:    "missing exec",
			run:     RunInput{},
			wantErr: "exec is required",
		},
		{
			name: "empty exec path",
			run: RunInput{
				Exec: []string{""},
			},
			wantErr: "exec[0] is required",
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			document := validDocument()
			document.Run = []RunInput{tt.run}
			_, err := document.Manifest()
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("expected %q error, got %v", tt.wantErr, err)
			}
		})
	}
}

func TestDocumentVirtioFSUsesExistingSocketWithoutRun(t *testing.T) {
	tmpDir := t.TempDir()
	socketPath := filepath.Join(tmpDir, ".virtle", "fs.sock")
	if err := os.MkdirAll(filepath.Dir(socketPath), 0o755); err != nil {
		t.Fatalf("create socket directory: %v", err)
	}
	listener, err := net.Listen("unix", socketPath)
	if err != nil {
		t.Fatalf("listen on virtiofs socket: %v", err)
	}
	defer listener.Close()

	document := validDocument()
	document.WorkingDir = tmpDir

	var logOutput bytes.Buffer
	manifest, err := document.ManifestWithOptions(ResolveOptions{Logger: slog.New(slog.NewTextHandler(&logOutput, nil))})
	if err != nil {
		t.Fatalf("resolve manifest: %v", err)
	}

	if len(manifest.Run) != 0 {
		t.Fatalf("expected existing virtiofs socket to suppress generated run, got %#v", manifest.Run)
	}
	if len(manifest.CleanupFiles) != 0 {
		t.Fatalf("expected external virtiofs socket not to add cleanup entry, got %#v", manifest.CleanupFiles)
	}
	if !strings.Contains(logOutput.String(), "using existing virtiofs socket") || !strings.Contains(logOutput.String(), socketPath) {
		t.Fatalf("expected existing socket log for %q, got %q", socketPath, logOutput.String())
	}
}

func TestDocumentVirtioFSStartsRunForStaleSocket(t *testing.T) {
	tmpDir := t.TempDir()
	socketPath := filepath.Join(tmpDir, ".virtle", "fs.sock")
	if err := os.MkdirAll(filepath.Dir(socketPath), 0o755); err != nil {
		t.Fatalf("create socket directory: %v", err)
	}
	fd, err := syscall.Socket(syscall.AF_UNIX, syscall.SOCK_STREAM, 0)
	if err != nil {
		t.Fatalf("create unix socket: %v", err)
	}
	if err := syscall.Bind(fd, &syscall.SockaddrUnix{Name: socketPath}); err != nil {
		_ = syscall.Close(fd)
		t.Fatalf("bind stale virtiofs socket: %v", err)
	}
	if err := syscall.Close(fd); err != nil {
		t.Fatalf("close stale unix socket: %v", err)
	}
	if info, err := os.Stat(socketPath); err != nil {
		t.Fatalf("stat stale virtiofs socket: %v", err)
	} else if info.Mode()&os.ModeSocket == 0 {
		t.Fatalf("expected stale path to remain a socket, got mode %v", info.Mode())
	}

	document := validDocument()
	document.WorkingDir = tmpDir

	var logOutput bytes.Buffer
	manifest, err := document.ManifestWithOptions(ResolveOptions{Logger: slog.New(slog.NewTextHandler(&logOutput, nil))})
	if err != nil {
		t.Fatalf("resolve manifest: %v", err)
	}

	if len(manifest.Run) != 1 || filepath.Base(manifest.Run[0].Exec[0]) != "virtiofsd-workspace" {
		t.Fatalf("expected generated virtiofs run, got %#v", manifest.Run)
	}
	if got, want := manifest.CleanupFiles, []string{"fs.sock"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("unexpected cleanup entries: got %#v want %#v", got, want)
	}
	if !strings.Contains(logOutput.String(), "appears stale") || !strings.Contains(logOutput.String(), socketPath) {
		t.Fatalf("expected stale socket warning for %q, got %q", socketPath, logOutput.String())
	}
}

func TestDocumentVirtioFSWarnsAndGeneratesRunForExistingNonSocket(t *testing.T) {
	tmpDir := t.TempDir()
	socketPath := filepath.Join(tmpDir, ".virtle", "fs.sock")
	if err := os.MkdirAll(filepath.Dir(socketPath), 0o755); err != nil {
		t.Fatalf("create socket directory: %v", err)
	}
	if err := os.WriteFile(socketPath, []byte("stale"), 0o600); err != nil {
		t.Fatalf("write stale socket path: %v", err)
	}

	document := validDocument()
	document.WorkingDir = tmpDir

	var logOutput bytes.Buffer
	manifest, err := document.ManifestWithOptions(ResolveOptions{Logger: slog.New(slog.NewTextHandler(&logOutput, nil))})
	if err != nil {
		t.Fatalf("resolve manifest: %v", err)
	}

	if len(manifest.Run) != 1 || filepath.Base(manifest.Run[0].Exec[0]) != "virtiofsd-workspace" {
		t.Fatalf("expected generated virtiofs run, got %#v", manifest.Run)
	}
	if got, want := manifest.CleanupFiles, []string{"fs.sock"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("unexpected cleanup entries: got %#v want %#v", got, want)
	}
	if !strings.Contains(logOutput.String(), "not a socket") || !strings.Contains(logOutput.String(), socketPath) {
		t.Fatalf("expected non-socket warning for %q, got %q", socketPath, logOutput.String())
	}
}

func TestDocumentManagedVirtioFSAddsCleanupFile(t *testing.T) {
	tmpDir := t.TempDir()
	document := validDocument()
	document.WorkingDir = tmpDir

	manifest, err := document.Manifest()
	if err != nil {
		t.Fatalf("resolve manifest: %v", err)
	}

	if got, want := manifest.CleanupFiles, []string{"fs.sock"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("unexpected cleanup entries: got %#v want %#v", got, want)
	}

	paths, err := manifest.ResolvedCleanupFiles()
	if err != nil {
		t.Fatalf("resolve cleanup files: %v", err)
	}
	if got, want := paths, []string{filepath.Join(tmpDir, ".virtle", "fs.sock")}; !reflect.DeepEqual(got, want) {
		t.Fatalf("unexpected resolved cleanup files: got %#v want %#v", got, want)
	}
}

func TestDocumentExternalVirtioFSSocketIsNotAutoRemovedOnShutdown(t *testing.T) {
	tmpDir := t.TempDir()
	socketPath := filepath.Join(tmpDir, ".virtle", "fs.sock")
	if err := os.MkdirAll(filepath.Dir(socketPath), 0o755); err != nil {
		t.Fatalf("create socket directory: %v", err)
	}
	listener, err := net.Listen("unix", socketPath)
	if err != nil {
		t.Fatalf("listen on virtiofs socket: %v", err)
	}
	defer listener.Close()

	document := validDocument()
	document.WorkingDir = tmpDir

	manifest, err := document.Manifest()
	if err != nil {
		t.Fatalf("resolve manifest: %v", err)
	}
	if len(manifest.CleanupFiles) != 0 {
		t.Fatalf("expected external virtiofs socket not to add cleanup entry, got %#v", manifest.CleanupFiles)
	}
}

func TestLoadTOMLExamples(t *testing.T) {
	for _, path := range []string{
		"../../examples/manifest-simple.toml",
		"../../examples/manifest-full.toml",
	} {
		t.Run(filepath.Base(path), func(t *testing.T) {
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("read example: %v", err)
			}
			if _, err := loadBytes(data, path); err != nil {
				t.Fatalf("load example: %v", err)
			}
		})
	}
}

func TestDecodeDocumentOrderedTaggedMounts(t *testing.T) {
	data := []byte(`
[kernel]
path = "/tmp/vmlinuz"
initrd_path = "/tmp/initrd"

[[mounts]]
type = "9p"
tag = "cache"
source = ".cache"
9p.security_model = "none"

[[mounts]]
type = "virtiofs"
tag = "workspace"
source = "."
virtiofs.socket = "workspace.sock"

[[mounts]]
type = "image"
source = "root.img"
image.size = 256
image.fs = "ext4"
image.create = true
`)
	document, err := DecodeDocumentBytes(data, "manifest.toml")
	if err != nil {
		t.Fatalf("decode manifest: %v", err)
	}
	if len(document.Mounts) != 3 {
		t.Fatalf("unexpected mount count: got %d want 3", len(document.Mounts))
	}
	if _, ok := document.Mounts[0].(NinePMountInput); !ok {
		t.Fatalf("expected first mount to be 9p, got %T", document.Mounts[0])
	}
	if _, ok := document.Mounts[1].(VirtioFSMountInput); !ok {
		t.Fatalf("expected second mount to be virtiofs, got %T", document.Mounts[1])
	}
	if _, ok := document.Mounts[2].(ImageMountInput); !ok {
		t.Fatalf("expected third mount to be image, got %T", document.Mounts[2])
	}

	manifest, err := document.Manifest()
	if err != nil {
		t.Fatalf("resolve manifest: %v", err)
	}
	if got, want := len(manifest.QEMU.Devices.Mounts), 3; got != want {
		t.Fatalf("unexpected resolved mount count: got %d want %d", got, want)
	}
	for i, want := range []string{MountTypeNineP, MountTypeVirtioFS, MountTypeImage} {
		if got := manifest.QEMU.Devices.Mounts[i].Type; got != want {
			t.Fatalf("unexpected resolved mount type at %d: got %q want %q", i, got, want)
		}
	}
}

func TestDecodeDocumentRejectsInvalidTaggedMounts(t *testing.T) {
	for _, tt := range []struct {
		name    string
		data    string
		file    string
		wantErr string
	}{
		{
			name:    "missing type",
			file:    "manifest.json",
			data:    `{"kernel":{"path":"/tmp/vmlinuz","initrd_path":"/tmp/initrd"},"mounts":[{"tag":"workspace"}]}`,
			wantErr: "manifest.mounts[0].type must be one of virtiofs, 9p, or image",
		},
		{
			name:    "unknown type",
			file:    "manifest.json",
			data:    `{"kernel":{"path":"/tmp/vmlinuz","initrd_path":"/tmp/initrd"},"mounts":[{"type":"sshfs","tag":"workspace"}]}`,
			wantErr: "manifest.mounts[0].type must be one of virtiofs, 9p, or image",
		},
		{
			name:    "unknown backend field",
			file:    "manifest.json",
			data:    `{"kernel":{"path":"/tmp/vmlinuz","initrd_path":"/tmp/initrd"},"mounts":[{"type":"virtiofs","tag":"workspace","virtiofs":{"socket":"fs.sock"},"extra":true}]}`,
			wantErr: "unknown field",
		},
		{
			name:    "top-level image format",
			file:    "manifest.json",
			data:    `{"kernel":{"path":"/tmp/vmlinuz","initrd_path":"/tmp/initrd"},"mounts":[{"type":"image","source":"data.qcow2","format":"qcow2","image":{"serial":"data"}}]}`,
			wantErr: "unknown field",
		},
		{
			name: "grouped toml mounts",
			file: "manifest.toml",
			data: `[kernel]
path = "/tmp/vmlinuz"
initrd_path = "/tmp/initrd"

[[mounts.virtiofs]]
tag = "workspace"
virtiofs.socket = "fs.sock"
`,
			wantErr: "manifest.mounts must be an array of tables",
		},
		{
			name: "non-table toml mount",
			file: "manifest.toml",
			data: `mounts = ["bad"]

[kernel]
path = "/tmp/vmlinuz"
initrd_path = "/tmp/initrd"
`,
			wantErr: "manifest.mounts[0] must be a table",
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			_, err := DecodeDocumentBytes([]byte(tt.data), tt.file)
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("expected %q error, got %v", tt.wantErr, err)
			}
		})
	}
}

func TestDecodeDocumentAllowsEmptyTOMLMounts(t *testing.T) {
	data := []byte(`mounts = []

[kernel]
path = "/tmp/vmlinuz"
initrd_path = "/tmp/initrd"
`)
	document, err := DecodeDocumentBytes(data, "manifest.toml")
	if err != nil {
		t.Fatalf("decode manifest: %v", err)
	}
	if got := len(document.Mounts); got != 0 {
		t.Fatalf("unexpected mount count: got %d want 0", got)
	}
	manifest, err := document.Manifest()
	if err != nil {
		t.Fatalf("resolve manifest: %v", err)
	}
	if got := len(manifest.QEMU.Devices.Mounts); got != 0 {
		t.Fatalf("unexpected resolved mount count: got %d want 0", got)
	}
}

func TestDecodeDocumentBareNumberDurationsMeanSeconds(t *testing.T) {
	bare, err := DecodeDocumentBytes([]byte("[ssh]\nretry_delay = 5\n[qemu]\nguest_default_timeout = 2.5\n"), "manifest.toml")
	if err != nil {
		t.Fatalf("decode bare number durations: %v", err)
	}
	strings, err := DecodeDocumentBytes([]byte("[ssh]\nretry_delay = \"5s\"\n[qemu]\nguest_default_timeout = \"2.5s\"\n"), "manifest.toml")
	if err != nil {
		t.Fatalf("decode duration strings: %v", err)
	}
	if bare.SSH.RetryDelay != strings.SSH.RetryDelay || bare.SSH.RetryDelay != units.Duration(5*time.Second) {
		t.Fatalf("retry delay: bare %s string %s want 5s", bare.SSH.RetryDelay, strings.SSH.RetryDelay)
	}
	if bare.QEMU.GuestDefaultTimeout != strings.QEMU.GuestDefaultTimeout || bare.QEMU.GuestDefaultTimeout != units.Duration(2500*time.Millisecond) {
		t.Fatalf("guest default timeout: bare %s string %s want 2.5s", bare.QEMU.GuestDefaultTimeout, strings.QEMU.GuestDefaultTimeout)
	}
}

func TestManifestSSHRetryDelayDefaultsAndValidation(t *testing.T) {
	decoded, err := DecodeDocumentBytes([]byte("[ssh]\n"), "manifest.toml")
	if err != nil {
		t.Fatalf("decode omitted ssh retry delay: %v", err)
	}
	if got, want := decoded.SSH.RetryDelay, units.Duration(500*time.Millisecond); got != want {
		t.Fatalf("omitted ssh retry delay: got %v want %v", got, want)
	}

	document := validDocument()
	document.SSH.RetryDelay = units.Duration(500 * time.Millisecond)
	manifest, err := document.Manifest()
	if err != nil {
		t.Fatalf("resolve manifest: %v", err)
	}
	if got, want := manifest.SSH.RetryDelay, 500*time.Millisecond; got != want {
		t.Fatalf("unexpected default ssh retry delay: got %s want %s", got, want)
	}
	if got, want := manifest.QEMU.SSHReady.SocketPath, ""; got != want {
		t.Fatalf("unexpected default ssh readiness socket: got %q want %q", got, want)
	}
	emptySSH := validManifest()
	emptySSH.SSH.Argv = nil
	if err := emptySSH.Validate(); err != nil {
		t.Fatalf("validate manifest with omitted ssh argv: %v", err)
	}
	if len(emptySSH.SSH.Argv) != 0 {
		t.Fatalf("expected omitted ssh argv to remain empty, got %#v", emptySSH.SSH.Argv)
	}
	if !manifest.QEMU.NoGraphicEnabled() {
		t.Fatalf("expected manifest without graphics to default to noGraphic")
	}

	customDoc := validDocument()
	customDoc.SSH.RetryDelay = units.Duration(250 * time.Millisecond)
	custom, err := customDoc.Manifest()
	if err != nil {
		t.Fatalf("resolve custom retry delay: %v", err)
	}
	if got, want := custom.SSH.RetryDelay, 250*time.Millisecond; got != want {
		t.Fatalf("unexpected custom ssh retry delay: got %s want %s", got, want)
	}

	zeroDoc := validDocument()
	zeroDoc.SSH.RetryDelay = 0
	if _, err := zeroDoc.Manifest(); err == nil || !strings.Contains(err.Error(), "manifest.ssh.retryDelay must be greater than zero") {
		t.Fatalf("expected zero retry delay error, got %v", err)
	}

	invalid := validDocument()
	invalid.SSH.RetryDelay = units.Duration(-time.Second)
	_, err = invalid.Manifest()
	if err == nil || !strings.Contains(err.Error(), "manifest.ssh.retryDelay must be greater than zero") {
		t.Fatalf("expected retry delay validation error, got %v", err)
	}
}

func TestDecodeDocumentGuestDefaultTimeout(t *testing.T) {
	omitted, err := DecodeDocumentBytes([]byte("[qemu]\n"), "manifest.toml")
	if err != nil {
		t.Fatalf("decode omitted guest default timeout: %v", err)
	}
	if got, want := omitted.QEMU.GuestDefaultTimeout, units.Duration(30*time.Second); got != want {
		t.Fatalf("omitted guest default timeout: got %v want %v", got, want)
	}

	zero, err := DecodeDocumentBytes([]byte("[qemu]\nguest_default_timeout = 0\n"), "manifest.toml")
	if err != nil {
		t.Fatalf("decode zero guest default timeout: %v", err)
	}
	if got := zero.QEMU.GuestDefaultTimeout; got != 0 {
		t.Fatalf("zero guest default timeout: got %v want 0", got)
	}

	custom, err := DecodeDocumentBytes([]byte(`{"qemu": {"guest_default_timeout": 2.5}}`), "manifest.json")
	if err != nil {
		t.Fatalf("decode custom guest default timeout: %v", err)
	}
	if got, want := custom.QEMU.GuestDefaultTimeout, units.Duration(2500*time.Millisecond); got != want {
		t.Fatalf("custom guest default timeout: got %v want %v", got, want)
	}
}

func TestManifestGuestDefaultTimeoutResolutionAndValidation(t *testing.T) {
	document := validDocument()
	document.QEMU.GuestDefaultTimeout = units.Duration(2500 * time.Millisecond)
	resolved, err := document.Manifest()
	if err != nil {
		t.Fatalf("resolve custom guest agent timeout: %v", err)
	}
	if got, want := resolved.QEMU.GuestAgent.CommandTimeout, 2500*time.Millisecond; got != want {
		t.Fatalf("custom guest agent timeout: got %s want %s", got, want)
	}

	zero := validDocument()
	zero.QEMU.GuestDefaultTimeout = 0
	resolved, err = zero.Manifest()
	if err != nil {
		t.Fatalf("resolve zero guest agent timeout: %v", err)
	}
	if got := resolved.QEMU.GuestAgent.CommandTimeout; got != 0 {
		t.Fatalf("zero guest agent timeout: got %s want no timeout", got)
	}

	invalid := validDocument()
	invalid.QEMU.GuestDefaultTimeout = units.Duration(-time.Second)
	_, err = invalid.Manifest()
	if err == nil || !strings.Contains(err.Error(), "manifest.qemu.guestAgent.commandTimeout must be greater than or equal to zero") {
		t.Fatalf("expected guest agent timeout validation error, got %v", err)
	}
}

func TestManifestGuestShutdownConfiguration(t *testing.T) {
	omitted, err := DecodeDocumentBytes([]byte("[qemu]\n"), "manifest.toml")
	if err != nil {
		t.Fatalf("decode omitted shutdown timeout: %v", err)
	}
	if got, want := omitted.QEMU.ShutdownTimeout, units.Duration(90*time.Second); got != want {
		t.Fatalf("omitted shutdown timeout: got %v want %v", got, want)
	}

	document := validDocument()
	document.QEMU.ShutdownExec = []string{"/bin/sh", "-c", "poweroff"}
	document.QEMU.ShutdownTimeout = units.Duration(2500 * time.Millisecond)
	resolved, err := document.Manifest()
	if err != nil {
		t.Fatalf("resolve manifest: %v", err)
	}
	if got, want := resolved.QEMU.GuestAgent.ShutdownExec, document.QEMU.ShutdownExec; !slices.Equal(got, want) {
		t.Fatalf("shutdown exec: got %v want %v", got, want)
	}
	if got, want := resolved.QEMU.GuestAgent.ShutdownTimeout, 2500*time.Millisecond; got != want {
		t.Fatalf("custom shutdown timeout: got %s want %s", got, want)
	}
}

func TestDocumentSSHReadySocketDefaultAndEnable(t *testing.T) {
	omitted := validDocument()
	omitted.SSH.ReadySocket = ""
	omittedManifest, err := omitted.Manifest()
	if err != nil {
		t.Fatalf("resolve manifest with omitted readiness socket: %v", err)
	}
	if got, want := omittedManifest.QEMU.SSHReady.SocketPath, ""; got != want {
		t.Fatalf("unexpected default ssh readiness socket: got %q want %q", got, want)
	}

	enabled := validDocument()
	enabled.SSH.ReadySocket = "ssh-ready.sock"
	enabledManifest, err := enabled.Manifest()
	if err != nil {
		t.Fatalf("resolve manifest with enabled readiness socket: %v", err)
	}
	if got, want := enabledManifest.QEMU.SSHReady.SocketPath, "ssh-ready.sock"; got != want {
		t.Fatalf("unexpected enabled ssh readiness socket: got %q want %q", got, want)
	}
	sshReadySocketPath, err := omittedManifest.ResolvedSSHReadySocketPath()
	if err != nil {
		t.Fatalf("resolve default ssh readiness socket: %v", err)
	}
	if sshReadySocketPath != "" {
		t.Fatalf("expected default ssh readiness socket to resolve to empty path, got %q", sshReadySocketPath)
	}
}

func TestDocumentSSHExecDefaultsToSSH(t *testing.T) {
	document := validDocument()
	document.SSH.Exec = nil

	manifest, err := document.Manifest()
	if err != nil {
		t.Fatalf("resolve manifest with omitted ssh exec: %v", err)
	}
	if got, want := manifest.SSH.Argv, []string{"ssh"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("ssh exec = %#v, want %#v", got, want)
	}
}

func TestDocumentSSHExecPreservesExplicitEmpty(t *testing.T) {
	document := validDocument()
	document.SSH.Exec = []string{}

	manifest, err := document.Manifest()
	if err != nil {
		t.Fatalf("resolve manifest with explicitly empty ssh exec: %v", err)
	}
	if len(manifest.SSH.Argv) != 0 {
		t.Fatalf("expected explicitly empty ssh exec to stay empty, got %#v", manifest.SSH.Argv)
	}
}

func TestDocumentQEMUExecRendersTemplates(t *testing.T) {
	t.Setenv("USER", "template-user")

	document := validDocument()
	document.Host = HostInput{
		OS:     "linux",
		Arch:   "x86_64",
		System: "x86_64-linux",
	}
	document.QEMU.Exec = []string{
		"/nix/store/qemu-{{.HostArch}}",
		"-name={{.HostName}}",
		"-sandbox={{.WorkingDir}}",
		"-state={{.StateDir}}",
		"-host={{.HostOS}}/{{.HostSystem}}",
		"-user={{.Env.USER}}",
	}

	manifest, err := document.Manifest()
	if err != nil {
		t.Fatalf("resolve manifest with qemu exec templates: %v", err)
	}
	if got, want := manifest.QEMU.BinaryPath, "/nix/store/qemu-x86_64"; got != want {
		t.Fatalf("unexpected qemu binary path: got %q want %q", got, want)
	}
	wantArgs := []string{
		"-name=agent-sandbox",
		"-sandbox=/tmp/work",
		"-state=.virtle",
		"-host=linux/x86_64-linux",
		"-user=template-user",
	}
	if got := manifest.QEMU.PassthroughArgs; !reflect.DeepEqual(got, wantArgs) {
		t.Fatalf("unexpected qemu passthrough args: got %#v want %#v", got, wantArgs)
	}
}

func TestDocumentQEMUUserIsRetainedForPersistenceAccess(t *testing.T) {
	document := validDocument()
	document.QEMU.User = "qemu-runtime"

	resolved, err := document.Manifest()
	if err != nil {
		t.Fatalf("resolve manifest: %v", err)
	}
	if got, want := resolved.QEMU.RunAsUser, document.QEMU.User; got != want {
		t.Fatalf("qemu run-as user: got %q want %q", got, want)
	}
	if got := resolved.QEMU.PassthroughArgs; !reflect.DeepEqual(got[:2], []string{"-user", "qemu-runtime"}) {
		t.Fatalf("qemu user arguments: got %#v", got)
	}
}

func TestDocumentQEMUExecRejectsMissingTemplateKey(t *testing.T) {
	document := validDocument()
	document.QEMU.Exec = []string{"qemu-system-{{.Missing}}"}

	_, err := document.Manifest()
	if err == nil {
		t.Fatal("expected qemu exec template error")
	}
	for _, want := range []string{
		"manifest.qemu.exec: exec[0]",
		`map has no entry for key "Missing"`,
	} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("expected error containing %q, got %v", want, err)
		}
	}
}

func TestDocumentSSHAutoprovisionResolvesToManifest(t *testing.T) {
	document := validDocument()
	document.SSH.Autoprovision = true

	manifest, err := document.Manifest()
	if err != nil {
		t.Fatalf("resolve manifest with ssh autoprovision: %v", err)
	}
	if !manifest.SSH.Autoprovision {
		t.Fatal("expected ssh autoprovision to resolve to manifest")
	}
}

func TestManifestResolvesSocketsFromRuntimeDir(t *testing.T) {
	runtimeDir := t.TempDir()
	setXDGTestRuntimeDir(t, runtimeDir)

	tests := []struct {
		name       string
		runtimeDir RuntimeDir
		socketPath string
		wantSocket string
		wantQMP    string
		wantQGA    string
		wantReady  string
	}{
		{
			name:       "legacy working dir",
			runtimeDir: RuntimeDir{},
			socketPath: "fs.sock",
			wantSocket: "/tmp/work/fs.sock",
			wantQMP:    "/tmp/work/qmp.sock",
			wantQGA:    "/tmp/work/qga.sock",
			wantReady:  "/tmp/work/ssh-ready.sock",
		},
		{
			name:       "default runtime dir",
			runtimeDir: RuntimeDir{Mode: RuntimeDirXDG},
			socketPath: "fs.sock",
			wantSocket: filepath.Join(runtimeDir, "agentspace", "agent-sandbox", "fs.sock"),
			wantQMP:    filepath.Join(runtimeDir, "agentspace", "agent-sandbox", "qmp.sock"),
			wantQGA:    filepath.Join(runtimeDir, "agentspace", "agent-sandbox", "qga.sock"),
			wantReady:  filepath.Join(runtimeDir, "agentspace", "agent-sandbox", "ssh-ready.sock"),
		},
		{
			name:       "relative runtime dir",
			runtimeDir: RuntimeDir{Mode: RuntimeDirPath, Path: "runtime"},
			socketPath: "fs.sock",
			wantSocket: "/tmp/work/runtime/fs.sock",
			wantQMP:    "/tmp/work/runtime/qmp.sock",
			wantQGA:    "/tmp/work/runtime/qga.sock",
			wantReady:  "/tmp/work/runtime/ssh-ready.sock",
		},
		{
			name:       "absolute runtime dir",
			runtimeDir: RuntimeDir{Mode: RuntimeDirPath, Path: "/tmp/runtime"},
			socketPath: "fs.sock",
			wantSocket: "/tmp/runtime/fs.sock",
			wantQMP:    "/tmp/runtime/qmp.sock",
			wantQGA:    "/tmp/runtime/qga.sock",
			wantReady:  "/tmp/runtime/ssh-ready.sock",
		},
		{
			name:       "absolute socket path bypasses runtime dir",
			runtimeDir: RuntimeDir{Mode: RuntimeDirXDG},
			socketPath: "/tmp/explicit-fs.sock",
			wantSocket: "/tmp/explicit-fs.sock",
			wantQMP:    "/tmp/explicit-qmp.sock",
			wantQGA:    "/tmp/explicit-qga.sock",
			wantReady:  "/tmp/explicit-ready.sock",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			manifest := validManifest()
			manifest.Paths.RuntimeDir = tt.runtimeDir
			manifest.CleanupFiles = []string{tt.socketPath}
			manifest.QEMU.Devices.VirtioFS[0].SocketPath = tt.socketPath
			manifest.QEMU.GuestAgent.SocketPath = "qga.sock"
			manifest.QEMU.SSHReady.SocketPath = "ssh-ready.sock"
			if tt.name == "absolute socket path bypasses runtime dir" {
				manifest.QEMU.QMP.SocketPath = "/tmp/explicit-qmp.sock"
				manifest.QEMU.GuestAgent.SocketPath = "/tmp/explicit-qga.sock"
				manifest.QEMU.SSHReady.SocketPath = "/tmp/explicit-ready.sock"
			}

			cleanupFiles, err := manifest.ResolvedCleanupFiles()
			if err != nil {
				t.Fatalf("resolve cleanup files: %v", err)
			}
			if got, want := cleanupFiles, []string{tt.wantSocket}; !reflect.DeepEqual(got, want) {
				t.Fatalf("unexpected cleanup files: got %v want %v", got, want)
			}
			virtioFSSocketPaths, err := manifest.ResolvedVirtioFSSocketPaths()
			if err != nil {
				t.Fatalf("resolve virtiofs socket paths: %v", err)
			}
			if got, want := virtioFSSocketPaths, []string{tt.wantSocket}; !reflect.DeepEqual(got, want) {
				t.Fatalf("unexpected virtiofs socket paths: got %v want %v", got, want)
			}

			qmpSocketPath, err := manifest.ResolvedQMPSocketPath()
			if err != nil {
				t.Fatalf("resolve qmp socket path: %v", err)
			}
			if qmpSocketPath != tt.wantQMP {
				t.Fatalf("unexpected qmp socket path: got %q want %q", qmpSocketPath, tt.wantQMP)
			}
			guestAgentSocketPath, err := manifest.ResolvedGuestAgentSocketPath()
			if err != nil {
				t.Fatalf("resolve guest agent socket path: %v", err)
			}
			if guestAgentSocketPath != tt.wantQGA {
				t.Fatalf("unexpected guest agent socket path: got %q want %q", guestAgentSocketPath, tt.wantQGA)
			}
			sshReadySocketPath, err := manifest.ResolvedSSHReadySocketPath()
			if err != nil {
				t.Fatalf("resolve ssh readiness socket path: %v", err)
			}
			if sshReadySocketPath != tt.wantReady {
				t.Fatalf("unexpected ssh readiness socket path: got %q want %q", sshReadySocketPath, tt.wantReady)
			}

			qemu, err := manifest.ResolvedQEMU()
			if err != nil {
				t.Fatalf("resolve qemu config: %v", err)
			}
			if got, want := qemu.Devices.VirtioFS[0].SocketPath, tt.wantSocket; got != want {
				t.Fatalf("unexpected qemu virtiofs socket path: got %q want %q", got, want)
			}
			if got, want := qemu.QMP.SocketPath, tt.wantQMP; got != want {
				t.Fatalf("unexpected qemu qmp socket path: got %q want %q", got, want)
			}
			if got, want := qemu.GuestAgent.SocketPath, tt.wantQGA; got != want {
				t.Fatalf("unexpected qemu guest agent socket path: got %q want %q", got, want)
			}
			if got, want := qemu.SSHReady.SocketPath, tt.wantReady; got != want {
				t.Fatalf("unexpected qemu ssh readiness socket path: got %q want %q", got, want)
			}

			resolvedCleanupFiles, err := manifest.ResolvedCleanupFiles()
			if err != nil {
				t.Fatalf("resolve cleanup files: %v", err)
			}
			if got, want := resolvedCleanupFiles, []string{tt.wantSocket}; !reflect.DeepEqual(got, want) {
				t.Fatalf("unexpected cleanup files: got %v want %v", got, want)
			}
		})
	}
}

func TestManifestWriteFilesValidation(t *testing.T) {
	validMode := "0640"

	tests := []struct {
		name      string
		configure func(*Manifest)
		wantError string
	}{
		{
			name: "valid text",
			configure: func(manifest *Manifest) {
				manifest.QEMU.GuestAgent.SocketPath = "qga.sock"
				manifest.WriteFiles = WriteFiles{
					"/etc/agent.conf": writeFileText("hello"),
				}
			},
		},
		{
			name: "valid mode",
			configure: func(manifest *Manifest) {
				manifest.QEMU.GuestAgent.SocketPath = "qga.sock"
				manifest.WriteFiles = WriteFiles{
					"/etc/agent.conf": {Content: writeFileTextContent("hello"), Mode: validMode, FollowLinks: true},
				}
			},
		},
		{
			name: "allows arbitrary chown",
			configure: func(manifest *Manifest) {
				manifest.QEMU.GuestAgent.SocketPath = "qga.sock"
				manifest.WriteFiles = WriteFiles{
					"/etc/agent.conf": {Content: writeFileTextContent("hello"), Chown: "", FollowLinks: true},
				}
			},
		},
		{
			name: "valid host path",
			configure: func(manifest *Manifest) {
				manifest.QEMU.GuestAgent.SocketPath = "qga.sock"
				manifest.WriteFiles = WriteFiles{
					"/etc/agent.conf": writeFilePath("agent.conf"),
				}
			},
		},
		{
			name: "valid write back host path",
			configure: func(manifest *Manifest) {
				manifest.QEMU.GuestAgent.SocketPath = "qga.sock"
				manifest.WriteFiles = WriteFiles{
					"/etc/agent.conf": {Content: writeFilePathContent("agent.conf"), FollowLinks: true, WriteBack: true},
				}
			},
		},
		{
			name: "requires guest agent socket",
			configure: func(manifest *Manifest) {
				manifest.WriteFiles = WriteFiles{
					"/etc/agent.conf": writeFileText("hello"),
				}
			},
			wantError: "manifest.qemu.guestAgent.socketPath is required",
		},
		{
			name: "rejects relative guest path",
			configure: func(manifest *Manifest) {
				manifest.QEMU.GuestAgent.SocketPath = "qga.sock"
				manifest.WriteFiles = WriteFiles{
					"etc/agent.conf": writeFileText("hello"),
				}
			},
			wantError: "guest path must be absolute",
		},
		{
			name: "rejects missing source",
			configure: func(manifest *Manifest) {
				manifest.QEMU.GuestAgent.SocketPath = "qga.sock"
				manifest.WriteFiles = WriteFiles{
					"/etc/agent.conf": {},
				}
			},
			wantError: "must set exactly one",
		},
		{
			name: "rejects duplicate source",
			configure: func(manifest *Manifest) {
				manifest.QEMU.GuestAgent.SocketPath = "qga.sock"
				manifest.WriteFiles = WriteFiles{
					"/etc/agent.conf": {Content: WriteFileContent{Kind: 99, Text: "hello", Path: "agent.conf"}},
				}
			},
			wantError: "must set exactly one",
		},
		{
			name: "rejects empty host path",
			configure: func(manifest *Manifest) {
				manifest.QEMU.GuestAgent.SocketPath = "qga.sock"
				manifest.WriteFiles = WriteFiles{
					"/etc/agent.conf": writeFilePath(""),
				}
			},
			wantError: "path must not be empty",
		},
		{
			name: "rejects write back without host path",
			configure: func(manifest *Manifest) {
				manifest.QEMU.GuestAgent.SocketPath = "qga.sock"
				manifest.WriteFiles = WriteFiles{
					"/etc/agent.conf": {Content: writeFileTextContent("hello"), FollowLinks: true, WriteBack: true},
				}
			},
			wantError: "writeBack requires path",
		},
		{
			name: "allows mode without leading zero",
			configure: func(manifest *Manifest) {
				manifest.QEMU.GuestAgent.SocketPath = "qga.sock"
				manifest.WriteFiles = WriteFiles{
					"/etc/agent.conf": {Content: writeFileTextContent("hello"), Mode: "640", FollowLinks: true},
				}
			},
		},
		{
			name: "rejects invalid octal mode",
			configure: func(manifest *Manifest) {
				manifest.QEMU.GuestAgent.SocketPath = "qga.sock"
				manifest.WriteFiles = WriteFiles{
					"/etc/agent.conf": {Content: writeFileTextContent("hello"), Mode: "0888", FollowLinks: true},
				}
			},
			wantError: "mode must match",
		},
		{
			name: "rejects symbolic mode",
			configure: func(manifest *Manifest) {
				manifest.QEMU.GuestAgent.SocketPath = "qga.sock"
				manifest.WriteFiles = WriteFiles{
					"/etc/agent.conf": {Content: writeFileTextContent("hello"), Mode: "u=rw", FollowLinks: true},
				}
			},
			wantError: "mode must match",
		},
		{
			name: "rejects empty mode",
			configure: func(manifest *Manifest) {
				manifest.QEMU.GuestAgent.SocketPath = "qga.sock"
				manifest.WriteFiles = WriteFiles{
					"/etc/agent.conf": {Content: writeFileTextContent("hello"), Mode: "", FollowLinks: true},
				}
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			manifest := validManifest()
			tt.configure(manifest)

			err := manifest.Validate()
			if tt.wantError == "" {
				if err != nil {
					t.Fatalf("unexpected validation error: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.wantError) {
				t.Fatalf("expected validation error containing %q, got %v", tt.wantError, err)
			}
		})
	}
}

func TestResolvedWriteFilesResolvesRelativeHostPaths(t *testing.T) {
	manifest := validManifest()
	text := "hello"
	chown := "agent:users"
	mode := "0640"
	overwrite := false
	overwriteTrue := true
	followLinksFalse := false
	writeBackTrue := true
	relativeHostPath := "files/agent.conf"
	absoluteHostPath := "/tmp/host.conf"
	manifest.WriteFiles = WriteFiles{
		"/etc/a.conf": {Content: writeFilePathContent(relativeHostPath), Overwrite: overwrite, FollowLinks: followLinksFalse, WriteBack: writeBackTrue},
		"/etc/b.conf": {Content: writeFileTextContent(text), Chown: chown, Mode: mode, Overwrite: overwriteTrue, FollowLinks: true},
		"/etc/c.conf": {Content: writeFilePathContent(absoluteHostPath), FollowLinks: true},
	}

	got := manifest.ResolvedWriteFiles()
	want := []ResolvedWriteFile{
		{GuestPath: "/etc/a.conf", Content: writeFilePathContent("/tmp/work/files/agent.conf"), Overwrite: false, FollowLinks: false, WriteBack: true},
		{GuestPath: "/etc/b.conf", Content: writeFileTextContent(text), Chown: chown, Mode: mode, Overwrite: true, FollowLinks: true},
		{GuestPath: "/etc/c.conf", Content: writeFilePathContent(absoluteHostPath), Overwrite: false, FollowLinks: true},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("unexpected resolved write files: got %#v want %#v", got, want)
	}
}

func TestManifestNotificationsValidationAndResolution(t *testing.T) {
	t.Run("disabled by default", func(t *testing.T) {
		manifest := validManifest()
		if err := manifest.Validate(); err != nil {
			t.Fatalf("validate manifest: %v", err)
		}
		if !manifest.Notifications.Command.IsZero() {
			t.Fatalf("expected disabled notifications by default, got %#v", manifest.Notifications)
		}
	})

	t.Run("accepts command path args and states", func(t *testing.T) {
		manifest := validManifest()
		manifest.Notifications = Notifications{
			Command: Command{
				Path: "bin/notify",
				Args: []string{"--verbose"},
			},
			States: []string{"runtime:resume", "balloon:resize"},
		}
		if err := manifest.Validate(); err != nil {
			t.Fatalf("validate manifest: %v", err)
		}

		resolved := manifest.ResolvedNotifications()
		if resolved.Command.IsZero() {
			t.Fatal("expected resolved notification command")
		}
		if got, want := resolved.Command.Path, "bin/notify"; got != want {
			t.Fatalf("unexpected resolved command path: got %q want %q", got, want)
		}
		if got, want := resolved.Command.Args, []string{"--verbose"}; !reflect.DeepEqual(got, want) {
			t.Fatalf("unexpected resolved command args: got %v want %v", got, want)
		}
		if got, want := resolved.States, []string{"runtime:resume", "balloon:resize"}; !reflect.DeepEqual(got, want) {
			t.Fatalf("unexpected resolved states: got %v want %v", got, want)
		}
	})

	t.Run("accepts args without path", func(t *testing.T) {
		data := []byte(`{
			"host_name": "agent-sandbox",
			"working_dir": "/tmp/work",
			"state_dir": ".virtle",
			"ssh": {"exec": ["/bin/ssh"], "user": "agent"},
			"qemu": {"exec": ["/bin/qemu-system-x86_64"]},
			"machine": {"memory": 1024},
			"kernel": {"path": "/tmp/vmlinuz", "initrd_path": "/tmp/initrd"},
			"mounts": [
				{"type": "virtiofs", "tag": "workspace", "virtiofs": {"socket": "fs.sock", "bin": "/tmp/virtiofsd-workspace"}},
				{"type": "image", "source": "root.img", "image": {"size": 256, "create": true}}
			],
			"notifications": {"exec": ["--verbose"]}
		}`)
		loaded, err := loadBytes(data, "")
		if err != nil {
			t.Fatalf("load manifest: %v", err)
		}
		if loaded.Notifications.Command.IsZero() {
			t.Fatal("expected notification command after load")
		}
		if got := loaded.Notifications.Command.Path; got != "--verbose" {
			t.Fatalf("unexpected notification command path: got %q want --verbose", got)
		}
	})

	t.Run("preserves through load", func(t *testing.T) {
		document := validDocument()
		document.Notifications = NotificationsInput{
			Exec:   []string{"/bin/notify", "--state"},
			States: []string{"runtime:suspend"},
		}
		data, err := json.Marshal(document)
		if err != nil {
			t.Fatalf("marshal manifest: %v", err)
		}

		loaded, err := loadBytes(data, "")
		if err != nil {
			t.Fatalf("load manifest: %v", err)
		}
		if loaded.Notifications.Command.IsZero() {
			t.Fatal("expected notification command after load")
		}
		if got, want := loaded.Notifications.Command, (Command{Path: "/bin/notify", Args: []string{"--state"}}); !reflect.DeepEqual(got, want) {
			t.Fatalf("unexpected loaded command: got %#v want %#v", got, want)
		}
		if got, want := loaded.Notifications.States, document.Notifications.States; !reflect.DeepEqual(got, want) {
			t.Fatalf("unexpected loaded states: got %#v want %#v", got, want)
		}
	})
}

func TestManifestAllowsExternalVirtioFSSocket(t *testing.T) {
	manifest := validManifest()
	manifest.Run = nil
	manifest.CleanupFiles = nil
	manifest.QEMU.Devices.VirtioFS[0].SocketPath = "/var/run/virtiofs-nix-store.sock"

	if err := manifest.Validate(); err != nil {
		t.Fatalf("unexpected validation error: %v", err)
	}

	cleanupFiles, err := manifest.ResolvedCleanupFiles()
	if err != nil {
		t.Fatalf("resolve cleanup files: %v", err)
	}
	if len(cleanupFiles) != 0 {
		t.Fatalf("unexpected cleanup files: got %v want none", cleanupFiles)
	}

	virtioFSSocketPaths, err := manifest.ResolvedVirtioFSSocketPaths()
	if err != nil {
		t.Fatalf("resolve virtiofs socket paths: %v", err)
	}
	if got, want := virtioFSSocketPaths, []string{"/var/run/virtiofs-nix-store.sock"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("unexpected virtiofs socket paths: got %v want %v", got, want)
	}

	externalSocketPaths, err := manifest.ResolvedExternalVirtioFSSocketPaths()
	if err != nil {
		t.Fatalf("resolve external virtiofs socket paths: %v", err)
	}
	if got, want := externalSocketPaths, []string{"/var/run/virtiofs-nix-store.sock"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("unexpected external virtiofs socket paths: got %v want %v", got, want)
	}
}

func TestManifestExternalVirtioFSDetectionUsesCleanupOwnership(t *testing.T) {
	manifest := validManifest()
	manifest.Run = nil
	manifest.CleanupFiles = []string{"fs.sock"}

	externalSocketPaths, err := manifest.ResolvedExternalVirtioFSSocketPaths()
	if err != nil {
		t.Fatalf("resolve external virtiofs socket paths: %v", err)
	}
	if len(externalSocketPaths) != 0 {
		t.Fatalf("unexpected external virtiofs socket paths: got %v want none", externalSocketPaths)
	}
}

func TestManifestCleanupValidation(t *testing.T) {
	manifest := validManifest()
	manifest.CleanupFiles = []string{"cleanup.sock", ""}

	err := manifest.Validate()
	if err == nil || !strings.Contains(err.Error(), "manifest.cleanupFiles[1] must not be empty") {
		t.Fatalf("expected empty cleanup file validation error, got %v", err)
	}
}

func TestResolvedVirtioFSRunsRenderExecTemplates(t *testing.T) {
	manifest := validManifest()
	manifest.Run[0].Exec = []string{"/tmp/work/bin/virtiofsd-{{.MountTag}}", "--socket-path={{.Socket}}", "--source={{.MountSource}}", "--user={{.Env.USER}}"}
	t.Setenv("USER", "template-user")

	runs, err := manifest.ResolvedRuns(3)
	if err != nil {
		t.Fatalf("resolve runs: %v", err)
	}
	if got, want := runs[0].Exec[0], "/tmp/work/bin/virtiofsd-workspace"; got != want {
		t.Fatalf("unexpected run path: got %q want %q", got, want)
	}
	if got, want := runs[0].Exec[1:], []string{"--socket-path=/tmp/work/fs.sock", "--source=/tmp/work", "--user=template-user"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("unexpected run args: got %#v want %#v", got, want)
	}
}

func TestDocumentMountDefaults(t *testing.T) {
	document := validDocument()
	document.Mounts = MountsInput{
		VirtioFSMountInput{
			MountInput: MountInput{
				Tag:        "workspace",
				SourcePath: ".",
			},
			VirtioFS: VirtioFSInput{
				Socket: "fs.sock",
				Args:   []string{"--socket-path={{.Socket}}"},
			},
		},
		NinePMountInput{
			MountInput: MountInput{
				Tag:        "cache",
				SourcePath: "cache",
			},
		},
	}

	manifest, err := document.Manifest()
	if err != nil {
		t.Fatalf("resolve manifest: %v", err)
	}
	if got, want := manifest.Run[0].Exec[0], "virtiofsd"; got != want {
		t.Fatalf("unexpected default virtiofs binary: got %q want %q", got, want)
	}
	if got, want := manifest.QEMU.Devices.NineP[0].SecurityModel, "mapped"; got != want {
		t.Fatalf("unexpected default 9p security model: got %q want %q", got, want)
	}

	virtioFSMount := document.Mounts[0].(VirtioFSMountInput)
	virtioFSMount.VirtioFS = VirtioFSInput{
		Socket: "fs.sock",
		Bin:    "/tmp/virtiofsd-workspace",
	}
	document.Mounts[0] = virtioFSMount
	manifest, err = document.Manifest()
	if err != nil {
		t.Fatalf("resolve manifest: %v", err)
	}
	if got, want := manifest.Run[0].Exec[1:], []string{
		"--socket-path={{.Socket}}",
		"--shared-dir={{.MountSource}}",
		"--tag={{.MountTag}}",
	}; !reflect.DeepEqual(got, want) {
		t.Fatalf("unexpected default virtiofs args: got %#v want %#v", got, want)
	}
}

func TestManifestValidatesQEMUDeviceTransports(t *testing.T) {
	tests := []struct {
		name      string
		configure func(*Manifest)
		wantError string
	}{
		{
			name: "rng",
			configure: func(manifest *Manifest) {
				manifest.QEMU.Devices.RNG.Transport = "usb"
			},
			wantError: "manifest.qemu.devices.rng.transport must be one of pci, mmio, or ccw",
		},
		{
			name: "vsock",
			configure: func(manifest *Manifest) {
				manifest.QEMU.Devices.VSOCK.Transport = "usb"
			},
			wantError: "manifest.qemu.devices.vsock.transport must be one of pci, mmio, or ccw",
		},
		{
			name: "virtiofs",
			configure: func(manifest *Manifest) {
				manifest.QEMU.Devices.VirtioFS[0].Transport = "usb"
			},
			wantError: "manifest.qemu.devices.virtiofs[0].transport must be one of pci, mmio, or ccw",
		},
		{
			name: "virtiofs socket path",
			configure: func(manifest *Manifest) {
				manifest.QEMU.Devices.VirtioFS[0].SocketPath = ""
			},
			wantError: "manifest.qemu.devices.virtiofs[0].socketPath is required",
		},
		{
			name: "balloon",
			configure: func(manifest *Manifest) {
				manifest.QEMU.Devices.Balloon = validManifestWithBalloon().QEMU.Devices.Balloon
				manifest.QEMU.Devices.Balloon.Transport = "usb"
			},
			wantError: "manifest.qemu.devices.balloon.transport must be one of pci, mmio, or ccw",
		},
		{
			name: "9p",
			configure: func(manifest *Manifest) {
				manifest.QEMU.Devices.NineP = []QEMUNinePShare{{Transport: "usb"}}
			},
			wantError: "manifest.qemu.devices.9p[0].transport must be one of pci, mmio, or ccw",
		},
		{
			name: "block",
			configure: func(manifest *Manifest) {
				manifest.QEMU.Devices.Block[0].Transport = "usb"
			},
			wantError: "manifest.qemu.devices.block[0].transport must be one of pci, mmio, or ccw",
		},
		{
			name: "network",
			configure: func(manifest *Manifest) {
				manifest.QEMU.Devices.Network[0].Transport = "usb"
			},
			wantError: "manifest.qemu.devices.network[0].transport must be one of pci, mmio, or ccw",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			manifest := validManifest()
			tt.configure(manifest)

			err := manifest.Validate()
			if err == nil || !strings.Contains(err.Error(), tt.wantError) {
				t.Fatalf("expected validation error containing %q, got %v", tt.wantError, err)
			}
		})
	}
}

func TestManifestValidatesQEMUGraphicsBackend(t *testing.T) {
	for _, backend := range []string{"gtk", "cocoa"} {
		t.Run("allows "+backend, func(t *testing.T) {
			manifest := validManifest()
			manifest.QEMU.Knobs.NoGraphic = false
			manifest.QEMU.Graphics = QEMUGraphics{Backend: backend}

			if err := manifest.Validate(); err != nil {
				t.Fatalf("unexpected validation error: %v", err)
			}
		})
	}

	t.Run("rejects unsupported backend", func(t *testing.T) {
		manifest := validManifest()
		manifest.QEMU.Knobs.NoGraphic = false
		manifest.QEMU.Graphics = QEMUGraphics{Backend: "vnc"}

		err := manifest.Validate()
		if err == nil || !strings.Contains(err.Error(), "manifest.qemu.graphics.backend must be one of gtk or cocoa") {
			t.Fatalf("expected graphics backend validation error, got %v", err)
		}
	})
}

func TestLoadRejectsMalformedForwardEndpoints(t *testing.T) {
	tests := []struct {
		name      string
		host      string
		guest     string
		wantError string
	}{
		{
			name:      "host missing port",
			host:      "127.0.0.1",
			guest:     "10.0.2.15:22",
			wantError: "manifest.networks[0].forward[0].host missing :port",
		},
		{
			name:      "host non integer port",
			host:      "127.0.0.1:http",
			guest:     "10.0.2.15:22",
			wantError: "manifest.networks[0].forward[0].host port must be an integer",
		},
		{
			name:      "guest out of range port",
			host:      "127.0.0.1:2222",
			guest:     "10.0.2.15:70000",
			wantError: "manifest.networks[0].forward[0].guest port must be between 1 and 65535",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			document := validDocument()
			document.Networks = []NetworkInput{
				{
					Forward: []ForwardPort{
						{
							Proto: "tcp",
							From:  "host",
							Host:  tt.host,
							Guest: tt.guest,
						},
					},
				},
			}

			data, err := json.Marshal(document)
			if err != nil {
				t.Fatalf("marshal manifest: %v", err)
			}

			_, err = loadBytes(data, "")
			if err == nil || !strings.Contains(err.Error(), tt.wantError) {
				t.Fatalf("expected error containing %q, got %v", tt.wantError, err)
			}
		})
	}
}

func TestLoadDefaultsForwardPortProtoAndFrom(t *testing.T) {
	document := validDocument()
	document.Networks = []NetworkInput{
		{
			Forward: []ForwardPort{
				{
					Host:  "127.0.0.1:2222",
					Guest: "10.0.2.15:22",
				},
			},
		},
	}

	data, err := json.Marshal(document)
	if err != nil {
		t.Fatalf("marshal manifest: %v", err)
	}

	loaded, err := loadBytes(data, "")
	if err != nil {
		t.Fatalf("load manifest: %v", err)
	}
	if got, want := loaded.QEMU.Devices.Network[0].NetdevOptions, []string{"hostfwd=tcp:127.0.0.1:2222-10.0.2.15:22"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("unexpected network forward options: got %#v want %#v", got, want)
	}
}

func TestLoadGuestForwardUsesTunnelExecTemplate(t *testing.T) {
	tests := []struct {
		name          string
		fwdTunnelExec []string
		want          []string
	}{
		{
			name: "default netcat",
			want: []string{"guestfwd=tcp:10.0.2.15:2222-cmd:nc 127.0.0.1 22"},
		},
		{
			name:          "explicit netcat template",
			fwdTunnelExec: []string{"/bin/nc", "{{.Host}}", "{{.Port}}"},
			want:          []string{"guestfwd=tcp:10.0.2.15:2222-cmd:/bin/nc 127.0.0.1 22"},
		},
		{
			name:          "socat template",
			fwdTunnelExec: []string{"socat", "-", "TCP:{{.Host}}:{{.Port}}"},
			want:          []string{"guestfwd=tcp:10.0.2.15:2222-cmd:socat - TCP:127.0.0.1:22"},
		},
		{
			name:          "shell template",
			fwdTunnelExec: []string{"sh", "-c", "socat - TCP:{{.Host}}:{{.Port}}"},
			want:          []string{"guestfwd=tcp:10.0.2.15:2222-cmd:sh -c 'socat - TCP:127.0.0.1:22'"},
		},
		{
			name:          "quotes shell-sensitive args",
			fwdTunnelExec: []string{"~/bin/nc", "{{.Host}}", "{{.Port}}"},
			want:          []string{"guestfwd=tcp:10.0.2.15:2222-cmd:\\~/bin/nc 127.0.0.1 22"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			document := validDocument()
			document.QEMU.FwdTunnelExec = tt.fwdTunnelExec
			document.Networks = []NetworkInput{
				{
					Forward: []ForwardPort{
						{
							Proto: "tcp",
							From:  "guest",
							Host:  "127.0.0.1:22",
							Guest: "10.0.2.15:2222",
						},
					},
				},
			}

			data, err := json.Marshal(document)
			if err != nil {
				t.Fatalf("marshal manifest: %v", err)
			}

			loaded, err := loadBytes(data, "")
			if err != nil {
				t.Fatalf("load manifest: %v", err)
			}
			if got := loaded.QEMU.Devices.Network[0].NetdevOptions; !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("unexpected network forward options: got %#v want %#v", got, tt.want)
			}
		})
	}
}

func TestLoadRejectsInvalidForwardOptions(t *testing.T) {
	tests := []struct {
		name      string
		port      ForwardPort
		wantError string
	}{
		{
			name: "unsupported proto",
			port: ForwardPort{
				Proto: "icmp",
				From:  "host",
				Host:  "127.0.0.1:2222",
				Guest: "10.0.2.15:22",
			},
			wantError: "manifest.networks[0].forward[0].proto must be one of tcp or udp",
		},
		{
			name: "unsupported direction",
			port: ForwardPort{
				Proto: "tcp",
				From:  "outside",
				Host:  "127.0.0.1:2222",
				Guest: "10.0.2.15:22",
			},
			wantError: "manifest.networks[0].forward[0].from must be one of host or guest",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			document := validDocument()
			document.Networks = []NetworkInput{
				{
					Forward: []ForwardPort{tt.port},
				},
			}

			data, err := json.Marshal(document)
			if err != nil {
				t.Fatalf("marshal manifest: %v", err)
			}

			_, err = loadBytes(data, "")
			if err == nil || !strings.Contains(err.Error(), tt.wantError) {
				t.Fatalf("expected error containing %q, got %v", tt.wantError, err)
			}
		})
	}
}

func TestLoadTreatsHeadlessGraphicsAsAbsentForTransport(t *testing.T) {
	for _, graphics := range []*GraphicsInput{
		nil,
		{Backend: "headless"},
	} {
		name := "omitted"
		if graphics != nil {
			name = "explicit headless"
		}
		t.Run(name, func(t *testing.T) {
			document := validDocument()
			document.Mounts = MountsInput{}
			document.Graphics = graphics

			data, err := json.Marshal(document)
			if err != nil {
				t.Fatalf("marshal manifest: %v", err)
			}

			loaded, err := loadBytes(data, "")
			if err != nil {
				t.Fatalf("load manifest: %v", err)
			}
			if got, want := loaded.QEMU.Devices.RNG.Transport, "mmio"; got != want {
				t.Fatalf("unexpected rng transport: got %q want %q", got, want)
			}
			if got, want := loaded.QEMU.Devices.Network[0].Transport, "mmio"; got != want {
				t.Fatalf("unexpected network transport: got %q want %q", got, want)
			}
			if !loaded.QEMU.Graphics.IsZero() {
				t.Fatalf("expected no qemu graphics for headless manifest, got %#v", loaded.QEMU.Graphics)
			}
		})
	}
}

func TestManifestTransportSelectionForMicroVM(t *testing.T) {
	imageMount := ImageMountInput{
		SourcePath: "root.img",
		Image: ImageInput{
			Size:       256,
			FSType:     "ext4",
			AutoCreate: true,
		},
	}

	tests := []struct {
		name          string
		mounts        MountsInput
		graphics      *GraphicsInput
		wantTransport string
		wantPCIE      string
	}{
		{
			name:          "image only keeps mmio",
			mounts:        MountsInput{imageMount},
			wantTransport: "mmio",
			wantPCIE:      "pcie=off",
		},
		{
			name: "virtiofs forces pci",
			mounts: MountsInput{
				imageMount,
				VirtioFSMountInput{
					MountInput: MountInput{Tag: "workspace"},
					VirtioFS:   VirtioFSInput{Socket: "fs.sock"},
				},
			},
			wantTransport: "pci",
			wantPCIE:      "pcie=on",
		},
		{
			name: "9p forces pci",
			mounts: MountsInput{
				imageMount,
				NinePMountInput{
					MountInput: MountInput{
						Tag:        "cache",
						SourcePath: "cache",
					},
				},
			},
			wantTransport: "pci",
			wantPCIE:      "pcie=on",
		},
		{
			name:          "graphics forces pci",
			mounts:        MountsInput{imageMount},
			graphics:      &GraphicsInput{Backend: "gtk"},
			wantTransport: "pci",
			wantPCIE:      "pcie=on",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			document := validDocument()
			document.Host = HostInput{
				OS:     "linux",
				Arch:   "x86_64",
				System: "x86_64-linux",
			}
			document.QEMU.MachineOptions = nil
			document.Mounts = tt.mounts
			document.Graphics = tt.graphics

			manifest, err := document.Manifest()
			if err != nil {
				t.Fatalf("resolve manifest: %v", err)
			}
			if got := manifest.QEMU.Devices.RNG.Transport; got != tt.wantTransport {
				t.Fatalf("unexpected rng transport: got %q want %q", got, tt.wantTransport)
			}
			if got := manifest.QEMU.Devices.Network[0].Transport; got != tt.wantTransport {
				t.Fatalf("unexpected network transport: got %q want %q", got, tt.wantTransport)
			}
			if got := manifest.QEMU.Devices.Block[0].Transport; got != tt.wantTransport {
				t.Fatalf("unexpected block transport: got %q want %q", got, tt.wantTransport)
			}
			if got := manifest.QEMU.Devices.Mounts[0].Block.Transport; got != tt.wantTransport {
				t.Fatalf("unexpected ordered mount block transport: got %q want %q", got, tt.wantTransport)
			}
			if !slices.Contains(manifest.QEMU.Machine.Options, tt.wantPCIE) {
				t.Fatalf("expected machine options to contain %q, got %#v", tt.wantPCIE, manifest.QEMU.Machine.Options)
			}
		})
	}
}

func TestManifestNoGraphicDefaultsPreserveExplicitFalse(t *testing.T) {
	t.Run("defaults omitted headless manifest to noGraphic", func(t *testing.T) {
		manifest := validManifest()

		if err := manifest.Validate(); err != nil {
			t.Fatalf("validate manifest: %v", err)
		}
		if !manifest.QEMU.NoGraphicEnabled() {
			t.Fatalf("expected omitted noGraphic without graphics to default true")
		}
	})

	t.Run("preserves explicit false without typed graphics", func(t *testing.T) {
		document := validDocument()
		document.Graphics = &GraphicsInput{Backend: "gtk"}

		data, err := json.Marshal(document)
		if err != nil {
			t.Fatalf("marshal manifest: %v", err)
		}

		loaded, err := loadBytes(data, "")
		if err != nil {
			t.Fatalf("load manifest: %v", err)
		}
		if loaded.QEMU.Knobs.NoGraphic {
			t.Fatalf("expected noGraphic=false to be preserved, got %#v", loaded.QEMU.Knobs.NoGraphic)
		}
	})

	t.Run("defaults typed graphics to graphical", func(t *testing.T) {
		manifest := validManifest()
		manifest.QEMU.Knobs.NoGraphic = false
		manifest.QEMU.Graphics = QEMUGraphics{Backend: "gtk"}

		if err := manifest.Validate(); err != nil {
			t.Fatalf("validate manifest: %v", err)
		}
		if manifest.QEMU.Knobs.NoGraphic {
			t.Fatalf("expected omitted noGraphic with graphics to default false, got %#v", manifest.QEMU.Knobs.NoGraphic)
		}
	})
}

func TestManifestAllowsInitrdApplianceWithoutStorageDevices(t *testing.T) {
	manifest := validManifest()
	manifest.QEMU.Devices.VirtioFS = nil
	manifest.QEMU.Devices.Block = nil
	manifest.QEMU.Devices.Network = nil
	manifest.Volumes = nil
	manifest.Run = nil
	manifest.CleanupFiles = nil

	if err := manifest.Validate(); err != nil {
		t.Fatalf("unexpected validation error: %v", err)
	}

	virtioFSSocketPaths, err := manifest.ResolvedVirtioFSSocketPaths()
	if err != nil {
		t.Fatalf("resolve virtiofs socket paths: %v", err)
	}
	if len(virtioFSSocketPaths) != 0 {
		t.Fatalf("unexpected virtiofs socket paths: got %v want none", virtioFSSocketPaths)
	}
	if volumes := manifest.ResolvedVolumes(); len(volumes) != 0 {
		t.Fatalf("unexpected volumes: got %v want none", volumes)
	}
}

func TestDocumentHotplugVirtioFSMountGeneratesHotplugEntry(t *testing.T) {
	document := validDocument()
	document.Mounts = nil
	document.Hotplug.Mounts = MountsInput{
		VirtioFSMountInput{
			MountInput: MountInput{
				Tag:        "workspace",
				SourcePath: "shares/cache",
			},
			Target:   "/mnt/cache",
			VirtioFS: VirtioFSInput{Bin: "/tmp/virtiofsd-workspace"},
		},
	}

	manifest, err := document.Manifest()
	if err != nil {
		t.Fatalf("resolve manifest: %v", err)
	}

	if len(manifest.QEMU.Devices.VirtioFS) != 0 {
		t.Fatalf("expected hotplug mount to be excluded from launch qemu devices, got %#v", manifest.QEMU.Devices.VirtioFS)
	}
	if len(manifest.Run) != 0 {
		t.Fatalf("expected hotplug mount to be excluded from launch runs, got %#v", manifest.Run)
	}
	if got, want := manifest.QEMU.Hotplug.PCIEPorts, 1; got != want {
		t.Fatalf("unexpected pcie hotplug ports: got %d want %d", got, want)
	}
	if len(manifest.Hotplug) != 1 {
		t.Fatalf("expected one hotplug entry, got %#v", manifest.Hotplug)
	}

	device := manifest.Hotplug[0]
	if got, want := device.ID, "workspace"; got != want {
		t.Fatalf("unexpected hotplug id: got %q want %q", got, want)
	}
	if got, want := device.Kind, HotplugKindVirtioFS; got != want {
		t.Fatalf("unexpected hotplug kind: got %q want %q", got, want)
	}
	if got, want := device.VirtioFS.Target, "/mnt/cache"; got != want {
		t.Fatalf("unexpected mount target: got %q want %q", got, want)
	}
	if got, want := device.VirtioFS.Bin, "/tmp/virtiofsd-workspace"; got != want {
		t.Fatalf("unexpected hotplug exec path: got %q want %q", got, want)
	}
	if !containsString(device.VirtioFS.Args, "--socket-path=/tmp/work/.virtle/workspace.sock") {
		t.Fatalf("expected resolved socket arg, got %#v", device.VirtioFS.Args)
	}
	if !containsString(device.VirtioFS.Args, "--shared-dir=/tmp/work/shares/cache") {
		t.Fatalf("expected resolved source arg, got %#v", device.VirtioFS.Args)
	}
}

func TestDocumentHotplugVirtioFSMountDefaultBinUsesPATH(t *testing.T) {
	document := validDocument()
	document.Mounts = nil
	document.Hotplug.Mounts = MountsInput{
		VirtioFSMountInput{
			MountInput: MountInput{
				Tag:        "workspace",
				SourcePath: "shares/cache",
			},
		},
	}

	manifest, err := document.Manifest()
	if err != nil {
		t.Fatalf("resolve manifest: %v", err)
	}
	if got, want := manifest.Hotplug[0].VirtioFS.Bin, "virtiofsd"; got != want {
		t.Fatalf("unexpected hotplug virtiofs bin: got %q want %q", got, want)
	}
}

func TestDocumentHotplugVirtioFSRendersCustomArgs(t *testing.T) {
	document := validDocument()
	document.Mounts = nil
	document.Hotplug.Mounts = MountsInput{
		VirtioFSMountInput{
			MountInput: MountInput{
				Tag:        "cache",
				SourcePath: "shares/cache",
			},
			VirtioFS: VirtioFSInput{
				Args: []string{"--socket-path={{.Socket}}", "--shared-dir={{.MountSource}}", "--tag={{.MountTag}}", "--user={{.Env.USER}}"},
			},
		},
	}
	t.Setenv("USER", "template-user")

	manifest, err := document.Manifest()
	if err != nil {
		t.Fatalf("resolve manifest: %v", err)
	}
	if got, want := manifest.Hotplug[0].VirtioFS.Args, []string{"--socket-path=/tmp/work/.virtle/cache.sock", "--shared-dir=/tmp/work/shares/cache", "--tag=cache", "--user=template-user"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("unexpected hotplug args: got %#v want %#v", got, want)
	}
}

func TestDocumentImageMountFormatResolvesToQEMU(t *testing.T) {
	document := validDocument()
	imageMount := document.Mounts[1].(ImageMountInput)
	imageMount.Image.Format = "qcow2"
	document.Mounts[1] = imageMount

	manifest, err := document.Manifest()
	if err != nil {
		t.Fatalf("resolve manifest: %v", err)
	}
	if got, want := manifest.QEMU.Devices.Block[0].Format, "qcow2"; got != want {
		t.Fatalf("unexpected block format: got %q want %q", got, want)
	}
	if got, want := manifest.QEMU.Devices.Mounts[1].Block.Format, "qcow2"; got != want {
		t.Fatalf("unexpected mount block format: got %q want %q", got, want)
	}
}

func TestDocumentTypedHotplugEntries(t *testing.T) {
	document := validDocument()
	document.Mounts = nil
	document.Hotplug = HotplugInput{
		Mounts: MountsInput{
			VirtioFSMountInput{
				MountInput: MountInput{
					Tag:        "cache",
					SourcePath: "shares/cache",
				},
				Target: "/mnt/cache",
			},
			ImageMountInput{
				SourcePath: "data.qcow2",
				Image:      ImageInput{Serial: stringPtr("data"), Format: "qcow2"},
			},
		},
		Networks: []NetworkInput{
			{
				ID:   "vpn",
				Type: "user",
				MAC:  "02:02:00:00:00:10",
				Forward: []ForwardPort{{
					Proto: "tcp",
					Host:  "127.0.0.1:2223",
					Guest: "10.0.2.15:22",
				}},
			},
		},
	}

	manifest, err := document.Manifest()
	if err != nil {
		t.Fatalf("resolve manifest: %v", err)
	}
	if got, want := manifest.QEMU.Hotplug.PCIEPorts, 3; got != want {
		t.Fatalf("unexpected pcie ports: got %d want %d", got, want)
	}
	if got, want := []HotplugKind{manifest.Hotplug[0].Kind, manifest.Hotplug[1].Kind, manifest.Hotplug[2].Kind}, []HotplugKind{HotplugKindVirtioFS, HotplugKindBlock, HotplugKindNet}; !reflect.DeepEqual(got, want) {
		t.Fatalf("unexpected hotplug order: got %#v want %#v", got, want)
	}
	if got, want := manifest.Hotplug[0].VirtioFS.SocketPath, "/tmp/work/.virtle/cache.sock"; got != want {
		t.Fatalf("unexpected virtiofs socket: got %q want %q", got, want)
	}
	if got, want := manifest.Hotplug[0].VirtioFS.Bin, "virtiofsd"; got != want {
		t.Fatalf("unexpected virtiofs bin: got %q want %q", got, want)
	}
	if got, want := manifest.Hotplug[0].ID, "cache"; got != want {
		t.Fatalf("unexpected virtiofs id: got %q want %q", got, want)
	}
	if got, want := manifest.Hotplug[2].Net.Forward[0].Host, "127.0.0.1:2223"; got != want {
		t.Fatalf("unexpected net forward host: got %q want %q", got, want)
	}
	if got, want := manifest.Hotplug[1].ID, "data"; got != want {
		t.Fatalf("unexpected image id: got %q want %q", got, want)
	}
	if got, want := manifest.Hotplug[1].Block.ImagePath, "/tmp/work/data.qcow2"; got != want {
		t.Fatalf("unexpected block image: got %q want %q", got, want)
	}
	if got, want := manifest.Hotplug[1].Block.Format, "qcow2"; got != want {
		t.Fatalf("unexpected block format: got %q want %q", got, want)
	}
}

func TestDocumentHotplugNetworkForwardDefaultsProto(t *testing.T) {
	document := validDocument()
	document.Mounts = nil
	document.Hotplug = HotplugInput{
		Networks: []NetworkInput{
			{
				ID: "vpn",
				Forward: []ForwardPort{{
					Host:  "127.0.0.1:2223",
					Guest: "10.0.2.15:22",
				}},
			},
		},
	}

	manifest, err := document.Manifest()
	if err != nil {
		t.Fatalf("resolve manifest: %v", err)
	}
	if got, want := manifest.Hotplug[0].Net.Forward, []HotplugForward{{Proto: "tcp", Host: "127.0.0.1:2223", Guest: "10.0.2.15:22"}}; !reflect.DeepEqual(got, want) {
		t.Fatalf("unexpected hotplug forward: got %#v want %#v", got, want)
	}
}

func TestDocumentHotplugNetworkForwardValidation(t *testing.T) {
	tests := []struct {
		name      string
		forward   ForwardPort
		wantError string
	}{
		{
			name: "malformed host endpoint",
			forward: ForwardPort{
				Host:  "127.0.0.1",
				Guest: "10.0.2.15:22",
			},
			wantError: "manifest.hotplug.networks[0]: forward[0].host missing :port",
		},
		{
			name: "malformed guest endpoint",
			forward: ForwardPort{
				Host:  "127.0.0.1:2223",
				Guest: "10.0.2.15:ssh",
			},
			wantError: "manifest.hotplug.networks[0]: forward[0].guest port must be an integer",
		},
		{
			name: "invalid proto",
			forward: ForwardPort{
				Proto: "icmp",
				Host:  "127.0.0.1:2223",
				Guest: "10.0.2.15:22",
			},
			wantError: "manifest.hotplug.networks[0]: forward[0].proto must be one of tcp or udp",
		},
		{
			name: "guest origin unsupported",
			forward: ForwardPort{
				From:  "guest",
				Host:  "127.0.0.1:2223",
				Guest: "10.0.2.15:22",
			},
			wantError: "manifest.hotplug.networks[0]: forward[0].from guest is not supported for hotplug networks",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			document := validDocument()
			document.Mounts = nil
			document.Hotplug = HotplugInput{
				Networks: []NetworkInput{
					{
						ID:      "vpn",
						Forward: []ForwardPort{tt.forward},
					},
				},
			}

			_, err := document.Manifest()
			if err == nil || !strings.Contains(err.Error(), tt.wantError) {
				t.Fatalf("expected error containing %q, got %v", tt.wantError, err)
			}
		})
	}
}

func TestDecodeDocumentGroupedHotplugEntries(t *testing.T) {
	document, err := DecodeDocumentBytes([]byte(`
[kernel]
path = "/tmp/vmlinuz"
initrd_path = "/tmp/initrd"

[[hotplug.mounts]]
type = "virtiofs"
tag = "workspace"
source = "."

[[hotplug.mounts]]
type = "image"
source = ".virtle/root.img"
image.serial = "root"

[[hotplug.networks]]
id = "vpn"
type = "user"
mac = "02:02:00:00:00:10"
`), "manifest.toml")
	if err != nil {
		t.Fatalf("decode document: %v", err)
	}

	manifest, err := document.Manifest()
	if err != nil {
		t.Fatalf("resolve manifest: %v", err)
	}
	if got, want := []string{manifest.Hotplug[0].ID, manifest.Hotplug[1].ID, manifest.Hotplug[2].ID}, []string{"workspace", "root", "vpn"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("unexpected hotplug ids: got %#v want %#v", got, want)
	}
	if got, want := manifest.Hotplug[1].Block.Format, "raw"; got != want {
		t.Fatalf("unexpected image format: got %q want %q", got, want)
	}
}

func TestDocumentExplicitVirtioFSHotplugEnablesSharedMemory(t *testing.T) {
	document := validDocument()
	document.Host.OS = "linux"
	document.Mounts = nil
	document.Hotplug = HotplugInput{
		Mounts: MountsInput{
			VirtioFSMountInput{
				MountInput: MountInput{
					Tag:        "cache",
					SourcePath: "shares/cache",
				},
			},
		},
	}

	manifest, err := document.Manifest()
	if err != nil {
		t.Fatalf("resolve manifest: %v", err)
	}
	if !manifest.QEMU.Memory.Shared {
		t.Fatal("expected explicit virtiofs hotplug to enable shared memory")
	}
	if got, want := manifest.QEMU.Memory.Backend, "memfd"; got != want {
		t.Fatalf("unexpected memory backend: got %q want %q", got, want)
	}
	if len(manifest.QEMU.Devices.VirtioFS) != 0 {
		t.Fatalf("expected explicit virtiofs hotplug to stay out of launch devices, got %#v", manifest.QEMU.Devices.VirtioFS)
	}
	if got, want := manifest.QEMU.Hotplug.PCIEPorts, 1; got != want {
		t.Fatalf("unexpected pcie ports: got %d want %d", got, want)
	}
}

func TestDocumentExplicitMachineOptionsEnablePCIForHotplug(t *testing.T) {
	tests := []struct {
		name    string
		options map[string]string
	}{
		{
			name:    "missing pcie",
			options: map[string]string{"accel": "kvm:tcg"},
		},
		{
			name:    "pcie off",
			options: map[string]string{"accel": "kvm:tcg", "pcie": "off"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			document := validDocument()
			document.QEMU.MachineOptions = tt.options
			document.Mounts = nil
			document.Hotplug = HotplugInput{
				Networks: []NetworkInput{
					{
						ID:   "vpn",
						Type: "user",
						MAC:  "02:02:00:00:00:10",
					},
				},
			}

			manifest, err := document.Manifest()
			if err != nil {
				t.Fatalf("resolve manifest: %v", err)
			}
			if !containsString(manifest.QEMU.Machine.Options, "pcie=on") {
				t.Fatalf("expected pcie=on in machine options, got %#v", manifest.QEMU.Machine.Options)
			}
			if got, want := document.QEMU.MachineOptions["pcie"], tt.options["pcie"]; got != want {
				t.Fatalf("resolution mutated input machine options: got %q want %q", got, want)
			}
		})
	}
}

func TestDocumentWithoutHotplugAllocatesNoHotplugPorts(t *testing.T) {
	document := validDocument()
	document.Mounts = nil
	document.Hotplug = HotplugInput{}

	manifest, err := document.Manifest()
	if err != nil {
		t.Fatalf("resolve manifest: %v", err)
	}
	if got := manifest.QEMU.Hotplug.PCIEPorts; got != 0 {
		t.Fatalf("expected no hotplug ports, got %d", got)
	}
	if got, want := manifest.QEMU.Devices.RNG.Transport, "mmio"; got != want {
		t.Fatalf("unexpected transport: got %q want %q", got, want)
	}
}

func TestDocumentTypedHotplugValidation(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*Document)
		want   string
	}{
		{
			name: "duplicate ids across kinds",
			mutate: func(document *Document) {
				document.Hotplug = HotplugInput{
					Mounts:   MountsInput{ImageMountInput{SourcePath: "data.raw", Image: ImageInput{Serial: stringPtr("same")}}},
					Networks: []NetworkInput{{ID: "same", Type: "user", MAC: "02:02:00:00:00:10"}},
				}
			},
			want: "duplicates",
		},
		{
			name: "unsupported net backend",
			mutate: func(document *Document) {
				document.Hotplug = HotplugInput{Networks: []NetworkInput{{ID: "vpn", Type: "tap"}}}
			},
			want: "net.backend must be user",
		},
		{
			name: "unsupported image format",
			mutate: func(document *Document) {
				document.Hotplug = HotplugInput{Mounts: MountsInput{ImageMountInput{SourcePath: "data.vmdk", Image: ImageInput{Serial: stringPtr("data"), Format: "vmdk"}}}}
			},
			want: "block.format must be raw or qcow2",
		},
		{
			name: "invalid virtiofs args template",
			mutate: func(document *Document) {
				document.Hotplug = HotplugInput{Mounts: MountsInput{VirtioFSMountInput{
					MountInput: MountInput{Tag: "cache", SourcePath: "cache"},
					VirtioFS:   VirtioFSInput{Args: []string{"--socket-path={{.Missing}}"}},
				}}}
			},
			want: "map has no entry for key",
		},
		{
			name: "unsupported 9p hotplug mount",
			mutate: func(document *Document) {
				document.Hotplug = HotplugInput{Mounts: MountsInput{NinePMountInput{MountInput: MountInput{Tag: "cache", SourcePath: "cache"}}}}
			},
			want: "does not support hotplug",
		},
		{
			name: "image id required",
			mutate: func(document *Document) {
				document.Hotplug = HotplugInput{Mounts: MountsInput{ImageMountInput{SourcePath: "data.raw"}}}
			},
			want: "id is required",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			document := validDocument()
			document.Mounts = nil
			tt.mutate(&document)
			_, err := document.Manifest()
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("expected %q error, got %v", tt.want, err)
			}
		})
	}
}

func TestManifestHotplugRequiresPCITransport(t *testing.T) {
	manifest, err := validDocument().Manifest()
	if err != nil {
		t.Fatalf("resolve manifest: %v", err)
	}
	manifest.Hotplug = []HotplugDevice{{Kind: HotplugKindNet, ID: "vpn", Net: HotplugNet{Backend: "user", MAC: "02:02:00:00:00:10"}}}
	manifest.QEMU.Hotplug.PCIEPorts = 1
	manifest.QEMU.Devices.RNG.Transport = "mmio"
	if err := manifest.Validate(); err == nil || !strings.Contains(err.Error(), "requires pci transport") {
		t.Fatalf("expected pci transport error, got %v", err)
	}
}

func TestDocumentHotplugVirtioFSMountTargetIsOptional(t *testing.T) {
	document := validDocument()
	document.Mounts = nil
	document.Hotplug.Mounts = MountsInput{
		VirtioFSMountInput{
			MountInput: MountInput{
				Tag:        "workspace",
				SourcePath: "shares/cache",
			},
		},
	}

	manifest, err := document.Manifest()
	if err != nil {
		t.Fatalf("resolve manifest: %v", err)
	}
	if len(manifest.Hotplug) != 1 {
		t.Fatalf("expected one hotplug entry, got %#v", manifest.Hotplug)
	}
	if manifest.Hotplug[0].VirtioFS.Target != "" {
		t.Fatalf("expected omitted target to skip guest mount command, got %#v", manifest.Hotplug[0].VirtioFS.Target)
	}
}

func TestManifestVolumeValidation(t *testing.T) {
	t.Run("allows empty image path when not auto creating", func(t *testing.T) {
		manifest := validManifest()
		manifest.Volumes = []Volume{{ImagePath: "", AutoCreate: false}}

		if err := manifest.Validate(); err != nil {
			t.Fatalf("unexpected validation error: %v", err)
		}
	})

	t.Run("requires image path when auto creating", func(t *testing.T) {
		manifest := validManifest()
		manifest.Volumes = []Volume{{ImagePath: "", Size: 256, AutoCreate: true}}

		err := manifest.Validate()
		if err == nil || !strings.Contains(err.Error(), "manifest.mounts.image[0].source is required") {
			t.Fatalf("expected auto-create image path validation error, got %v", err)
		}
	})

	t.Run("requires size when auto creating", func(t *testing.T) {
		manifest := validManifest()
		manifest.Volumes = []Volume{{ImagePath: "root.img", Size: 0, AutoCreate: true}}

		err := manifest.Validate()
		if err == nil || !strings.Contains(err.Error(), "manifest.mounts.image[0].image.size must be greater than zero") {
			t.Fatalf("expected auto-create size validation error, got %v", err)
		}
	})

	t.Run("rejects auto-created volumes below minimum size", func(t *testing.T) {
		manifest := validManifest()
		manifest.Volumes = []Volume{{ImagePath: "root.img", Size: 255, AutoCreate: true}}

		err := manifest.Validate()
		if err == nil || !strings.Contains(err.Error(), "manifest.mounts.image[0].image.size must be at least 256") {
			t.Fatalf("expected auto-create minimum size validation error, got %v", err)
		}
	})

	t.Run("rejects non-ext4 filesystem when auto creating", func(t *testing.T) {
		manifest := validManifest()
		manifest.Volumes = []Volume{{ImagePath: "root.img", Size: 256, FSType: "xfs", AutoCreate: true}}

		err := manifest.Validate()
		if err == nil || !strings.Contains(err.Error(), `manifest.mounts.image[0].image.fs must be "ext4"`) {
			t.Fatalf("expected auto-create fsType validation error, got %v", err)
		}
	})

	t.Run("rejects mkfs extra args when auto creating", func(t *testing.T) {
		manifest := validManifest()
		manifest.Volumes = []Volume{{
			ImagePath:     "root.img",
			Size:          256,
			AutoCreate:    true,
			MkfsExtraArgs: []string{"-E", "discard"},
		}}

		err := manifest.Validate()
		if err == nil || !strings.Contains(err.Error(), "manifest.mounts.image[0].image.mkfs_extra_args is not supported") {
			t.Fatalf("expected auto-create mkfsExtraArgs validation error, got %v", err)
		}
	})

	t.Run("allows label when auto creating ext4", func(t *testing.T) {
		manifest := validManifest()
		label := "persist"
		manifest.Volumes = []Volume{{ImagePath: "root.img", Size: 256, AutoCreate: true, Label: label}}

		if err := manifest.Validate(); err != nil {
			t.Fatalf("unexpected validation error: %v", err)
		}
	})
}

func TestManifestAllowsRuntimeSelectedCPUCount(t *testing.T) {
	manifest := validManifest()
	manifest.QEMU.SMP.CPUs = CPUCount{}

	if err := manifest.Validate(); err != nil {
		t.Fatalf("validate omitted CPU count: %v", err)
	}
}

func TestManifestRejectsNonPositiveMachineResources(t *testing.T) {
	tests := []struct {
		name      string
		configure func(*Manifest)
		wantError string
	}{
		{
			name: "zero memory",
			configure: func(manifest *Manifest) {
				manifest.QEMU.Memory.Size = 0
			},
			wantError: "manifest.qemu.memory.sizeMiB must be greater than zero",
		},
		{
			name: "negative memory",
			configure: func(manifest *Manifest) {
				manifest.QEMU.Memory.Size = -1
			},
			wantError: "manifest.qemu.memory.sizeMiB must be greater than zero",
		},
		{
			name: "explicit zero CPUs",
			configure: func(manifest *Manifest) {
				manifest.QEMU.SMP.CPUs = ExplicitCPUs(0)
			},
			wantError: "manifest.qemu.smp.cpus must be greater than zero when set",
		},
		{
			name: "negative CPUs",
			configure: func(manifest *Manifest) {
				manifest.QEMU.SMP.CPUs = ExplicitCPUs(-1)
			},
			wantError: "manifest.qemu.smp.cpus must be greater than zero when set",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			manifest := validManifest()
			tt.configure(manifest)
			if err := manifest.Validate(); err == nil || !strings.Contains(err.Error(), tt.wantError) {
				t.Fatalf("validation error = %v, want %q", err, tt.wantError)
			}
		})
	}
}

// loadBytes decodes and resolves a manifest the way production does
// (DecodeDocumentBytes then Manifest), as a test convenience.
func loadBytes(data []byte, name string) (*Manifest, error) {
	doc, err := DecodeDocumentBytes(data, name)
	if err != nil {
		return nil, err
	}
	return doc.Manifest()
}

func stringPtr(value string) *string {
	return &value
}

func validDocument() Document {
	return Document{
		HostName:   "agent-sandbox",
		WorkingDir: "/tmp/work",
		StateDir:   ".virtle",
		QEMU: QEMUInput{
			Exec:           []string{"/bin/qemu-system-x86_64"},
			Seccomp:        true,
			MachineOptions: map[string]string{"accel": "kvm:tcg"},
		},
		Machine: MachineInput{
			Type:   "microvm",
			VCPU:   2,
			CPU:    "host",
			Memory: 1024,
		},
		Kernel: KernelInput{
			Path:       "/tmp/vmlinuz",
			InitrdPath: "/tmp/initrd",
		},
		Mounts: MountsInput{
			VirtioFSMountInput{
				MountInput: MountInput{
					Tag: "workspace",
				},
				VirtioFS: VirtioFSInput{
					Socket: "fs.sock",
					Bin:    "/tmp/virtiofsd-workspace",
				},
			},
			ImageMountInput{
				SourcePath: "root.img",
				Image: ImageInput{
					Size:       256,
					FSType:     "ext4",
					AutoCreate: true,
				},
			},
		},
		Networks: []NetworkInput{
			{
				ID:   "net0",
				Type: "user",
				MAC:  "02:02:00:00:00:01",
			},
		},
		SSH: SSHInput{
			Exec:       []string{"/bin/ssh"},
			User:       "agent",
			RetryDelay: units.Duration(500 * time.Millisecond),
		},
	}
}

func containsString(values []string, needle string) bool {
	for _, value := range values {
		if value == needle {
			return true
		}
	}
	return false
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

func TestDocumentHotplugPortsReservesExtraPCIEPorts(t *testing.T) {
	document := validDocument()
	document.QEMU.HotplugPorts = 2

	manifest, err := document.Manifest()
	if err != nil {
		t.Fatalf("resolve manifest: %v", err)
	}
	if got, want := manifest.QEMU.Hotplug.PCIEPorts, 2; got != want {
		t.Fatalf("unexpected pcie ports: got %d want %d", got, want)
	}
	// Reserved ports alone must force the PCI transport, exactly as listed
	// hotplug devices do.
	if got, want := manifest.QEMU.Devices.RNG.Transport, "pci"; got != want {
		t.Fatalf("unexpected transport: got %q want %q", got, want)
	}
}

func TestDocumentHotplugPortsBelowDeviceCountKeepsDeviceCount(t *testing.T) {
	document := validDocument()
	document.Mounts = nil
	document.QEMU.HotplugPorts = 1
	document.Hotplug = HotplugInput{
		Mounts: MountsInput{
			VirtioFSMountInput{
				MountInput: MountInput{Tag: "a", SourcePath: "shares/a"},
			},
			VirtioFSMountInput{
				MountInput: MountInput{Tag: "b", SourcePath: "shares/b"},
			},
		},
	}

	manifest, err := document.Manifest()
	if err != nil {
		t.Fatalf("resolve manifest: %v", err)
	}
	if got, want := manifest.QEMU.Hotplug.PCIEPorts, 2; got != want {
		t.Fatalf("unexpected pcie ports: got %d want %d", got, want)
	}
}

func TestDocumentHotplugPortsRejectsNegative(t *testing.T) {
	document := validDocument()
	document.QEMU.HotplugPorts = -1

	if _, err := document.Manifest(); err == nil || !strings.Contains(err.Error(), "hotplug_ports must not be negative") {
		t.Fatalf("expected negative ports error, got %v", err)
	}
}
