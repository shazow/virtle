package launch

import (
	"os/exec"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/shazow/virtle/internal/manifest"
)

func TestBuildSSHCommandBuildsInteractiveSession(t *testing.T) {
	launchManifest := &manifest.Manifest{
		Paths: manifest.Paths{
			WorkingDir: "/tmp/work",
		},
		SSH: manifest.SSH{
			Argv: []string{
				"/bin/ssh",
				"-q",
				"-o",
				"StrictHostKeyChecking=no",
			},
			User: "agent",
		},
	}

	session, err := buildSSHCommand(launchManifest, 10, []string{"bash", "-lc", "echo hi"})
	if err != nil {
		t.Fatalf("build ssh command: %v", err)
	}
	wantArgs := []string{
		"-tt",
		"-q",
		"-o",
		"StrictHostKeyChecking=no",
		"agent@vsock/10",
		"bash -lc 'echo hi'",
	}
	if !reflect.DeepEqual(commandArgs(session), wantArgs) {
		t.Fatalf("unexpected ssh session args: got %v want %v", commandArgs(session), wantArgs)
	}
}

func TestBuildSSHCommandShellQuotesRemoteCommand(t *testing.T) {
	launchManifest := &manifest.Manifest{
		Paths: manifest.Paths{WorkingDir: "/tmp/work"},
		SSH: manifest.SSH{
			Argv: []string{"/bin/ssh"},
			User: "agent",
		},
	}

	session, err := buildSSHCommand(launchManifest, 10, []string{"printf", "%s\n", "it's $HOME", ""})
	if err != nil {
		t.Fatalf("build ssh command: %v", err)
	}
	want := "printf '%s\n' 'it'\\''s $HOME' ''"
	if got := commandArgs(session)[len(commandArgs(session))-1]; got != want {
		t.Fatalf("unexpected quoted remote command: got %q want %q", got, want)
	}

	configuredCommand, err := buildSSHCommand(launchManifest, 10, []string{"tmux new-session -A -s codex \"npx @openai/codex --yolo\""})
	if err != nil {
		t.Fatalf("build configured ssh command: %v", err)
	}
	wantConfigured := "tmux new-session -A -s codex \"npx @openai/codex --yolo\""
	if got := configuredCommand.Args[len(configuredCommand.Args)-1]; got != wantConfigured {
		t.Fatalf("unexpected configured remote command: got %q want %q", got, wantConfigured)
	}
}

func TestBuildSSHCommandRendersManifestExecTemplates(t *testing.T) {
	launchManifest := &manifest.Manifest{
		Paths: manifest.Paths{WorkingDir: "/tmp/work"},
		SSH: manifest.SSH{
			Argv: []string{"/bin/ssh", "-S", "control-{{.CID}}", "-o", "HostName={{.Destination}}"},
			User: "agent",
		},
	}

	session, err := buildSSHCommand(launchManifest, 10, nil)
	if err != nil {
		t.Fatalf("build ssh command: %v", err)
	}

	for _, want := range []string{"CID=10", "USER=agent", "DESTINATION=agent@vsock/10"} {
		if !slices.Contains(commandEnvAdditions(session.Env), want) {
			t.Fatalf("expected ssh env %q in %#v", want, session.Env)
		}
	}
	if !slices.Contains(commandArgs(session), "control-10") || !slices.Contains(commandArgs(session), "HostName=agent@vsock/10") {
		t.Fatalf("expected rendered ssh args, got %#v", commandArgs(session))
	}
	hint, err := BuildSSHCommandHint(launchManifest, 10)
	if err != nil {
		t.Fatalf("build ssh command hint: %v", err)
	}
	if got, want := hint, "/bin/ssh -S control-10 -o HostName=agent@vsock/10 agent@vsock/10"; got != want {
		t.Fatalf("unexpected rendered ssh hint: got %q want %q", got, want)
	}
}

func TestBuildSSHCommandHintReturnsTemplateError(t *testing.T) {
	launchManifest := &manifest.Manifest{
		SSH: manifest.SSH{
			Argv: []string{"/bin/ssh", "{{.Missing}}"},
			User: "agent",
		},
	}

	_, err := BuildSSHCommandHint(launchManifest, 10)
	if err == nil || !strings.Contains(err.Error(), `map has no entry for key "Missing"`) {
		t.Fatalf("expected ssh hint template error, got %v", err)
	}
}

func commandArgs(cmd *exec.Cmd) []string {
	if cmd == nil {
		return nil
	}
	return append([]string(nil), cmd.Args[1:]...)
}

func commandEnvAdditions(env []string) []string {
	var additions []string
	for _, entry := range env {
		if strings.HasPrefix(entry, "CID=") || strings.HasPrefix(entry, "USER=") || strings.HasPrefix(entry, "DESTINATION=") {
			additions = append(additions, entry)
		}
	}
	return additions
}

// buildSSHCommand is a test-only convenience using the manifest's own argv.
func buildSSHCommand(launchManifest *manifest.Manifest, cid int, remoteCommand []string) (*exec.Cmd, error) {
	return buildSSHCommandWithArgv(launchManifest, cid, remoteCommand, launchManifest.SSH.Argv)
}
