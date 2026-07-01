package launch

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/shazow/virtle/internal/executor"
)

// ProcessSet tracks launch-owned managed processes. A nil *ProcessSet is invalid.
type ProcessSet struct {
	mu    sync.Mutex
	group executor.Group
	qemu  *executor.Process
	tasks taskGroup
}

func NewProcessSet() *ProcessSet {
	return &ProcessSet{group: executor.NewGroup()}
}

func (p *ProcessSet) Add(processes ...*executor.Process) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.group.Add(processes...)
}

func (p *ProcessSet) AddGroup(group executor.Group) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.group.Add(group.Processes()...)
}

func (p *ProcessSet) SetQEMU(process *executor.Process) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.qemu = process
	p.group.Add(process)
}

func (p *ProcessSet) QEMU() *executor.Process {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.qemu
}

func (p *ProcessSet) Remove(process *executor.Process) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.group.Remove(process)
}

func (p *ProcessSet) Watchers() executor.Group {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.group.Snapshot()
}

func (p *ProcessSet) VMWatchers() executor.Group {
	p.mu.Lock()
	watchers := p.group.Snapshot()
	qemu := p.qemu
	p.mu.Unlock()
	watchers.Remove(qemu)
	return watchers
}

func (p *ProcessSet) StartTasks(ctx context.Context, tasks ...func(context.Context) error) {
	var started taskGroup
	for _, task := range tasks {
		started.Add(startTask(ctx, task))
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.tasks = started
}

func (p *ProcessSet) Close(delay time.Duration) error {
	p.mu.Lock()
	tasks := taskGroup{tasks: append([]*task(nil), p.tasks.tasks...)}
	group := p.group.Snapshot()
	p.mu.Unlock()
	return errors.Join(tasks.Stop(), group.StopAll(delay))
}
