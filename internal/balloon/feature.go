package balloon

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	govmmQemu "github.com/kata-containers/govmm/qemu"
)

func sessionFromMonitor(session MonitorSession) session {
	if session == nil {
		return nil
	}
	return newQMPSession(session)
}

func AppendQEMUArgs(
	args []string,
	config *govmmQemu.Config,
	resolveTransport func(string) (govmmQemu.VirtioTransport, error),
	device *Device,
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
		fmt.Sprintf("deflate-on-oom=%s", onOff(device.DeflateOnOOM)),
		fmt.Sprintf("free-page-reporting=%s", onOff(device.FreePageReporting)),
	}

	return append(args, "-device", strings.Join(deviceParams, ",")), nil
}

func ControllerTask(qmpTimeout time.Duration, session MonitorSession, device *Device, notificationSink notifier) func(context.Context) error {
	if device == nil || device.Controller == nil || session == nil {
		return nil
	}

	controller := &controller{
		Session:    sessionFromMonitor(session),
		Logger:     logger,
		DeviceID:   device.ID,
		Config:     *device.Controller,
		QMPTimeout: qmpTimeout,
		Notifier:   notificationSink,
	}

	return func(ctx context.Context) error {
		err := controller.Run(ctx)
		if err != nil && !errors.Is(err, context.Canceled) {
			controller.Logger.Info("balloon controller disabled", "err", err)
		}
		return nil
	}
}

func onOff(v bool) string {
	if v {
		return "on"
	}
	return "off"
}
