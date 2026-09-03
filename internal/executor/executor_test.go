package executor

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestCommandLeavesEmptyEnvNil(t *testing.T) {
	cmd := Command("/bin/echo", []string{"hello"}, nil)
	if cmd.Env != nil {
		t.Fatalf("expected empty env to leave command env nil, got %#v", cmd.Env)
	}
}

func TestCommandAppendsEnvAfterEnviron(t *testing.T) {
	additions := []string{"VIRTLE_TEST_ONE=1", "VIRTLE_TEST_TWO=2"}
	cmd := Command("/bin/echo", nil, additions)
	if !slices.Equal(cmd.Env, WrapEnv(additions)) {
		t.Fatalf("unexpected env: got %#v want %#v", cmd.Env, WrapEnv(additions))
	}
}

func TestWrapEnv(t *testing.T) {
	if env := WrapEnv(nil); env != nil {
		t.Fatalf("empty env: got %#v want nil", env)
	}

	environ := os.Environ()
	additions := []string{"VIRTLE_TEST_ONE=1", "VIRTLE_TEST_TWO=2"}
	env := WrapEnv(additions)
	if len(env) != len(environ)+len(additions) {
		t.Fatalf("unexpected env length: got %d want %d", len(env), len(environ)+len(additions))
	}
	if !slices.Equal(env[:len(environ)], environ) {
		t.Fatalf("expected env to start with os.Environ()")
	}
	if !slices.Equal(env[len(environ):], additions) {
		t.Fatalf("unexpected appended env: got %#v want %#v", env[len(environ):], additions)
	}
}

func TestCommandPassesArgsAfterArgv0(t *testing.T) {
	cmd := Command("/bin/echo", []string{"hello", "world"}, nil)
	if !slices.Equal(cmd.Args, []string{"/bin/echo", "hello", "world"}) {
		t.Fatalf("unexpected args: %#v", cmd.Args)
	}
}

func TestRunnerStartsCommand(t *testing.T) {
	if os.Getenv("EXECUTOR_RUNNER_CHILD") == "1" {
		os.Exit(0)
	}

	exe, err := os.Executable()
	if err != nil {
		t.Fatalf("resolve test executable: %v", err)
	}
	cmd := exec.Command(exe, "-test.run=TestRunnerStartsCommand")
	cmd.Env = append(os.Environ(), "EXECUTOR_RUNNER_CHILD=1")
	process, err := (&Runner{}).Start(cmd)
	if err != nil {
		t.Fatalf("start child: %v", err)
	}
	if process.PID() == 0 {
		t.Fatalf("expected started process to have a pid")
	}
	if got, want := process.Name(), filepath.Base(exe); got != want {
		t.Fatalf("unexpected process name: got %q want %q", got, want)
	}
	if err := process.Wait(); err != nil {
		t.Fatalf("wait child: %v", err)
	}
}

func TestRunnerStopsProcessGroup(t *testing.T) {
	const helperEnv = "EXECUTOR_PROCESS_GROUP_CHILD"
	if role := os.Getenv(helperEnv); role != "" {
		runProcessGroupHelper(helperEnv, role)
		return
	}

	tests := []struct {
		name       string
		role       string
		stopCtx    func() context.Context
		wantStatus string
	}{
		{
			name:       "graceful",
			role:       "parent-graceful",
			stopCtx:    context.Background,
			wantStatus: "dp",
		},
		{
			name: "forced",
			role: "parent-forced",
			stopCtx: func() context.Context {
				ctx, cancel := context.WithCancel(context.Background())
				cancel()
				return ctx
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			statusRead, statusWrite, err := os.Pipe()
			if err != nil {
				t.Fatalf("create status pipe: %v", err)
			}
			defer statusRead.Close()

			cmd := exec.Command(os.Args[0], "-test.run=^TestRunnerStopsProcessGroup$")
			cmd.Env = replaceEnv(os.Environ(), helperEnv, tt.role)
			cmd.ExtraFiles = []*os.File{statusWrite}
			cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
			process, err := (&Runner{}).Start(cmd)
			if err != nil {
				statusWrite.Close()
				t.Fatalf("start process group helper: %v", err)
			}
			if err := statusWrite.Close(); err != nil {
				t.Fatalf("close parent status writer: %v", err)
			}
			defer func() {
				_ = syscall.Kill(-process.PID(), syscall.SIGKILL)
				_ = process.Wait()
			}()

			ready := readPipe(t, statusRead, 2)
			if !strings.Contains(ready, "P") || !strings.Contains(ready, "D") {
				t.Fatalf("unexpected readiness status %q", ready)
			}

			process.SetGracePeriod(2 * time.Second)
			if err := process.Stop(tt.stopCtx()); err != nil {
				t.Fatalf("stop process group: %v", err)
			}
			if got := readPipe(t, statusRead, -1); got != tt.wantStatus {
				t.Fatalf("unexpected exit status: got %q want %q", got, tt.wantStatus)
			}
			if err := process.Stop(context.Background()); err != nil {
				t.Fatalf("repeat stop: %v", err)
			}
		})
	}
}

func TestSignalProcessGroupFallsBackToProcess(t *testing.T) {
	const helperEnv = "EXECUTOR_PROCESS_GROUP_CHILD"
	statusRead, statusWrite, err := os.Pipe()
	if err != nil {
		t.Fatalf("create status pipe: %v", err)
	}
	defer statusRead.Close()

	cmd := exec.Command(os.Args[0], "-test.run=^TestRunnerStopsProcessGroup$")
	cmd.Env = replaceEnv(os.Environ(), helperEnv, "descendant-graceful")
	cmd.ExtraFiles = []*os.File{statusWrite}
	process, err := (&Runner{}).Start(cmd)
	if err != nil {
		statusWrite.Close()
		t.Fatalf("start fallback helper: %v", err)
	}
	if err := statusWrite.Close(); err != nil {
		t.Fatalf("close parent status writer: %v", err)
	}
	defer func() {
		_ = process.Kill()
		_ = process.Wait()
	}()

	if got := readPipe(t, statusRead, 1); got != "D" {
		t.Fatalf("unexpected readiness status %q", got)
	}
	if err := SignalProcessGroup(process.PID(), syscall.SIGTERM); err != nil {
		t.Fatalf("signal process group with fallback: %v", err)
	}
	if err := process.Wait(); err != nil {
		t.Fatalf("wait fallback helper: %v", err)
	}
	if got := readPipe(t, statusRead, -1); got != "d" {
		t.Fatalf("unexpected exit status %q", got)
	}
}

func runProcessGroupHelper(helperEnv, role string) {
	status := os.NewFile(3, "status")
	if status == nil {
		fmt.Fprintln(os.Stderr, "status descriptor is unavailable")
		os.Exit(2)
	}
	defer status.Close()

	if strings.HasPrefix(role, "descendant-") {
		signals := make(chan os.Signal, 1)
		signal.Notify(signals, syscall.SIGTERM)
		defer signal.Stop(signals)
		if _, err := status.Write([]byte("D")); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(2)
		}
		<-signals
		if _, err := status.Write([]byte("d")); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(2)
		}
		return
	}

	mode, ok := strings.CutPrefix(role, "parent-")
	if !ok {
		fmt.Fprintf(os.Stderr, "unknown process group helper role %q\n", role)
		os.Exit(2)
	}
	signals := make(chan os.Signal, 1)
	signal.Notify(signals, syscall.SIGTERM)
	defer signal.Stop(signals)

	descendant := exec.Command(os.Args[0], "-test.run=^TestRunnerStopsProcessGroup$")
	descendant.Env = replaceEnv(os.Environ(), helperEnv, "descendant-"+mode)
	descendant.ExtraFiles = []*os.File{status}
	if err := descendant.Start(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	if _, err := status.Write([]byte("P")); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	<-signals
	if mode == "forced" {
		<-make(chan struct{})
	}
	if err := descendant.Wait(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	if _, err := status.Write([]byte("p")); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
}

func replaceEnv(env []string, key, value string) []string {
	prefix := key + "="
	replaced := make([]string, 0, len(env)+1)
	for _, entry := range env {
		if !strings.HasPrefix(entry, prefix) {
			replaced = append(replaced, entry)
		}
	}
	return append(replaced, prefix+value)
}

func readPipe(t *testing.T, reader io.Reader, size int) string {
	t.Helper()
	type result struct {
		data []byte
		err  error
	}
	resultCh := make(chan result, 1)
	go func() {
		var data []byte
		var err error
		if size < 0 {
			data, err = io.ReadAll(reader)
		} else {
			data = make([]byte, size)
			_, err = io.ReadFull(reader, data)
		}
		resultCh <- result{data: data, err: err}
	}()
	select {
	case result := <-resultCh:
		if result.err != nil {
			t.Fatalf("read process status: %v", result.err)
		}
		return string(result.data)
	case <-time.After(10 * time.Second):
		t.Fatal("timed out reading process status")
		return ""
	}
}

func TestRunnerLogsUnsetCommandStreamsAtDebug(t *testing.T) {
	if os.Getenv("EXECUTOR_STREAM_CHILD") == "1" {
		fmt.Fprint(os.Stdout, "stdout line\nstdout partial")
		fmt.Fprintln(os.Stderr, "stderr line")
		os.Exit(0)
	}

	exe, err := os.Executable()
	if err != nil {
		t.Fatalf("resolve test executable: %v", err)
	}
	var logs bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	cmd := exec.Command(exe, "-test.run=TestRunnerLogsUnsetCommandStreamsAtDebug")
	cmd.Env = append(os.Environ(), "EXECUTOR_STREAM_CHILD=1")
	process, err := (&Runner{Logger: logger}).Start(cmd)
	if err != nil {
		t.Fatalf("start child: %v", err)
	}
	if err := process.Wait(); err != nil {
		t.Fatalf("wait child: %v", err)
	}

	output := logs.String()
	for _, want := range []string{
		"level=DEBUG",
		"command=" + filepath.Base(exe),
		"stream=stdout",
		`msg="stdout line"`,
		`msg="stdout partial"`,
		"stream=stderr",
		`msg="stderr line"`,
	} {
		if !strings.Contains(output, want) {
			t.Fatalf("expected command logs to contain %q, got %q", want, output)
		}
	}
}

func TestRunnerPreservesExplicitCommandStreams(t *testing.T) {
	if os.Getenv("EXECUTOR_EXPLICIT_STREAM_CHILD") == "1" {
		fmt.Fprint(os.Stdout, "stdout")
		fmt.Fprint(os.Stderr, "stderr")
		os.Exit(0)
	}

	exe, err := os.Executable()
	if err != nil {
		t.Fatalf("resolve test executable: %v", err)
	}
	var stdout, stderr, logs bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	cmd := exec.Command(exe, "-test.run=TestRunnerPreservesExplicitCommandStreams")
	cmd.Env = append(os.Environ(), "EXECUTOR_EXPLICIT_STREAM_CHILD=1")
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	process, err := (&Runner{Logger: logger}).Start(cmd)
	if err != nil {
		t.Fatalf("start child: %v", err)
	}
	if err := process.Wait(); err != nil {
		t.Fatalf("wait child: %v", err)
	}
	if got, want := stdout.String(), "stdout"; got != want {
		t.Fatalf("explicit stdout: got %q want %q", got, want)
	}
	if got, want := stderr.String(), "stderr"; got != want {
		t.Fatalf("explicit stderr: got %q want %q", got, want)
	}
	if logs.Len() != 0 {
		t.Fatalf("expected no command logs for explicit streams, got %q", logs.String())
	}
}

func TestRunnerLeavesUnsetStreamsDisabledWithoutDebug(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelInfo}))
	cmd := exec.Command(os.Args[0], "-test.run=^$")
	process, err := (&Runner{Logger: logger}).Start(cmd)
	if err != nil {
		t.Fatalf("start child: %v", err)
	}
	if cmd.Stdout != nil || cmd.Stderr != nil {
		t.Fatal("expected disabled debug streams to remain unset")
	}
	if err := process.Wait(); err != nil {
		t.Fatalf("wait child: %v", err)
	}
}

func TestLineLoggerBoundsUnterminatedOutput(t *testing.T) {
	var logs bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	output := &lineLogger{logger: logger}
	line := strings.Repeat("x", maxLogLine+1)
	if n, err := output.Write([]byte(line)); err != nil || n != len(line) {
		t.Fatalf("write: n=%d err=%v", n, err)
	}
	output.close()
	records := strings.Split(strings.TrimSpace(logs.String()), "\n")
	if got, want := len(records), 2; got != want {
		t.Fatalf("debug records: got %d want %d", got, want)
	}
	messages := make([]string, 0, len(records))
	for _, record := range records {
		_, message, ok := strings.Cut(record, " msg=")
		if !ok {
			t.Fatalf("debug record has no message: %q", record)
		}
		messages = append(messages, message)
	}
	if got, want := len(messages[0]), maxLogLine; got != want {
		t.Fatalf("first record length: got %d want %d", got, want)
	}
	if got, want := len(messages[1]), 1; got != want {
		t.Fatalf("second record length: got %d want %d", got, want)
	}
	if got := strings.Join(messages, ""); got != line {
		t.Fatal("debug records did not preserve command output")
	}
}

func TestRunnerRejectsNilCommand(t *testing.T) {
	_, err := (&Runner{}).Start(nil)
	if err == nil || !strings.Contains(err.Error(), "command must not be nil") {
		t.Fatalf("expected nil command error, got %v", err)
	}
}

func TestProcessNameFallsBackToCommandPath(t *testing.T) {
	process := &execCmdHandle{cmd: &exec.Cmd{Path: "/tmp/bin/custom"}}
	if got, want := process.Name(), "custom"; got != want {
		t.Fatalf("unexpected process name: got %q want %q", got, want)
	}
}

func TestRenderArgvAndEnv(t *testing.T) {
	renderer, err := NewWithEnviron(Context{
		"Host": "127.0.0.1",
		"Port": "22",
	}, []string{"USER=template-user"})
	if err != nil {
		t.Fatalf("new renderer: %v", err)
	}

	argv, err := renderer.RenderArgv([]string{
		"socat",
		"-",
		"TCP:{{.Host}}:{{.Port}}",
		"--user={{.Env.USER}}",
	})
	if err != nil {
		t.Fatalf("render argv: %v", err)
	}

	if got, want := argv, []string{"socat", "-", "TCP:127.0.0.1:22", "--user=template-user"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("unexpected argv: got %#v want %#v", got, want)
	}
	if got, want := renderer.Env(), []string{"HOST=127.0.0.1", "PORT=22"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("unexpected env: got %#v want %#v", got, want)
	}
}

func TestRendererCopiesInputsAndOutputs(t *testing.T) {
	context := Context{"Host": "127.0.0.1"}
	environ := []string{"USER=template-user"}
	renderer, err := NewWithEnviron(context, environ)
	if err != nil {
		t.Fatalf("new renderer: %v", err)
	}
	context["Host"] = "192.0.2.1"
	environ[0] = "USER=changed"

	value, err := renderer.RenderString("{{.Host}} {{.Env.USER}}")
	if err != nil {
		t.Fatalf("render string: %v", err)
	}
	if got, want := value, "127.0.0.1 template-user"; got != want {
		t.Fatalf("unexpected rendered value: got %q want %q", got, want)
	}

	env := renderer.Env()
	env[0] = "HOST=changed"
	if got, want := renderer.Env(), []string{"HOST=127.0.0.1"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("unexpected env after mutating copy: got %#v want %#v", got, want)
	}
}

func TestRendererEnvLookupUsesLastValue(t *testing.T) {
	renderer, err := NewWithEnviron(nil, []string{"USER=first", "USER=last"})
	if err != nil {
		t.Fatalf("new renderer: %v", err)
	}
	value, err := renderer.RenderString("{{.Env.USER}}")
	if err != nil {
		t.Fatalf("render string: %v", err)
	}
	if got, want := value, "last"; got != want {
		t.Fatalf("unexpected rendered value: got %q want %q", got, want)
	}
}

func TestNewRejectsReservedKeys(t *testing.T) {
	tests := []struct {
		name      string
		context   Context
		wantError string
	}{
		{
			name:      "empty",
			context:   Context{"": "value"},
			wantError: "must not be empty",
		},
		{
			name:      "env",
			context:   Context{"Env": "value"},
			wantError: `key "Env" is reserved`,
		},
		{
			name:      "contains equals",
			context:   Context{"BAD=KEY": "value"},
			wantError: `key "BAD=KEY" must not contain '='`,
		},
		{
			name:      "no env name",
			context:   Context{"---": "value"},
			wantError: `key "---" does not produce an environment name`,
		},
		{
			name:      "collision",
			context:   Context{"vmStatePath": "camel", "vm_state_path": "snake"},
			wantError: `both produce environment name "VM_STATE_PATH"`,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := NewWithEnviron(test.context, nil)
			if err == nil || !strings.Contains(err.Error(), test.wantError) {
				t.Fatalf("expected error containing %q, got %v", test.wantError, err)
			}
		})
	}
}

func TestRenderRejectsMissingTemplateKey(t *testing.T) {
	renderer, err := New(nil)
	if err != nil {
		t.Fatalf("new renderer: %v", err)
	}
	_, err = renderer.RenderArgv([]string{"echo", "{{.Missing}}"})
	if err == nil ||
		!strings.Contains(err.Error(), `exec[1] "{{.Missing}}"`) ||
		!strings.Contains(err.Error(), `map has no entry for key "Missing"`) {
		t.Fatalf("expected missing key error, got %v", err)
	}
}

func TestRenderRejectsInvalidTemplate(t *testing.T) {
	renderer, err := New(nil)
	if err != nil {
		t.Fatalf("new renderer: %v", err)
	}
	_, err = renderer.RenderArgv([]string{"echo", "{{"})
	if err == nil ||
		!strings.Contains(err.Error(), `exec[1] "{{"`) ||
		!strings.Contains(err.Error(), "unclosed action") {
		t.Fatalf("expected template parse error, got %v", err)
	}
}

// TestMain drops the race detector's one-second exit sleep for the helper
// processes these tests spawn from the test binary (GORACE is read at process
// start, so the parent binary keeps its own).
func TestMain(m *testing.M) {
	os.Setenv("GORACE", "atexit_sleep_ms=0")
	os.Exit(m.Run())
}
