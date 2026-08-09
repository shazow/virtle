package balloon

import (
	"fmt"
	"time"

	"github.com/shazow/virtle/units"
)

const (
	DefaultControllerStep           = units.MiB(256)
	DefaultControllerPollInterval   = 5 * time.Second
	DefaultControllerReclaimHoldoff = 30 * time.Second
)

type Device struct {
	ID                string            `json:"id" toml:"id"`
	Transport         string            `json:"transport" toml:"transport"`
	DeflateOnOOM      bool              `json:"deflateOnOOM,omitempty" toml:"deflateOnOOM"`
	FreePageReporting bool              `json:"freePageReporting,omitempty" toml:"freePageReporting"`
	Controller        *ControllerConfig `json:"controller,omitempty" toml:"controller"`
}

type ControllerConfig struct {
	MinActual             units.MiB     `json:"minActualMiB" toml:"minActualMiB"`
	MaxActual             units.MiB     `json:"maxActualMiB,omitempty" toml:"maxActualMiB"`
	GrowBelowAvailable    units.MiB     `json:"growBelowAvailableMiB" toml:"growBelowAvailableMiB"`
	ReclaimAboveAvailable units.MiB     `json:"reclaimAboveAvailableMiB" toml:"reclaimAboveAvailableMiB"`
	Step                  units.MiB     `json:"stepMiB,omitempty" toml:"stepMiB"`
	PollInterval          time.Duration `json:"pollInterval,omitempty" toml:"pollInterval"`
	ReclaimHoldoff        time.Duration `json:"reclaimHoldoff,omitempty" toml:"reclaimHoldoff"`
}

func ApplyDefaults(memory units.MiB, device *Device) {
	if device == nil {
		return
	}

	if device.Controller == nil {
		device.Controller = &ControllerConfig{}
	}

	controller := device.Controller
	if controller.MaxActual == 0 {
		controller.MaxActual = memory
	}
	idleTarget := controller.MinActual
	if idleTarget <= 0 {
		idleTarget = defaultMinActual(controller.MaxActual, memory)
	}
	if controller.MinActual == 0 {
		controller.MinActual = idleTarget
	}
	if controller.GrowBelowAvailable == 0 {
		controller.GrowBelowAvailable = defaultGrowBelowAvailable(idleTarget)
	}
	if controller.ReclaimAboveAvailable == 0 {
		controller.ReclaimAboveAvailable = defaultReclaimAboveAvailable(idleTarget)
	}
	if controller.Step == 0 {
		controller.Step = DefaultControllerStep
	}
	if controller.PollInterval == 0 {
		controller.PollInterval = DefaultControllerPollInterval
	}
	if controller.ReclaimHoldoff == 0 {
		controller.ReclaimHoldoff = DefaultControllerReclaimHoldoff
	}
}

func defaultMinActual(maxActual units.MiB, fallback units.MiB) units.MiB {
	if maxActual <= 0 {
		maxActual = fallback
	}
	if maxActual <= 1 {
		return 1
	}
	return (maxActual + 1) / 2
}

func defaultGrowBelowAvailable(minActual units.MiB) units.MiB {
	if minActual <= 1 {
		return 0
	}
	return minActual / 2
}

func defaultReclaimAboveAvailable(minActual units.MiB) units.MiB {
	if minActual <= 0 {
		return 1
	}
	return minActual
}

func ValidateController(memory units.MiB, controller *ControllerConfig) error {
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

func CloneDevice(device *Device) *Device {
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
