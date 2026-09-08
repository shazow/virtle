//go:build linux && integration

package firecracker_test

import (
	"bytes"
	"context"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/shazow/virtle/backend/firecracker"
	"github.com/shazow/virtle/units"
	"github.com/shazow/virtle/vm"
)

type guestOutput struct {
	mu    sync.Mutex
	data  bytes.Buffer
	once  sync.Once
	ready chan struct{}
}

func (w *guestOutput) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.data.Len()+len(p) <= 1024*1024 {
		w.data.Write(p)
	}
	if bytes.Contains(w.data.Bytes(), []byte("VIRTLE_READY:42")) {
		w.once.Do(func() { close(w.ready) })
	}
	return len(p), nil
}
func (w *guestOutput) String() string { w.mu.Lock(); defer w.mu.Unlock(); return w.data.String() }

func TestIntegrationFirecracker(t *testing.T) {
	kernel, initrd, rootfs := os.Getenv("VIRTLE_FIRECRACKER_KERNEL"), os.Getenv("VIRTLE_FIRECRACKER_INITRD"), os.Getenv("VIRTLE_FIRECRACKER_ROOTFS")
	if kernel == "" || initrd == "" || rootfs == "" {
		t.Skip("requires the recipe's VIRTLE_FIRECRACKER_KERNEL, VIRTLE_FIRECRACKER_INITRD and VIRTLE_FIRECRACKER_ROOTFS; Linux x86_64 with KVM")
	}
	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	defer cancel()
	output := &guestOutput{ready: make(chan struct{})}
	b := &firecracker.Backend{Binary: os.Getenv("VIRTLE_FIRECRACKER_BINARY"), Console: "print", ConsoleOutput: output}
	m, err := b.Start(ctx, &vm.Spec{Dir: t.TempDir(), CPUs: 1, Memory: 256 * units.Mebibyte, Kernel: vm.Kernel{Path: kernel, Initrd: initrd, Cmdline: "console=ttyS0 reboot=k panic=-1 pci=off rdinit=/init i8042.noaux i8042.nomux i8042.dumbkbd"}, Disks: []vm.Disk{{Path: rootfs, Format: "raw", ReadOnly: true}}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = m.Kill() })
	select {
	case <-output.ready:
	case <-m.Done():
		t.Fatalf("exited before ready: %v\n%s", m.Err(), output.String())
	case <-ctx.Done():
		t.Fatalf("readiness: %v\n%s", ctx.Err(), output.String())
	}
	if err := m.Shutdown(ctx); err != nil {
		t.Fatalf("shutdown: %v\n%s", err, output.String())
	}
	if err := m.Wait(ctx); err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains([]byte(output.String()), []byte("VIRTLE_SHUTDOWN:unmounted")) {
		t.Fatalf("no clean guest shutdown:\n%s", output.String())
	}
}
