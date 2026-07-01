package executor

import (
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
)

func TestCommandLeavesEmptyEnvNil(t *testing.T) {
	cmd := Command("/bin/echo", []string{"hello"}, nil)
	if cmd.Env != nil {
		t.Fatalf("expected empty env to leave command env nil, got %#v", cmd.Env)
	}
}

func TestCommandAppendsEnvAfterEnviron(t *testing.T) {
	additions := []string{"VIRTIE_TEST_ONE=1", "VIRTIE_TEST_TWO=2"}
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
	additions := []string{"VIRTIE_TEST_ONE=1", "VIRTIE_TEST_TWO=2"}
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
