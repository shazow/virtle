package balloon

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	govmmQemu "github.com/kata-containers/govmm/qemu"
	"github.com/shazow/virtle/internal/manifest"
)

// AppendQEMUArgs appends the virtio-balloon device arguments for device,
// or returns args unchanged when the manifest configures no balloon.
func AppendQEMUArgs(
	args []string,
	resolveTransport func(string) (govmmQemu.VirtioTransport, error),
	device *manifest.BalloonDevice,
) ([]string, error) {
	if device == nil {
		return args, nil
	}

	balloonTransport, err := resolveTransport(device.Transport)
	if err != nil {
		return nil, err
	}
	driver := govmmQemu.BalloonDeviceTransport[balloonTransport]
	deviceParams := []string{
		driver,
		fmt.Sprintf("id=%s", device.ID),
		fmt.Sprintf("deflate-on-oom=%s", manifest.OnOff(device.DeflateOnOOM)),
		fmt.Sprintf("free-page-reporting=%s", manifest.OnOff(device.FreePageReporting)),
	}

	return append(args, "-device", strings.Join(deviceParams, ",")), nil
}

func ControllerTask(session MonitorSession, device *manifest.BalloonDevice, notificationSink notifier, logger *slog.Logger) func(context.Context) error {
	if device == nil || device.Controller == nil || session == nil {
		return nil
	}
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}

	controller := &controller{
		Session:  newQMPSession(session),
		Logger:   logger,
		DeviceID: device.ID,
		Config:   *device.Controller,
		Notifier: notificationSink,
	}

	return func(ctx context.Context) error {
		err := controller.Run(ctx)
		if err != nil && !errors.Is(err, context.Canceled) {
			controller.Logger.Warn("balloon controller stopped", "err", err)
		}
		return nil
	}
}
