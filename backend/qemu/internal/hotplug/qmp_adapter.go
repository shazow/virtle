package hotplug

import (
	"context"

	"github.com/shazow/virtle/internal/manifest"
)

// QMPDeviceAdapter adapts a generic QMP client to hotplug device operations.
type QMPDeviceAdapter struct {
	Client interface {
		RunRaw(context.Context, string) error
		DeviceDelAndWait(context.Context, string) error
	}
}

func (a QMPDeviceAdapter) AttachDevice(ctx context.Context, device manifest.HotplugDevice, bus string) (func(context.Context), error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	plan := planFor(device, bus)
	// Rollback must run even when the attach failed because ctx was canceled,
	// so release commands use a detached context; only commands that actually
	// succeeded are released.
	rollbackCtx := context.WithoutCancel(ctx)
	succeeded := 0
	rollback := func() {
		for _, command := range plan.release[:succeeded] {
			_ = a.Client.RunRaw(rollbackCtx, command)
		}
	}
	for _, command := range plan.backend {
		if err := ctx.Err(); err != nil {
			rollback()
			return nil, err
		}
		if err := a.Client.RunRaw(ctx, command); err != nil {
			rollback()
			return nil, err
		}
		succeeded++
	}
	if err := ctx.Err(); err != nil {
		rollback()
		return nil, err
	}
	if err := a.Client.RunRaw(ctx, plan.deviceAdd); err != nil {
		rollback()
		return nil, err
	}
	// Fully attached: undoing now needs a device_del round trip first, which
	// DetachDevice does before running the same backend release.
	return func(ctx context.Context) {
		_ = a.DetachDevice(context.WithoutCancel(ctx), device)
	}, nil
}

func (a QMPDeviceAdapter) DetachDevice(ctx context.Context, device manifest.HotplugDevice) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	deviceID := qemuDeviceID(device.ID)
	if err := a.Client.DeviceDelAndWait(ctx, deviceID); err != nil {
		return err
	}
	for _, command := range planFor(device, "").release {
		if err := a.Client.RunRaw(ctx, command); err != nil {
			return err
		}
	}
	return nil
}
