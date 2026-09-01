package backendtest

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"

	"github.com/shazow/virtle/backend"
	"github.com/shazow/virtle/units"
	"github.com/shazow/virtle/vm"
	"github.com/shazow/virtle/vm/vmtest"
)

// memBackend is an in-memory backend that satisfies the full contract,
// including the Suspender/Resumer pair and MemoryResizer, so the
// conformance test is itself tested.
type memBackend struct {
	mu        sync.Mutex
	suspended map[string]bool // by spec.Dir
}

func (b *memBackend) Start(ctx context.Context, spec *vm.Spec) (backend.Instance, error) {
	return newMemInstance(b, spec), nil
}

func (b *memBackend) Resume(ctx context.Context, spec *vm.Spec) (backend.Instance, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if !b.suspended[spec.Dir] {
		return nil, fmt.Errorf("no suspended state in %q", spec.Dir)
	}
	delete(b.suspended, spec.Dir)
	return newMemInstance(b, spec), nil
}

func (b *memBackend) StateVersion() string { return "mem-v1" }

type memInstance struct {
	spec    *vm.Spec
	backend *memBackend
	guest   *vmtest.Guest

	once sync.Once
	done chan struct{}
	err  error
}

func newMemInstance(b *memBackend, spec *vm.Spec) *memInstance {
	return &memInstance{spec: spec, backend: b, guest: &vmtest.Guest{}, done: make(chan struct{})}
}

func (i *memInstance) exit(err error) {
	i.once.Do(func() {
		i.err = err
		close(i.done)
	})
}

func (i *memInstance) Done() <-chan struct{} { return i.done }
func (i *memInstance) Err() error {
	select {
	case <-i.done:
		return i.err
	default:
		return nil
	}
}
func (i *memInstance) Wait(ctx context.Context) error {
	select {
	case <-i.done:
		return i.err
	case <-ctx.Done():
		return ctx.Err()
	}
}
func (i *memInstance) Kill() error {
	i.exit(errors.New("killed"))
	return nil
}
func (i *memInstance) Shutdown(ctx context.Context) error {
	if err := i.guest.Shutdown(ctx); err != nil {
		return i.Kill()
	}
	i.exit(nil)
	return nil
}
func (i *memInstance) RemoteControl() (vm.Guest, error) { return i.guest, nil }
func (i *memInstance) Suspend(ctx context.Context) error {
	i.backend.mu.Lock()
	i.backend.suspended[i.spec.Dir] = true
	i.backend.mu.Unlock()
	i.exit(nil)
	return nil
}
func (i *memInstance) ResizeMemory(ctx context.Context, size units.Bytes) error {
	if size > 2*i.spec.Memory {
		return fmt.Errorf("resize memory: %s exceeds the maximum", size)
	}
	i.spec.Memory = size
	return nil
}

var (
	_ backend.Backend       = (*memBackend)(nil)
	_ backend.Resumer       = (*memBackend)(nil)
	_ backend.Suspender     = (*memInstance)(nil)
	_ backend.MemoryResizer = (*memInstance)(nil)
)

func TestMemBackend(t *testing.T) {
	TestBackend(t, func(t *testing.T) (backend.Backend, *vm.Spec) {
		return &memBackend{suspended: map[string]bool{}}, &vm.Spec{Memory: 512 * units.Mebibyte, Dir: t.TempDir()}
	})
}
