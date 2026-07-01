package executor

import (
	"errors"
	"time"
)

// Group tracks a set of managed processes.
type Group struct {
	processes []*Process
}

// NewGroup returns a group containing processes.
func NewGroup(processes ...*Process) Group {
	var group Group
	group.Add(processes...)
	return group
}

// Add appends processes to the group.
func (g *Group) Add(processes ...*Process) {
	for _, process := range processes {
		if process != nil {
			g.processes = append(g.processes, process)
		}
	}
}

// Remove removes process from the group.
func (g *Group) Remove(process *Process) bool {
	if g == nil || process == nil {
		return false
	}
	for i := len(g.processes) - 1; i >= 0; i-- {
		if g.processes[i] == process {
			g.processes = append(g.processes[:i], g.processes[i+1:]...)
			return true
		}
	}
	return false
}

// Snapshot returns a group with an independent copy of the process list.
func (g *Group) Snapshot() Group {
	if g == nil {
		return NewGroup()
	}
	return NewGroup(g.processes...)
}

// Processes returns a copy of the process list.
func (g *Group) Processes() []*Process {
	if g == nil || len(g.processes) == 0 {
		return nil
	}
	return append([]*Process(nil), g.processes...)
}

// FirstExit returns the first process in the group that has exited without blocking.
func (g *Group) FirstExit() (*Process, error, bool) {
	if g == nil {
		return nil, nil, false
	}
	for _, process := range g.processes {
		if process == nil {
			continue
		}
		exited, err := process.PollExit()
		if exited {
			return process, err, true
		}
	}
	return nil, nil, false
}

// StopAll stops processes in reverse order.
func (g *Group) StopAll(delay time.Duration) error {
	if g == nil {
		return nil
	}
	var errs []error
	for i := len(g.processes) - 1; i >= 0; i-- {
		if err := g.processes[i].Stop(delay); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// Len returns the number of processes in the group.
func (g *Group) Len() int {
	if g == nil {
		return 0
	}
	return len(g.processes)
}
