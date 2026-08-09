package manifest

import (
	"fmt"

	"github.com/shazow/virtle/internal/balloon"
	"github.com/shazow/virtle/units"
)

func applyBalloonDefaults(memory units.MiB, device *balloon.Device) {
	balloon.ApplyDefaults(memory, device)
}

func validateBalloonDevice(memory units.MiB, device *balloon.Device) error {
	if device == nil {
		return nil
	}

	switch {
	case !validQEMUTransport(device.Transport):
		return fmt.Errorf("manifest.qemu.devices.balloon.transport must be one of pci, mmio, or ccw")
	}

	if err := balloon.ValidateController(memory, device.Controller); err != nil {
		return fmt.Errorf("manifest.qemu.devices.balloon.controller.%s", err)
	}

	return nil
}

func cloneBalloonDevice(device *balloon.Device) *balloon.Device {
	return balloon.CloneDevice(device)
}
