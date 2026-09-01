package manifest

import (
	"fmt"
	"time"
)

const (
	defaultBalloonControllerStep           = MiB(256)
	defaultBalloonControllerPollInterval   = 5 * time.Second
	defaultBalloonControllerReclaimHoldoff = 30 * time.Second
)

// BalloonDevice is the resolved virtio-balloon device configuration.
type BalloonDevice struct {
	ID                string                   `json:"id" toml:"id"`
	Transport         string                   `json:"transport" toml:"transport"`
	DeflateOnOOM      bool                     `json:"deflateOnOOM,omitempty" toml:"deflateOnOOM"`
	FreePageReporting bool                     `json:"freePageReporting,omitempty" toml:"freePageReporting"`
	Controller        *BalloonControllerConfig `json:"controller,omitempty" toml:"controller"`
}

// BalloonControllerConfig configures the automatic balloon controller.
type BalloonControllerConfig struct {
	MinActual             MiB           `json:"minActualMiB" toml:"minActualMiB"`
	MaxActual             MiB           `json:"maxActualMiB,omitempty" toml:"maxActualMiB"`
	GrowBelowAvailable    MiB           `json:"growBelowAvailableMiB" toml:"growBelowAvailableMiB"`
	ReclaimAboveAvailable MiB           `json:"reclaimAboveAvailableMiB" toml:"reclaimAboveAvailableMiB"`
	Step                  MiB           `json:"stepMiB,omitempty" toml:"stepMiB"`
	PollInterval          time.Duration `json:"pollInterval,omitempty" toml:"pollInterval"`
	ReclaimHoldoff        time.Duration `json:"reclaimHoldoff,omitempty" toml:"reclaimHoldoff"`
}

func applyBalloonDefaults(memory MiB, device *BalloonDevice) {
	if device == nil {
		return
	}

	if device.Controller == nil {
		device.Controller = &BalloonControllerConfig{}
	}

	controller := device.Controller
	if controller.MaxActual == 0 {
		controller.MaxActual = memory
	}
	idleTarget := controller.MinActual
	if idleTarget <= 0 {
		idleTarget = defaultBalloonMinActual(controller.MaxActual, memory)
	}
	if controller.MinActual == 0 {
		controller.MinActual = idleTarget
	}
	if controller.GrowBelowAvailable == 0 {
		controller.GrowBelowAvailable = defaultBalloonGrowBelowAvailable(idleTarget)
	}
	if controller.ReclaimAboveAvailable == 0 {
		controller.ReclaimAboveAvailable = defaultBalloonReclaimAboveAvailable(idleTarget)
	}
	if controller.Step == 0 {
		controller.Step = defaultBalloonControllerStep
	}
	if controller.PollInterval == 0 {
		controller.PollInterval = defaultBalloonControllerPollInterval
	}
	if controller.ReclaimHoldoff == 0 {
		controller.ReclaimHoldoff = defaultBalloonControllerReclaimHoldoff
	}
}

func defaultBalloonMinActual(maxActual MiB, fallback MiB) MiB {
	if maxActual <= 0 {
		maxActual = fallback
	}
	if maxActual <= 1 {
		return 1
	}
	return (maxActual + 1) / 2
}

func defaultBalloonGrowBelowAvailable(minActual MiB) MiB {
	if minActual <= 1 {
		return 0
	}
	return minActual / 2
}

func defaultBalloonReclaimAboveAvailable(minActual MiB) MiB {
	if minActual <= 0 {
		return 1
	}
	return minActual
}

func validateBalloonDevice(memory MiB, device *BalloonDevice) error {
	if device == nil {
		return nil
	}

	switch {
	case !validQEMUTransport(device.Transport):
		return fmt.Errorf("manifest.qemu.devices.balloon.transport must be one of pci, mmio, or ccw")
	}

	if err := validateBalloonController(memory, device.Controller); err != nil {
		return fmt.Errorf("manifest.qemu.devices.balloon.controller.%s", err)
	}

	return nil
}

func validateBalloonController(memory MiB, controller *BalloonControllerConfig) error {
	if controller == nil {
		return nil
	}

	switch {
	case controller.MinActual <= 0:
		return fmt.Errorf("minActualMiB must be greater than zero")
	case controller.MinActual > controller.MaxActual:
		return fmt.Errorf("minActualMiB must be less than or equal to maxActualMiB")
	case controller.MaxActual > memory:
		return fmt.Errorf("maxActualMiB must be less than or equal to manifest.qemu.memory.sizeMiB")
	case controller.GrowBelowAvailable < 0:
		return fmt.Errorf("growBelowAvailableMiB must be greater than or equal to zero")
	case controller.ReclaimAboveAvailable < 0:
		return fmt.Errorf("reclaimAboveAvailableMiB must be greater than or equal to zero")
	case controller.GrowBelowAvailable >= controller.ReclaimAboveAvailable:
		return fmt.Errorf("growBelowAvailableMiB must be less than reclaimAboveAvailableMiB")
	case controller.Step <= 0:
		return fmt.Errorf("stepMiB must be greater than zero")
	case controller.PollInterval <= 0:
		return fmt.Errorf("pollInterval must be greater than zero")
	case controller.ReclaimHoldoff <= 0:
		return fmt.Errorf("reclaimHoldoff must be greater than zero")
	}

	return nil
}

func cloneBalloonDevice(device *BalloonDevice) *BalloonDevice {
	if device == nil {
		return nil
	}

	cloned := *device
	if device.Controller != nil {
		controller := *device.Controller
		cloned.Controller = &controller
	}
	return &cloned
}
