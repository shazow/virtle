package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/jessevdk/go-flags"
	"github.com/shazow/virtle/internal/manager/control"
)

func TestOptionsDeclaresCommands(t *testing.T) {
	tests := []struct {
		field           string
		command         string
		description     string
		longDescription string
	}{
		{
			field:           "Launch",
			command:         "launch",
			description:     "Launch a virtiofs + ssh sandbox session",
			longDescription: "Start configured host-side run processes, launch QEMU directly, then optionally attach over ssh.",
		},
		{
			field:           "Suspend",
			command:         "suspend",
			description:     "Suspend a running sandbox session",
			longDescription: "Save QEMU state to disk and exit the launch session.",
		},
		{
			field:           "Hotplug",
			command:         "hotplug",
			description:     "Attach or detach a predefined hotplug device",
			longDescription: "Attach or detach a device described under manifest [hotplug].",
		},
		{
			field:           "RPC",
			command:         "rpc",
			description:     "Call a virtle control socket RPC method",
			longDescription: "Call a method on the running virtle control socket with optional JSON params.",
		},
		{
			field:           "ManifestCommand",
			command:         "manifest",
			description:     "Inspect and work with virtle manifests",
			longDescription: "Inspect and work with the virtle manifest input format.",
		},
	}

	optionsType := reflect.TypeOf(Options{})
	for _, tt := range tests {
		t.Run(tt.command, func(t *testing.T) {
			field, ok := optionsType.FieldByName(tt.field)
			if !ok {
				t.Fatalf("Options is missing %s command field", tt.field)
			}
			if field.Type.Kind() != reflect.Struct || field.Type.Name() != "" {
				t.Fatalf("%s field type = %s, want anonymous struct in Options", tt.field, field.Type)
			}
			if got := field.Tag.Get("command"); got != tt.command {
				t.Fatalf("%s command tag = %q, want %q", tt.field, got, tt.command)
			}
			if got := field.Tag.Get("description"); got != tt.description {
				t.Fatalf("%s description tag = %q, want %q", tt.field, got, tt.description)
			}
			if got := field.Tag.Get("long-description"); got != tt.longDescription {
				t.Fatalf("%s long-description tag = %q, want %q", tt.field, got, tt.longDescription)
			}
		})
	}
}

func TestParserRejectsInvalidCommandLines(t *testing.T) {
	tests := []struct {
		name    string
		args    []string
		wantErr string
	}{
		{
			name:    "launch missing manifest",
			args:    []string{"launch"},
			wantErr: "no manifest path provided",
		},
		{
			name:    "launch positional manifest",
			args:    []string{"launch", "/tmp/manifest.json"},
			wantErr: "remote command arguments require --ssh",
		},
		{
			name:    "suspend missing manifest",
			args:    []string{"suspend"},
			wantErr: "no manifest path provided",
		},
		{
			name:    "remote command without ssh",
			args:    []string{"--manifest=/tmp/manifest.json", "launch", "--", "echo", "hi"},
			wantErr: "remote command arguments require --ssh",
		},
		{
			name:    "invalid launch resume mode",
			args:    []string{"--manifest=/tmp/manifest.json", "launch", "--resume=maybe"},
			wantErr: "Invalid value",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := run(tt.args)
			if err == nil {
				t.Fatalf("run(%v) succeeded, expected error containing %q", tt.args, tt.wantErr)
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("run(%v) error %q does not contain %q", tt.args, err, tt.wantErr)
			}
		})
	}
}

func TestRunWithoutCommandReturnsHelpOnce(t *testing.T) {
	tests := []struct {
		name         string
		args         []string
		wantCommands []string
	}{
		{
			name:         "no arguments",
			args:         nil,
			wantCommands: []string{"hotplug", "launch", "manifest", "rpc", "suspend"},
		},
		{
			name:         "manifest without subcommand",
			args:         []string{"manifest"},
			wantCommands: []string{"defaults", "resolve", "schema", "validate"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var err error
			stderr := captureStderr(t, func() {
				stdout := captureStdout(t, func() {
					err = run(tt.args)
				})
				if stdout != "" {
					t.Errorf("run(%v) printed to stdout: %q", tt.args, stdout)
				}
			})
			// The parser must not print the error itself; main prints the
			// returned error exactly once.
			if stderr != "" {
				t.Errorf("run(%v) printed to stderr: %q", tt.args, stderr)
			}

			var flagsErr *flags.Error
			if !errors.As(err, &flagsErr) || flagsErr.Type != flags.ErrCommandRequired {
				t.Fatalf("run(%v) error = %v, want flags.ErrCommandRequired", tt.args, err)
			}
			if !strings.Contains(flagsErr.Message, "Usage:") {
				t.Errorf("run(%v) error does not include usage: %q", tt.args, flagsErr.Message)
			}
			for _, command := range tt.wantCommands {
				if !strings.Contains(flagsErr.Message, command) {
					t.Errorf("run(%v) error does not mention command %q: %q", tt.args, command, flagsErr.Message)
				}
			}
		})
	}
}

func TestParserAcceptsLaunchFlags(t *testing.T) {
	tests := []struct {
		name           string
		args           []string
		unwantedErrMsg string
	}{
		{
			name:           "resume no",
			args:           []string{"--manifest=/tmp/manifest.json", "launch", "--resume=no"},
			unwantedErrMsg: "Invalid value",
		},
		{
			name:           "resume auto",
			args:           []string{"--manifest=/tmp/manifest.json", "launch", "--resume=auto"},
			unwantedErrMsg: "Invalid value",
		},
		{
			name:           "resume force",
			args:           []string{"--manifest=/tmp/manifest.json", "launch", "--resume=force"},
			unwantedErrMsg: "Invalid value",
		},
		{
			name:           "ssh",
			args:           []string{"--manifest=/tmp/manifest.json", "launch", "--ssh"},
			unwantedErrMsg: "unknown flag `ssh'",
		},
		{
			name:           "verbose short",
			args:           []string{"--manifest=/tmp/manifest.json", "launch", "-v"},
			unwantedErrMsg: "unknown flag `v'",
		},
		{
			name:           "debug short",
			args:           []string{"--manifest=/tmp/manifest.json", "launch", "-vv"},
			unwantedErrMsg: "unknown flag `v'",
		},
		{
			name:           "verbose long",
			args:           []string{"--manifest=/tmp/manifest.json", "launch", "--verbose"},
			unwantedErrMsg: "unknown flag `verbose'",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := newParserForOptions(&Options{}).ParseArgs(tt.args)
			if err != nil && strings.Contains(err.Error(), tt.unwantedErrMsg) {
				t.Fatalf("ParseArgs(%v) rejected supported flag: %v", tt.args, err)
			}
		})
	}
}

func TestParserAcceptsSharedOptionsBeforeOrAfterSubcommand(t *testing.T) {
	manifestPath := filepath.Join(t.TempDir(), "manifest.toml")
	tests := []struct {
		name string
		args []string
	}{
		{
			name: "manifest root first",
			args: []string{"--manifest=" + manifestPath, "launch"},
		},
		{
			name: "manifest command after",
			args: []string{"launch", "--manifest=" + manifestPath},
		},
		{
			name: "verbose root first",
			args: []string{"--manifest=" + manifestPath, "-vv", "launch"},
		},
		{
			name: "verbose command after",
			args: []string{"launch", "-vv", "--manifest=" + manifestPath},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := run(tt.args)
			if err == nil {
				t.Fatalf("run(%v) succeeded, expected manifest load error", tt.args)
			}
			if strings.Contains(err.Error(), "unknown flag `manifest'") {
				t.Fatalf("run(%v) rejected supported manifest placement: %v", tt.args, err)
			}
			if strings.Contains(err.Error(), "unknown flag `v'") {
				t.Fatalf("run(%v) rejected supported verbose placement: %v", tt.args, err)
			}
			if !strings.Contains(err.Error(), `open manifest`) || !strings.Contains(err.Error(), manifestPath) {
				t.Fatalf("run(%v) did not parse manifest path, got %v", tt.args, err)
			}
		})
	}
}

func TestRunRPCStatusPrintsControlSocketResponse(t *testing.T) {
	tmpDir := t.TempDir()
	manifestPath := filepath.Join(tmpDir, "manifest.json")
	if err := os.WriteFile(manifestPath, []byte(testManifestJSON(tmpDir)), 0o644); err != nil {
		t.Fatalf("write manifest: %v", err)
	}
	cfg, err := loadManifest(manifestPath)
	if err != nil {
		t.Fatalf("load manifest: %v", err)
	}
	controlSocketPath, err := cfg.ResolvedControlSocketPath()
	if err != nil {
		t.Fatalf("resolve control socket: %v", err)
	}
	startMainTestControlServerAt(t, controlSocketPath, &mainTestControlCore{
		status: control.StatusResponse{
			State: control.RuntimeReady,
			CID:   7,
			Paths: control.StatusPaths{ControlSocket: controlSocketPath},
		},
	})

	output := captureStdout(t, func() {
		if err := run([]string{"--manifest=" + manifestPath, "rpc", "status"}); err != nil {
			t.Fatalf("rpc status: %v", err)
		}
	})

	var got control.StatusResponse
	if err := json.Unmarshal([]byte(output), &got); err != nil {
		t.Fatalf("decode rpc output %q: %v", output, err)
	}
	if got.State != control.RuntimeReady || got.CID != 7 || got.Paths.ControlSocket != controlSocketPath {
		t.Fatalf("unexpected rpc status output: %#v", got)
	}
}

func TestRunRPCUsesJSONParams(t *testing.T) {
	tmpDir := t.TempDir()
	manifestPath := filepath.Join(tmpDir, "manifest.json")
	if err := os.WriteFile(manifestPath, []byte(testManifestJSON(tmpDir)), 0o644); err != nil {
		t.Fatalf("write manifest: %v", err)
	}
	cfg, err := loadManifest(manifestPath)
	if err != nil {
		t.Fatalf("load manifest: %v", err)
	}
	controlSocketPath, err := cfg.ResolvedControlSocketPath()
	if err != nil {
		t.Fatalf("resolve control socket: %v", err)
	}
	handler := &mainTestControlHandler{}
	startMainTestControlServerAt(t, controlSocketPath, handler)

	output := captureStdout(t, func() {
		args := []string{"--manifest=" + manifestPath, "rpc", "hotplug", `{"id":"cache","detach":true}`}
		if err := run(args); err != nil {
			t.Fatalf("rpc hotplug: %v", err)
		}
	})

	if handler.hotplugReq.ID != "cache" || !handler.hotplugReq.Detach {
		t.Fatalf("unexpected hotplug request: %#v", handler.hotplugReq)
	}
	var got control.HotplugResponse
	if err := json.Unmarshal([]byte(output), &got); err != nil {
		t.Fatalf("decode rpc output %q: %v", output, err)
	}
	if got.ID != "cache" || !got.Detach {
		t.Fatalf("unexpected rpc hotplug output: %#v", got)
	}
}

func TestRunRPCMethodsPrintsAvailableControlSocketMethods(t *testing.T) {
	tmpDir := t.TempDir()
	manifestPath := filepath.Join(tmpDir, "manifest.json")
	if err := os.WriteFile(manifestPath, []byte(testManifestJSON(tmpDir)), 0o644); err != nil {
		t.Fatalf("write manifest: %v", err)
	}
	cfg, err := loadManifest(manifestPath)
	if err != nil {
		t.Fatalf("load manifest: %v", err)
	}
	controlSocketPath, err := cfg.ResolvedControlSocketPath()
	if err != nil {
		t.Fatalf("resolve control socket: %v", err)
	}
	handler := &mainTestControlHandler{}
	startMainTestControlServerAt(t, controlSocketPath, handler)

	output := captureStdout(t, func() {
		if err := run([]string{"--manifest=" + manifestPath, "rpc", "methods"}); err != nil {
			t.Fatalf("rpc methods: %v", err)
		}
	})

	var got control.MethodsResponse
	if err := json.Unmarshal([]byte(output), &got); err != nil {
		t.Fatalf("decode rpc output %q: %v", output, err)
	}
	want := []string{"status", "methods", "guest-ps", "guest-exec", "guest-read", "guest-write", "hotplug"}
	if !reflect.DeepEqual(got.Methods, want) {
		t.Fatalf("unexpected rpc methods output: got %#v want %#v", got.Methods, want)
	}
}

func TestResolveManifestPathDefaultsToTOMLThenJSON(t *testing.T) {
	tmpDir := t.TempDir()
	oldDir, err := os.Getwd()
	if err != nil {
		t.Fatalf("get cwd: %v", err)
	}
	defer func() {
		if err := os.Chdir(oldDir); err != nil {
			t.Fatalf("restore cwd: %v", err)
		}
	}()
	if err := os.Chdir(tmpDir); err != nil {
		t.Fatalf("chdir: %v", err)
	}

	jsonPath := filepath.Join(tmpDir, "manifest.json")
	if err := os.WriteFile(jsonPath, []byte("{}"), 0o644); err != nil {
		t.Fatalf("write json manifest: %v", err)
	}
	resolved, err := resolveManifestPath("")
	if err != nil {
		t.Fatalf("resolve json default: %v", err)
	}
	if resolved != jsonPath {
		t.Fatalf("unexpected json default: got %q want %q", resolved, jsonPath)
	}

	tomlPath := filepath.Join(tmpDir, "manifest.toml")
	if err := os.WriteFile(tomlPath, []byte(""), 0o644); err != nil {
		t.Fatalf("write toml manifest: %v", err)
	}
	resolved, err = resolveManifestPath("")
	if err != nil {
		t.Fatalf("resolve toml default: %v", err)
	}
	if resolved != tomlPath {
		t.Fatalf("unexpected toml default: got %q want %q", resolved, tomlPath)
	}
}

func TestResolveManifestPathRequiresExplicitOrDefaultManifest(t *testing.T) {
	tmpDir := t.TempDir()
	oldDir, err := os.Getwd()
	if err != nil {
		t.Fatalf("get cwd: %v", err)
	}
	defer func() {
		if err := os.Chdir(oldDir); err != nil {
			t.Fatalf("restore cwd: %v", err)
		}
	}()
	if err := os.Chdir(tmpDir); err != nil {
		t.Fatalf("chdir: %v", err)
	}

	_, err = resolveManifestPath("")
	if err == nil || !strings.Contains(err.Error(), "no manifest path provided") {
		t.Fatalf("expected missing manifest error, got %v", err)
	}
}

func TestLoadLaunchManifestResolvesWorkingDirAgainstProcessCWD(t *testing.T) {
	tmpDir := t.TempDir()
	manifestPath := filepath.Join(tmpDir, "manifest.json")
	original := []byte(testManifestJSON("."))
	if err := os.WriteFile(manifestPath, original, 0o644); err != nil {
		t.Fatalf("write manifest: %v", err)
	}

	// A relative working_dir follows the process working directory, so the
	// same manifest run from elsewhere takes that directory's relative paths.
	otherDir := t.TempDir()
	oldDir, err := os.Getwd()
	if err != nil {
		t.Fatalf("get cwd: %v", err)
	}
	defer func() {
		if err := os.Chdir(oldDir); err != nil {
			t.Fatalf("restore cwd: %v", err)
		}
	}()
	if err := os.Chdir(otherDir); err != nil {
		t.Fatalf("chdir: %v", err)
	}
	wantDir, err := os.Getwd()
	if err != nil {
		t.Fatalf("get cwd after chdir: %v", err)
	}

	loaded, err := loadLaunchManifest(manifestPath, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatalf("load launch manifest: %v", err)
	}
	if loaded.Paths.WorkingDir != wantDir {
		t.Fatalf("unexpected working dir: got %q want %q", loaded.Paths.WorkingDir, wantDir)
	}
	if loaded.Paths.WorkingDir == tmpDir {
		t.Fatalf("working dir must not anchor to the manifest directory %q", tmpDir)
	}

	// Loading must never write to the manifest, whatever the working dir.
	data, err := os.ReadFile(manifestPath)
	if err != nil {
		t.Fatalf("read manifest: %v", err)
	}
	if !bytes.Equal(data, original) {
		t.Fatalf("manifest file was modified by loading it")
	}
}

func TestLoadManifestLeavesReadOnlyManifestIntact(t *testing.T) {
	tmpDir := t.TempDir()
	manifestPath := filepath.Join(tmpDir, "manifest.json")
	original := []byte(testManifestJSON("."))
	if err := os.WriteFile(manifestPath, original, 0o400); err != nil {
		t.Fatalf("write manifest: %v", err)
	}

	// Every loader path must cope with a manifest the user made read-only.
	if _, err := loadManifest(manifestPath); err != nil {
		t.Fatalf("load manifest: %v", err)
	}
	if _, err := loadLaunchManifest(manifestPath, slog.New(slog.DiscardHandler)); err != nil {
		t.Fatalf("load launch manifest: %v", err)
	}

	info, err := os.Stat(manifestPath)
	if err != nil {
		t.Fatalf("stat manifest: %v", err)
	}
	if got := info.Mode().Perm(); got != 0o400 {
		t.Fatalf("manifest permissions changed: got %o want 400", got)
	}
	data, err := os.ReadFile(manifestPath)
	if err != nil {
		t.Fatalf("read manifest: %v", err)
	}
	if !bytes.Equal(data, original) {
		t.Fatalf("manifest file was modified by loading it")
	}
}

func testManifestJSON(workingDir string) string {
	return `{
  "host_name": "test-vm",
  "working_dir": "` + workingDir + `",
  "state_dir": ".agentspace",
  "ssh": {
    "exec": ["/bin/ssh"],
    "user": "agent"
  },
  "qemu": {
    "exec": ["/bin/qemu-system-x86_64"]
  },
  "machine": {
    "type": "microvm",
    "memory": 256,
    "vcpu": 1
  },
  "kernel": {
    "path": "/tmp/vmlinuz",
    "initrd_path": "/tmp/initrd"
  },
  "mounts": [
    {
      "type": "virtiofs",
      "tag": "workspace",
      "virtiofs": {
        "socket": "virtiofs.sock",
        "bin": "/bin/virtiofsd"
      }
    },
    {
      "type": "image",
      "source": "overlay.img"
    }
  ]
}
`
}

type mainTestControlCore struct {
	status control.StatusResponse
}

func (h *mainTestControlCore) Status(context.Context, control.StatusRequest) (control.StatusResponse, error) {
	return h.status, nil
}

type mainTestControlHandler struct {
	mainTestControlCore
	hotplugReq control.HotplugRequest
}

func (h *mainTestControlHandler) GuestPS(context.Context, control.GuestPSRequest) (control.GuestPSResponse, error) {
	return control.GuestPSResponse{ProcessList: "USER COMMAND\nroot init"}, nil
}

func (h *mainTestControlHandler) GuestExec(ctx context.Context, req control.GuestExecRequest) (control.GuestExecResponse, error) {
	return control.GuestExecResponse{Exited: true, ExitCode: 0, OutData: "b2sK"}, nil
}

func (h *mainTestControlHandler) GuestRead(ctx context.Context, req control.GuestReadRequest) (control.GuestReadResponse, error) {
	return control.GuestReadResponse{Path: req.Path, DataBase64: "b2sK"}, nil
}

func (h *mainTestControlHandler) GuestWrite(ctx context.Context, req control.GuestWriteRequest) (control.GuestWriteResponse, error) {
	return control.GuestWriteResponse{Path: req.Path}, nil
}

func (h *mainTestControlHandler) Hotplug(ctx context.Context, req control.HotplugRequest) (control.HotplugResponse, error) {
	h.hotplugReq = req
	return control.HotplugResponse{ID: req.ID, Detach: req.Detach}, nil
}

func startMainTestControlServerAt(t *testing.T, path string, runtime control.RuntimeCore) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("create control socket directory: %v", err)
	}
	handlers := control.Handlers{Core: runtime}
	if guest, ok := runtime.(control.RuntimeGuest); ok {
		handlers.Guest = guest
	}
	if hotplug, ok := runtime.(control.RuntimeHotplug); ok {
		handlers.Hotplug = hotplug
	}
	router, err := control.NewRouter(handlers)
	if err != nil {
		t.Fatalf("router: %v", err)
	}
	listener, err := control.Listen(path)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	server, err := control.NewServer(router)
	if err != nil {
		t.Fatalf("server: %v", err)
	}
	done := make(chan error, 1)
	go func() {
		done <- server.Serve(listener)
	}()
	t.Cleanup(func() {
		if err := listener.Close(); err != nil && !strings.Contains(err.Error(), "use of closed") {
			t.Errorf("close control socket: %v", err)
		}
		if err := <-done; err != nil {
			t.Errorf("serve control socket: %v", err)
		}
	})
}

func captureStdout(t *testing.T, fn func()) string {
	t.Helper()
	return captureFile(t, &os.Stdout, fn)
}

func captureStderr(t *testing.T, fn func()) string {
	t.Helper()
	return captureFile(t, &os.Stderr, fn)
}

func captureFile(t *testing.T, target **os.File, fn func()) string {
	t.Helper()
	original := *target
	readFile, writeFile, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	*target = writeFile
	defer func() {
		*target = original
	}()

	fn()

	if err := writeFile.Close(); err != nil {
		t.Fatalf("close pipe writer: %v", err)
	}
	data, err := io.ReadAll(readFile)
	if err != nil {
		t.Fatalf("read pipe: %v", err)
	}
	if err := readFile.Close(); err != nil {
		t.Fatalf("close pipe reader: %v", err)
	}
	return string(data)
}
