package qemu

import (
	"context"
	"errors"
	"fmt"

	"github.com/shazow/virtle/backend"
	"github.com/shazow/virtle/units"
)

var _ backend.MemoryResizer = (*qemuBackend)(nil)

// ResizeMemory sets how much of the machine's memory the guest may use, by
// inflating or deflating its virtio-balloon. The machine must have been
// started with [Config].Balloon set; without the device there is nothing to
// resize, and the call reports errors.ErrUnsupported.
//
// The size is a target, not a guarantee: a guest releases ballooned memory as
// it can, and never gives back more than the guest kernel is willing to free.
func (b *qemuBackend) ResizeMemory(ctx context.Context, inst backend.Instance, size units.Bytes) error {
	i, err := b.instance(inst)
	if err != nil {
		return err
	}
	if i.manifest.QEMU.Devices.Balloon == nil {
		return fmt.Errorf("machine has no balloon device: %w", errors.ErrUnsupported)
	}
	if size <= 0 {
		return fmt.Errorf("memory size must be positive, got %s", size)
	}
	monitor, err := i.monitor()
	if err != nil {
		return err
	}
	i.logger.Info("resizing guest memory", "size", size.String())
	return b.setBalloonSize(ctx, monitor, size.Int64())
}
