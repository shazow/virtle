package hotplug

import (
	"context"
	"time"
)

// QMPDeviceAdapter adapts a generic QMP client to hotplug device operations.
type QMPDeviceAdapter struct {
	Client interface {
		RunRaw(time.Duration, string) error
		DeviceDelAndWait(time.Duration, string) error
	}
	Timeout time.Duration
}

func (a QMPDeviceAdapter) AttachDevice(ctx context.Context, device Device, bus string) (func(context.Context), error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	successful := 0
	for _, command := range attachCommands(device, bus) {
		if err := ctx.Err(); err != nil {
			a.rollbackAttach(ctx, device, successful)
			return nil, err
		}
		if err := a.Client.RunRaw(a.Timeout, command); err != nil {
			a.rollbackAttach(ctx, device, successful)
			return nil, err
		}
		successful++
	}
	return func(ctx context.Context) {
		a.rollbackAttach(ctx, device, successful)
	}, nil
}

func (a QMPDeviceAdapter) DetachDevice(ctx context.Context, device Device) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	deviceID := qemuDeviceID(device.ID)
	if err := a.Client.DeviceDelAndWait(a.Timeout, deviceID); err != nil {
		return err
	}
	for _, command := range detachPostDeviceDelCommands(device) {
		if err := a.Client.RunRaw(a.Timeout, command); err != nil {
			return err
		}
	}
	return nil
}

func (a QMPDeviceAdapter) rollbackAttach(ctx context.Context, device Device, successful int) {
	if successful == 0 {
		return
	}
	cleanupCtx := context.WithoutCancel(ctx)
	if successful == len(attachCommands(device, "")) {
		_ = a.DetachDevice(cleanupCtx, device)
		return
	}
	for _, command := range rollbackAttachCommands(device, successful) {
		_ = a.Client.RunRaw(a.Timeout, command)
	}
}
