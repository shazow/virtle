// Package executor renders exec command templates and manages external processes.
package executor

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"text/template"
	"unicode"
)

// Context holds named values available to exec command templates.
type Context map[string]any

// Command builds an external command with virtle's environment inheritance rules.
func Command(path string, args []string, env []string) *exec.Cmd {
	cmd := exec.Command(path, args...)
	cmd.Env = WrapEnv(env)
	return cmd
}

// WrapEnv returns env additions appended after the current process environment.
func WrapEnv(additions []string) []string {
	if len(additions) == 0 {
		return nil
	}
	env := os.Environ()
	return append(env, additions...)
}

// RunningProcess is an already-started process that can be wrapped by Process.
type RunningProcess interface {
	Wait() error
	Signal(sig os.Signal) error
	Kill() error
	PID() int
	Name() string
}

// Runner starts external commands and returns process handles.
type Runner struct{}

// Start starts cmd and returns its process handle.
func (r *Runner) Start(cmd *exec.Cmd) (*Process, error) {
	if cmd == nil {
		return nil, fmt.Errorf("start command: command must not be nil")
	}
	handle := &execCmdHandle{cmd: cmd}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start %s: %w", handle.Name(), err)
	}

	return Wrap(handle), nil
}

type execCmdHandle struct {
	cmd *exec.Cmd
}

func (p *execCmdHandle) Wait() error {
	return p.cmd.Wait()
}

func (p *execCmdHandle) Signal(sig os.Signal) error {
	if p.cmd.Process == nil {
		return nil
	}
	return p.cmd.Process.Signal(sig)
}

func (p *execCmdHandle) Kill() error {
	if p.cmd.Process == nil {
		return nil
	}
	return p.cmd.Process.Kill()
}

func (p *execCmdHandle) PID() int {
	if p.cmd.Process == nil {
		return 0
	}
	return p.cmd.Process.Pid
}

func (p *execCmdHandle) Name() string {
	if p == nil || p.cmd == nil {
		return ""
	}
	if len(p.cmd.Args) > 0 && p.cmd.Args[0] != "" {
		return filepath.Base(p.cmd.Args[0])
	}
	if p.cmd.Path != "" {
		return filepath.Base(p.cmd.Path)
	}
	return "command"
}

// Renderer renders exec command templates from a fixed context.
type Renderer struct {
	data map[string]any
	env  []string
}

// New returns a Renderer that uses the current process environment for Env lookups.
func New(context Context) (*Renderer, error) {
	return NewWithEnviron(context, os.Environ())
}

// NewWithEnviron returns a Renderer that uses environ for Env lookups.
func NewWithEnviron(context Context, environ []string) (*Renderer, error) {
	env, err := contextEnv(context)
	if err != nil {
		return nil, err
	}
	data := make(map[string]any, len(context)+1)
	for key, value := range context {
		data[key] = value
	}
	data["Env"] = environMap(environ)
	return &Renderer{
		data: data,
		env:  env,
	}, nil
}

// RenderArgv renders each argument in argv as a Go template.
func (r *Renderer) RenderArgv(argv []string) ([]string, error) {
	rendered := make([]string, 0, len(argv))
	for i, arg := range argv {
		value, err := r.RenderString(arg)
		if err != nil {
			return nil, fmt.Errorf("exec[%d] %q: %w", i, arg, err)
		}
		rendered = append(rendered, value)
	}
	return rendered, nil
}

// RenderString renders value as a Go template.
func (r *Renderer) RenderString(value string) (string, error) {
	tmpl, err := template.New("exec").Option("missingkey=error").Parse(value)
	if err != nil {
		return "", err
	}
	var out bytes.Buffer
	if err := tmpl.Execute(&out, r.data); err != nil {
		return "", err
	}
	return out.String(), nil
}

// Env returns the environment variables derived from the context.
func (r *Renderer) Env() []string {
	return append([]string(nil), r.env...)
}

func contextEnv(context Context) ([]string, error) {
	if len(context) == 0 {
		return nil, nil
	}
	keys := make([]string, 0, len(context))
	for key := range context {
		keys = append(keys, key)
	}
	sort.Strings(keys)

	values := make(map[string]string, len(keys))
	sources := make(map[string]string, len(keys))
	for _, key := range keys {
		value, ok := scalarEnvValue(context[key])
		if !ok {
			continue
		}
		envKey, err := EnvName(key)
		if err != nil {
			return nil, err
		}
		if source, ok := sources[envKey]; ok {
			return nil, fmt.Errorf("exec template context keys %q and %q both produce environment name %q", source, key, envKey)
		}
		sources[envKey] = key
		values[envKey] = value
	}

	keys = keys[:0]
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)

	env := make([]string, 0, len(keys))
	for _, key := range keys {
		env = append(env, key+"="+values[key])
	}
	return env, nil
}

func scalarEnvValue(value any) (string, bool) {
	switch value := value.(type) {
	case string:
		return value, true
	case bool,
		int, int8, int16, int32, int64,
		uint, uint8, uint16, uint32, uint64,
		float32, float64:
		return fmt.Sprint(value), true
	case fmt.Stringer:
		return value.String(), true
	default:
		return "", false
	}
}

func environMap(environ []string) map[string]string {
	env := make(map[string]string)
	for _, entry := range environ {
		key, value, ok := strings.Cut(entry, "=")
		if !ok {
			continue
		}
		env[key] = value
	}
	return env
}

// EnvName converts template context keys into stable environment names.
// For example, vmStatePath becomes VM_STATE_PATH.
func EnvName(key string) (string, error) {
	if key == "" {
		return "", fmt.Errorf("exec template context key must not be empty")
	}
	if key == "Env" {
		return "", fmt.Errorf("exec template context key %q is reserved", key)
	}
	if strings.ContainsRune(key, '=') {
		return "", fmt.Errorf("exec template context key %q must not contain '='", key)
	}

	var builder strings.Builder
	var previousUnderscore bool
	var previousLowerOrDigit bool
	for _, r := range key {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			if unicode.IsUpper(r) && previousLowerOrDigit && !previousUnderscore {
				builder.WriteByte('_')
			}
			builder.WriteRune(unicode.ToUpper(r))
			previousUnderscore = false
			previousLowerOrDigit = unicode.IsLower(r) || unicode.IsDigit(r)
			continue
		}
		if builder.Len() > 0 && !previousUnderscore {
			builder.WriteByte('_')
		}
		previousUnderscore = true
		previousLowerOrDigit = false
	}
	envKey := strings.Trim(builder.String(), "_")
	if envKey == "" {
		return "", fmt.Errorf("exec template context key %q does not produce an environment name", key)
	}
	return envKey, nil
}
