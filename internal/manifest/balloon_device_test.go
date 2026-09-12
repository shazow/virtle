package manifest

import (
	"testing"

	"github.com/shazow/virtle/units"
)

func TestApplyDefaultsCreatesHalfAllocationTarget(t *testing.T) {
	device := &BalloonDevice{
		ID:        "balloon0",
		Transport: "pci",
	}

	applyBalloonDefaults(2048, device)

	if device.Controller == nil {
		t.Fatal("expected controller defaults to be created")
	}
	if got, want := device.Controller.MinActual, units.MiB(1024); got != want {
		t.Fatalf("unexpected minActualMiB: got %d want %d", got, want)
	}
	if got, want := device.Controller.MaxActual, units.MiB(2048); got != want {
		t.Fatalf("unexpected maxActualMiB: got %d want %d", got, want)
	}
	if got, want := device.Controller.GrowBelowAvailable, units.MiB(512); got != want {
		t.Fatalf("unexpected growBelowAvailableMiB: got %d want %d", got, want)
	}
	if got, want := device.Controller.ReclaimAboveAvailable, units.MiB(1024); got != want {
		t.Fatalf("unexpected reclaimAboveAvailableMiB: got %d want %d", got, want)
	}
	if got, want := device.Controller.Step, defaultBalloonControllerStep; got != want {
		t.Fatalf("unexpected stepMiB: got %d want %d", got, want)
	}
	if got, want := device.Controller.PollInterval, defaultBalloonControllerPollInterval; got != want {
		t.Fatalf("unexpected pollIntervalSeconds: got %d want %d", got, want)
	}
	if got, want := device.Controller.ReclaimHoldoff, defaultBalloonControllerReclaimHoldoff; got != want {
		t.Fatalf("unexpected reclaimHoldoffSeconds: got %d want %d", got, want)
	}
}

func TestApplyDefaultsDerivesThresholdsFromExplicitIdleTarget(t *testing.T) {
	device := &BalloonDevice{
		ID:        "balloon0",
		Transport: "pci",
		Controller: &BalloonControllerConfig{
			MinActual: 768,
		},
	}

	applyBalloonDefaults(2048, device)

	if got, want := device.Controller.MaxActual, units.MiB(2048); got != want {
		t.Fatalf("unexpected maxActualMiB: got %d want %d", got, want)
	}
	if got, want := device.Controller.GrowBelowAvailable, units.MiB(384); got != want {
		t.Fatalf("unexpected growBelowAvailableMiB: got %d want %d", got, want)
	}
	if got, want := device.Controller.ReclaimAboveAvailable, units.MiB(768); got != want {
		t.Fatalf("unexpected reclaimAboveAvailableMiB: got %d want %d", got, want)
	}
}

func TestValidateControllerRejectsNegativeThresholds(t *testing.T) {
	config := &BalloonControllerConfig{
		MinActual:             512,
		MaxActual:             1024,
		GrowBelowAvailable:    -1,
		ReclaimAboveAvailable: 512,
		Step:                  defaultBalloonControllerStep,
		PollInterval:          defaultBalloonControllerPollInterval,
		ReclaimHoldoff:        defaultBalloonControllerReclaimHoldoff,
	}
	if err := validateBalloonController(1024, config); err == nil {
		t.Fatal("expected negative grow threshold validation error")
	}

	config = &BalloonControllerConfig{
		MinActual:             512,
		MaxActual:             1024,
		GrowBelowAvailable:    256,
		ReclaimAboveAvailable: -1,
		Step:                  defaultBalloonControllerStep,
		PollInterval:          defaultBalloonControllerPollInterval,
		ReclaimHoldoff:        defaultBalloonControllerReclaimHoldoff,
	}
	if err := validateBalloonController(1024, config); err == nil {
		t.Fatal("expected negative reclaim threshold validation error")
	}
}
