package hotplug

import (
	"sync"

	"github.com/shazow/virtle/internal/executor"
	"github.com/shazow/virtle/internal/manifest"
)

type processSet interface {
	Add(...*executor.Process)
	Remove(*executor.Process) bool
}

type attachment struct {
	device manifest.HotplugDevice
	port   int
	helper *executor.Process
}

// Runtime is the live hotplug registry for one VM. Hotplug is experimental
// and supported only through the running VM's control plane, so attachment
// recovery across launcher restarts is deliberately out of scope.
type Runtime struct {
	mu          sync.Mutex
	attachments map[string]*attachment
	processes   processSet
}

func NewRuntime(processes processSet) *Runtime {
	return &Runtime{
		attachments: make(map[string]*attachment),
		processes:   processes,
	}
}

// allocatePort runs while the caller holds r.mu.
func (r *Runtime) allocatePort(preferred, count int) int {
	occupied := make(map[int]bool, len(r.attachments))
	for _, attached := range r.attachments {
		occupied[attached.port] = true
	}
	if preferred >= 0 && preferred < count && !occupied[preferred] {
		return preferred
	}
	for port := 0; port < count; port++ {
		if !occupied[port] {
			return port
		}
	}
	return -1
}

// register runs while the caller holds r.mu.
func (r *Runtime) register(device manifest.HotplugDevice, port int, helper *executor.Process) {
	attached := &attachment{device: device, port: port, helper: helper}
	r.attachments[device.ID] = attached
	if helper == nil {
		return
	}
	if r.processes != nil {
		r.processes.Add(helper)
	}
	go func() {
		<-helper.Done()
		r.mu.Lock()
		if current := r.attachments[device.ID]; current == attached && current.helper == helper {
			current.helper = nil
		}
		r.mu.Unlock()
		if r.processes != nil {
			r.processes.Remove(helper)
		}
	}()
}

// remove runs while the caller holds r.mu.
func (r *Runtime) remove(id string) {
	attached := r.attachments[id]
	delete(r.attachments, id)
	if attached != nil && attached.helper != nil && r.processes != nil {
		r.processes.Remove(attached.helper)
	}
}
