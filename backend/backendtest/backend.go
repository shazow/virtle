// Package backendtest provides backend conformance tests and an in-memory
// backend factory for unit tests.
package backendtest

import (
	"context"
	"sync"

	"github.com/shazow/virtle/backend"
	"github.com/shazow/virtle/units"
	"github.com/shazow/virtle/vm"
	"github.com/shazow/virtle/vm/vmtest"
)

type memoryBackend struct {
	guest *vmtest.Guest
}

// NewMemoryBackend returns an in-memory backend whose machines start
// immediately and use guest for remote control. A nil guest creates an empty
// vmtest.Guest for each machine.
func NewMemoryBackend(guest *vmtest.Guest) backend.Backend {
	return &memoryBackend{guest: guest}
}

func (b *memoryBackend) Start(ctx context.Context, spec *vm.Spec) (backend.Machine, error) {
	if err := context.Cause(ctx); err != nil {
		return nil, err
	}
	g := b.guest
	if g == nil {
		g = &vmtest.Guest{}
	}
	return &memoryMachine{guest: g, spec: spec, done: make(chan struct{}), state: backend.StateReady}, nil
}

func (b *memoryBackend) Resume(ctx context.Context, spec *vm.Spec) (backend.Machine, error) {
	return b.Start(ctx, spec)
}

func (*memoryBackend) StateVersion() string { return "backendtest-v1" }

type memoryMachine struct {
	guest *vmtest.Guest
	spec  *vm.Spec
	done  chan struct{}
	once  sync.Once

	mu      sync.Mutex
	err     error
	state   backend.State
	memory  units.Bytes
	devices []vm.Device
}

func (m *memoryMachine) Done() <-chan struct{} { return m.done }

func (m *memoryMachine) Err() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.err
}

func (m *memoryMachine) Wait(ctx context.Context) error {
	select {
	case <-m.done:
		return m.Err()
	case <-ctx.Done():
		return context.Cause(ctx)
	}
}

func (m *memoryMachine) stop(err error) {
	m.once.Do(func() {
		m.mu.Lock()
		m.err = err
		m.state = backend.StateStopped
		m.mu.Unlock()
		close(m.done)
	})
}

func (m *memoryMachine) Kill() error {
	m.stop(nil)
	return nil
}

func (m *memoryMachine) Shutdown(ctx context.Context) error {
	if err := context.Cause(ctx); err != nil {
		m.stop(err)
		return err
	}
	err := m.guest.Shutdown(ctx)
	m.stop(err)
	return err
}

func (m *memoryMachine) Suspend(ctx context.Context) error {
	if err := context.Cause(ctx); err != nil {
		return err
	}
	m.stop(nil)
	return nil
}

func (m *memoryMachine) ResizeMemory(ctx context.Context, size units.Bytes) error {
	if err := context.Cause(ctx); err != nil {
		return err
	}
	m.mu.Lock()
	m.memory = size
	m.mu.Unlock()
	return nil
}

func (m *memoryMachine) Attach(ctx context.Context, dev vm.Device) error {
	if err := context.Cause(ctx); err != nil {
		return err
	}
	m.mu.Lock()
	m.devices = append(m.devices, dev)
	m.mu.Unlock()
	return nil
}

func (m *memoryMachine) Detach(ctx context.Context, dev vm.Device) error {
	if err := context.Cause(ctx); err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	for i := range m.devices {
		if m.devices[i] == dev {
			m.devices = append(m.devices[:i], m.devices[i+1:]...)
			break
		}
	}
	return nil
}

func (m *memoryMachine) RemoteControl() (vm.Guest, error) { return m.guest, nil }

func (m *memoryMachine) Status(ctx context.Context) (backend.Status, error) {
	if err := context.Cause(ctx); err != nil {
		return backend.Status{}, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	return backend.Status{State: m.state}, nil
}

var (
	_ backend.Backend        = (*memoryBackend)(nil)
	_ backend.Resumer        = (*memoryBackend)(nil)
	_ backend.Machine        = (*memoryMachine)(nil)
	_ backend.Suspender      = (*memoryMachine)(nil)
	_ backend.MemoryResizer  = (*memoryMachine)(nil)
	_ backend.DeviceAttacher = (*memoryMachine)(nil)
	_ backend.StatusReporter = (*memoryMachine)(nil)
)
