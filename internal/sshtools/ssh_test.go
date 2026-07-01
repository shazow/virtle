package sshtools

import (
	"bytes"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestBuildArgsAndHint(t *testing.T) {
	cfg := Config{Exec: []string{"/bin/ssh", "-q"}, User: "agent"}
	command, err := NewCommand(cfg, 10, []string{"bash", "-lc", "echo hi"})
	if err != nil {
		t.Fatalf("NewCommand: %v", err)
	}
	if command.Path != "/bin/ssh" {
		t.Fatalf("unexpected command path %q", command.Path)
	}
	if got, want := command.String(), "/bin/ssh -tt -q agent@vsock/10 'bash -lc '\\''echo hi'\\'''"; got != want {
		t.Fatalf("unexpected command string: got %q want %q", got, want)
	}

	path, args := BuildArgs(cfg, 10, []string{"bash", "-lc", "echo hi"})
	if path != "/bin/ssh" {
		t.Fatalf("unexpected path %q", path)
	}
	wantArgs := []string{"-tt", "-q", "agent@vsock/10", "bash -lc 'echo hi'"}
	if !reflect.DeepEqual(args, wantArgs) {
		t.Fatalf("unexpected args: got %v want %v", args, wantArgs)
	}
	if got, want := CommandHint(cfg, 10), "/bin/ssh -q agent@vsock/10"; got != want {
		t.Fatalf("unexpected hint: got %q want %q", got, want)
	}
	if got, want := cfg.Destination(10), "agent@vsock/10"; got != want {
		t.Fatalf("unexpected destination: got %q want %q", got, want)
	}
}

func TestNewCommandRejectsEmptyExec(t *testing.T) {
	if _, err := NewCommand(Config{User: "agent"}, 10, nil); err == nil {
		t.Fatalf("expected empty exec error")
	}
	path, args := BuildArgs(Config{User: "agent"}, 10, nil)
	if path != "" || args != nil {
		t.Fatalf("expected empty BuildArgs result, got path=%q args=%v", path, args)
	}
}

func TestBuildArgsQuotesTildeRemoteArguments(t *testing.T) {
	cfg := Config{Exec: []string{"/bin/ssh"}, User: "agent"}
	_, args := BuildArgs(cfg, 10, []string{"printf", "%s\n", "~", "~/file", "~user/file"})
	want := "printf '%s\n' \\~ \\~/file \\~user/file"
	if got := args[len(args)-1]; got != want {
		t.Fatalf("unexpected quoted remote command: got %q want %q", got, want)
	}
}

func TestWithIdentity(t *testing.T) {
	got := WithIdentity([]string{"/bin/ssh"}, "/tmp/id")
	want := []string{"/bin/ssh", "-i", "/tmp/id", "-o", "IdentitiesOnly=yes"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("unexpected argv: got %v want %v", got, want)
	}

	cfg := Config{Exec: []string{"/bin/ssh"}, User: "agent"}.WithIdentity("/tmp/id")
	if !reflect.DeepEqual(cfg.Exec, want) {
		t.Fatalf("unexpected config argv: got %v want %v", cfg.Exec, want)
	}
}

func TestClassifyFailureAndPhase(t *testing.T) {
	if got := ClassifyFailure(assertErr("exit"), "Permission denied (publickey)."); got != FailureAuthentication {
		t.Fatalf("unexpected auth classification: %v", got)
	}
	if got := ClassifyFailure(assertErr("exit"), "Connection refused"); got != FailureTransient {
		t.Fatalf("unexpected transient classification: %v", got)
	}
	if got := RetryPhaseForFailure(nil, "Connection closed by remote host"); got != RetryPhaseConnecting {
		t.Fatalf("unexpected connecting phase: %v", got)
	}
	if got := RetryPhaseForFailure(nil, "No route to host"); got != RetryPhaseWaiting {
		t.Fatalf("unexpected waiting phase: %v", got)
	}
}

type assertErr string

func (e assertErr) Error() string { return string(e) }

func TestRetryLoggerLogsPhasesOnceAndWarnsAfterRepeatedFailures(t *testing.T) {
	var output bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&output, &slog.HandlerOptions{Level: slog.LevelDebug}))
	retryLog := NewRetryLogger(logger)

	retryLog.Log(nil, "Connection refused")
	retryLog.Log(nil, "Connection timed out")
	retryLog.Log(nil, "Connection closed by remote host")
	retryLog.Log(nil, "Connection reset by peer")
	retryLog.Log(nil, "Connection refused")
	retryLog.Log(nil, "permission denied")

	logs := output.String()
	if got := strings.Count(logs, "msg=\"waiting for ssh connection\""); got != 1 {
		t.Fatalf("expected waiting phase once, got %d logs:\n%s", got, logs)
	}
	if got := strings.Count(logs, "msg=\"connecting ssh\""); got != 1 {
		t.Fatalf("expected connecting phase once, got %d logs:\n%s", got, logs)
	}
	if got := strings.Count(logs, "msg=\"ssh exec failed 5 times; ensure the guest is reachable and credentials are configured\""); got != 1 {
		t.Fatalf("expected repeated failure warning once, got %d logs:\n%s", got, logs)
	}
}

func TestRetryOutput(t *testing.T) {
	var output bytes.Buffer
	stderr := NewRetryOutput(&output, false, time.Hour)
	_, _ = stderr.Write([]byte("Connection timed out\n"))
	if !strings.Contains(stderr.String(), "Connection timed out") {
		t.Fatalf("expected retry output capture, got %q", stderr.String())
	}
	stderr.Suppress()
	stderr.Flush()
	if output.String() != "" {
		t.Fatalf("expected suppressed output, got %q", output.String())
	}

	var flushed bytes.Buffer
	stderr = NewRetryOutput(&flushed, false, time.Hour)
	_, _ = stderr.Write([]byte("fatal\n"))
	stderr.Flush()
	if got, want := flushed.String(), "fatal\n"; got != want {
		t.Fatalf("unexpected flushed output: got %q want %q", got, want)
	}

	var verbose bytes.Buffer
	stderr = NewRetryOutput(&verbose, true, time.Hour)
	_, _ = stderr.Write([]byte("verbose\n"))
	if got, want := verbose.String(), "verbose\n"; got != want {
		t.Fatalf("unexpected verbose output: got %q want %q", got, want)
	}
}

func TestKeyStoreEnsureGeneratesReusesAndRepairs(t *testing.T) {
	dir := t.TempDir()
	store := KeyStore{Dir: dir, Comment: "test-key"}
	key, err := store.Ensure()
	if err != nil {
		t.Fatalf("Ensure generate: %v", err)
	}
	if key.IdentityFile != filepath.Join(dir, "id_ed25519") {
		t.Fatalf("unexpected identity path %q", key.IdentityFile)
	}
	if !strings.HasPrefix(key.AuthorizedKey, "ssh-ed25519 ") {
		t.Fatalf("unexpected authorized key %q", key.AuthorizedKey)
	}
	if info, err := os.Stat(key.IdentityFile); err != nil {
		t.Fatalf("stat identity: %v", err)
	} else if got, want := info.Mode().Perm(), os.FileMode(0o600); got != want {
		t.Fatalf("unexpected identity mode: got %v want %v", got, want)
	}

	reused, err := store.Ensure()
	if err != nil {
		t.Fatalf("Ensure reuse: %v", err)
	}
	if reused.AuthorizedKey != key.AuthorizedKey {
		t.Fatalf("expected key reuse")
	}

	if err := os.WriteFile(key.PublicKeyFile, nil, 0o644); err != nil {
		t.Fatalf("empty public key: %v", err)
	}
	repaired, err := store.Ensure()
	if err != nil {
		t.Fatalf("Ensure repair: %v", err)
	}
	if repaired.AuthorizedKey != key.AuthorizedKey {
		t.Fatalf("expected repaired key to match original")
	}
}

func TestAuthorizedKeysInstallPlan(t *testing.T) {
	plan := NewAuthorizedKeysInstallPlan("agent", "ssh-ed25519 abc")
	if got, want := plan.AuthorizedKeysPath, "/home/agent/.ssh/authorized_keys"; got != want {
		t.Fatalf("unexpected authorized_keys path: got %q want %q", got, want)
	}
	if got, want := plan.Owner, "agent:users"; got != want {
		t.Fatalf("unexpected owner: got %q want %q", got, want)
	}
	if !strings.Contains(plan.AppendScript, "grep -qxF") {
		t.Fatalf("append plan missing idempotent grep: %q", plan.AppendScript)
	}
	command := plan.AppendCommand("/bin/sh")
	if got, want := command.Path, "/bin/sh"; got != want {
		t.Fatalf("unexpected append command path: got %q want %q", got, want)
	}
	if got, want := command.Args, []string{"-c", plan.AppendScript, "virtie-ssh-autoprovision", plan.AuthorizedKeysPath, plan.TempKeyPath}; !reflect.DeepEqual(got, want) {
		t.Fatalf("unexpected append command args: got %v want %v", got, want)
	}
	if got, want := command.InputPath, plan.AuthorizedKeysPath; got != want {
		t.Fatalf("unexpected append command input path: got %q want %q", got, want)
	}
	if got, want := AuthorizedKeysPath("root"), "/root/.ssh/authorized_keys"; got != want {
		t.Fatalf("unexpected root authorized_keys path: got %q want %q", got, want)
	}
}
