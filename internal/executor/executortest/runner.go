package executortest

import (
	"fmt"
	"os"
	"os/exec"
	"sync"

	"github.com/shazow/virtle/internal/executor"
)

// Start records a command started by Runner.
type Start struct {
	Name         string
	Args         []string
	Env          []string
	EnvAdditions []string
	Dir          string
	ProcessGroup bool
	Cmd          *exec.Cmd
}

// RunnerSignal records a signal or kill observed by a runner process.
type RunnerSignal struct {
	Name   string
	Signal os.Signal
}

// Runner is a test implementation of the Runner Start API.
type Runner struct {
	OnStart     func(Start) (*Process, error)
	StartErrors map[string]error

	mu             sync.Mutex
	starts         []Start
	processes      map[string][]*Process
	prepared       map[*Process]struct{}
	processSignals []RunnerSignal
}

// Start records cmd and returns a test process.
func (r *Runner) Start(cmd *exec.Cmd) (*executor.Process, error) {
	start := runnerStart(cmd)

	r.mu.Lock()
	r.starts = append(r.starts, start)
	err := r.StartErrors[start.Name]
	onStart := r.OnStart
	r.mu.Unlock()

	if err != nil {
		return nil, err
	}
	if onStart != nil {
		process, err := onStart(start)
		if err != nil {
			return nil, err
		}
		if process == nil {
			process = ProcessFor(start.Name)
		} else {
			process.OverrideName = defaultName(process.OverrideName, start.Name)
		}
		r.prepareProcess(start.Name, process)
		return process.Process(), nil
	}

	return r.NewProcess(start.Name).Process(), nil
}

// ProcessFor returns an untracked test process for use from Runner.OnStart.
func ProcessFor(name string) *Process {
	return &Process{OverrideName: name}
}

// NewProcess returns a tracked test process with signal recording callbacks.
func (r *Runner) NewProcess(name string) *Process {
	process := ProcessFor(name)
	r.prepareProcess(name, process)
	return process
}

// Starts returns recorded command starts.
func (r *Runner) Starts() []Start {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]Start(nil), r.starts...)
}

// StartedNames returns recorded process names in start order.
func (r *Runner) StartedNames() []string {
	starts := r.Starts()
	names := make([]string, 0, len(starts))
	for _, start := range starts {
		names = append(names, start.Name)
	}
	return names
}

// Processes returns processes created for name.
func (r *Runner) Processes(name string) []*Process {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]*Process(nil), r.processes[name]...)
}

// ProcessSignals returns signal and kill events in observed order.
func (r *Runner) ProcessSignals() []RunnerSignal {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]RunnerSignal(nil), r.processSignals...)
}

// SignalNames returns process names for signal and kill events.
func (r *Runner) SignalNames() []string {
	signals := r.ProcessSignals()
	names := make([]string, 0, len(signals))
	for _, signal := range signals {
		names = append(names, signal.Name)
	}
	return names
}

func (r *Runner) prepareProcess(name string, process *Process) {
	if process == nil {
		return
	}
	if r.markPrepared(name, process) {
		r.configureProcess(name, process)
	}
}

func (r *Runner) markPrepared(name string, process *Process) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.prepared == nil {
		r.prepared = make(map[*Process]struct{})
	}
	if _, ok := r.prepared[process]; ok {
		return false
	}
	r.prepared[process] = struct{}{}
	if r.processes == nil {
		r.processes = make(map[string][]*Process)
	}
	r.processes[name] = append(r.processes[name], process)
	return true
}

func (r *Runner) configureProcess(name string, process *Process) {
	onSignal := process.OnSignal
	onKill := process.OnKill
	process.OnSignal = func(sig os.Signal) {
		if onSignal != nil {
			onSignal(sig)
		}
		r.recordProcessSignal(name, sig)
	}
	process.OnKill = func() {
		if onKill != nil {
			onKill()
		}
		r.recordProcessSignal(name, nil)
	}
}

func (r *Runner) recordProcessSignal(name string, sig os.Signal) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.processSignals = append(r.processSignals, RunnerSignal{Name: name, Signal: sig})
}

func runnerStart(cmd *exec.Cmd) Start {
	return Start{
		Name:         executor.CommandName(cmd),
		Args:         commandArgs(cmd),
		Env:          commandEnv(cmd),
		EnvAdditions: commandEnvAdditions(commandEnv(cmd)),
		Dir:          commandDir(cmd),
		ProcessGroup: commandProcessGroup(cmd),
		Cmd:          cmd,
	}
}

func defaultName(name string, fallback string) string {
	if name != "" {
		return name
	}
	return fallback
}

func commandArgs(cmd *exec.Cmd) []string {
	if cmd == nil || len(cmd.Args) == 0 {
		return nil
	}
	return append([]string(nil), cmd.Args[1:]...)
}

func commandEnv(cmd *exec.Cmd) []string {
	if cmd == nil {
		return nil
	}
	return append([]string(nil), cmd.Env...)
}

func commandEnvAdditions(env []string) []string {
	environ := os.Environ()
	if len(env) < len(environ) {
		return append([]string(nil), env...)
	}
	for i, entry := range environ {
		if env[i] != entry {
			return append([]string(nil), env...)
		}
	}
	return append([]string(nil), env[len(environ):]...)
}

func commandDir(cmd *exec.Cmd) string {
	if cmd == nil {
		return ""
	}
	return cmd.Dir
}

func commandProcessGroup(cmd *exec.Cmd) bool {
	return cmd != nil && cmd.SysProcAttr != nil && cmd.SysProcAttr.Setpgid
}

func (s Start) String() string {
	return fmt.Sprintf("%s %v", s.Name, s.Args)
}

var _ interface {
	Start(*exec.Cmd) (*executor.Process, error)
} = (*Runner)(nil)
