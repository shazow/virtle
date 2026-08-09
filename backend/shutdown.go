package backend

import (
	"context"
	"errors"
)

// Shutdown stops inst gracefully.
//
// It asks the guest to power itself down through Instance.RemoteControl and
// waits for the VM to exit, falling back to Instance.Kill when the VM has no
// guest agent, when the request fails, or when ctx ends first. Kill is called
// either way, so the instance's host resources are released by the time
// Shutdown returns — which makes it the natural deferred teardown:
//
//	inst, err := b.Start(ctx, spec)
//	defer backend.Shutdown(ctx, inst)
//
// Give ctx a deadline generous enough for the guest to flush and unmount;
// when it expires the VM is killed, not left running.
func Shutdown(ctx context.Context, inst Instance) error {
	if inst == nil {
		return nil
	}
	return errors.Join(requestGuestShutdown(ctx, inst), inst.Kill())
}

func requestGuestShutdown(ctx context.Context, inst Instance) error {
	guest, err := inst.RemoteControl()
	if err != nil {
		if errors.Is(err, errors.ErrUnsupported) {
			// No agent to ask; Kill is the only stop this VM has.
			return nil
		}
		return err
	}
	if err := guest.Shutdown(ctx); err != nil {
		return err
	}
	return inst.Wait(ctx)
}
