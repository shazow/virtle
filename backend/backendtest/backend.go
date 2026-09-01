// Package backendtest provides backend conformance tests and in-memory doubles.
package backendtest

import (
	"context"
	"sync"

	"github.com/shazow/virtle/backend"
	"github.com/shazow/virtle/units"
	"github.com/shazow/virtle/vm"
	"github.com/shazow/virtle/vm/vmtest"
)

// Backend is an in-memory backend whose machines start immediately.
type Backend struct {
	Guest *vmtest.Guest
}

func (b *Backend) Start(ctx context.Context, spec *vm.Spec) (backend.Machine, error) {
	if err := context.Cause(ctx); err != nil {
		return nil, err
	}
	g := b.Guest
	if g == nil {
		g = &vmtest.Guest{}
	}
	return &Machine{guest: g, spec: spec, done: make(chan struct{}), state: backend.StateReady}, nil
}

func (b *Backend) Resume(ctx context.Context, spec *vm.Spec) (backend.Machine, error) {
	return b.Start(ctx, spec)
}

func (*Backend) StateVersion() string { return "backendtest-v1" }

// Machine is an in-memory backend.Machine.
type Machine struct {
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

func (m *Machine) Done() <-chan struct{} { return m.done }

func (m *Machine) Err() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.err
}

func (m *Machine) Wait(ctx context.Context) error {
	select {
	case <-m.done:
		return m.Err()
	case <-ctx.Done():
		return context.Cause(ctx)
	}
}

func (m *Machine) stop(err error) {
	m.once.Do(func() {
		m.mu.Lock()
		m.err = err
		m.state = backend.StateStopped
		m.mu.Unlock()
		close(m.done)
	})
}

func (m *Machine) Kill() error {
	m.stop(nil)
	return nil
}

func (m *Machine) Shutdown(ctx context.Context) error {
	if err := context.Cause(ctx); err != nil {
		m.stop(err)
		return err
	}
	err := m.guest.Shutdown(ctx)
	m.stop(err)
	return err
}

func (m *Machine) Suspend(ctx context.Context) error {
	if err := context.Cause(ctx); err != nil {
		return err
	}
	m.stop(nil)
	return nil
}

func (m *Machine) ResizeMemory(ctx context.Context, size units.Bytes) error {
	if err := context.Cause(ctx); err != nil {
		return err
	}
	m.mu.Lock()
	m.memory = size
	m.mu.Unlock()
	return nil
}

func (m *Machine) Attach(ctx context.Context, dev vm.Device) error {
	if err := context.Cause(ctx); err != nil {
		return err
	}
	m.mu.Lock()
	m.devices = append(m.devices, dev)
	m.mu.Unlock()
	return nil
}

func (m *Machine) Detach(ctx context.Context, dev vm.Device) error {
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

func (m *Machine) RemoteControl() (vm.Guest, error) { return m.guest, nil }

func (m *Machine) Status(ctx context.Context) (backend.Status, error) {
	if err := context.Cause(ctx); err != nil {
		return backend.Status{}, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	return backend.Status{State: m.state}, nil
}

var (
	_ backend.Backend        = (*Backend)(nil)
	_ backend.Resumer        = (*Backend)(nil)
	_ backend.Machine        = (*Machine)(nil)
	_ backend.Suspender      = (*Machine)(nil)
	_ backend.MemoryResizer  = (*Machine)(nil)
	_ backend.DeviceAttacher = (*Machine)(nil)
	_ backend.StatusReporter = (*Machine)(nil)
)
