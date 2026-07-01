package balloon

import "testing"

func TestApplyDefaultsCreatesHalfAllocationTarget(t *testing.T) {
	device := &Device{
		ID:        "balloon0",
		Transport: "pci",
	}

	ApplyDefaults(2048, device)

	if device.Controller == nil {
		t.Fatal("expected controller defaults to be created")
	}
	if got, want := device.Controller.MinActual.Int(), 1024; got != want {
		t.Fatalf("unexpected minActualMiB: got %d want %d", got, want)
	}
	if got, want := device.Controller.MaxActual.Int(), 2048; got != want {
		t.Fatalf("unexpected maxActualMiB: got %d want %d", got, want)
	}
	if got, want := device.Controller.GrowBelowAvailable.Int(), 512; got != want {
		t.Fatalf("unexpected growBelowAvailableMiB: got %d want %d", got, want)
	}
	if got, want := device.Controller.ReclaimAboveAvailable.Int(), 1024; got != want {
		t.Fatalf("unexpected reclaimAboveAvailableMiB: got %d want %d", got, want)
	}
	if got, want := device.Controller.Step, DefaultControllerStep; got != want {
		t.Fatalf("unexpected stepMiB: got %d want %d", got, want)
	}
	if got, want := device.Controller.PollIntervalSeconds, DefaultControllerPollIntervalSecs; got != want {
		t.Fatalf("unexpected pollIntervalSeconds: got %d want %d", got, want)
	}
	if got, want := device.Controller.ReclaimHoldoffSeconds, DefaultControllerReclaimHoldoff; got != want {
		t.Fatalf("unexpected reclaimHoldoffSeconds: got %d want %d", got, want)
	}
}

func TestApplyDefaultsDerivesThresholdsFromExplicitIdleTarget(t *testing.T) {
	device := &Device{
		ID:        "balloon0",
		Transport: "pci",
		Controller: &ControllerConfig{
			MinActual: 768,
		},
	}

	ApplyDefaults(2048, device)

	if got, want := device.Controller.MaxActual.Int(), 2048; got != want {
		t.Fatalf("unexpected maxActualMiB: got %d want %d", got, want)
	}
	if got, want := device.Controller.GrowBelowAvailable.Int(), 384; got != want {
		t.Fatalf("unexpected growBelowAvailableMiB: got %d want %d", got, want)
	}
	if got, want := device.Controller.ReclaimAboveAvailable.Int(), 768; got != want {
		t.Fatalf("unexpected reclaimAboveAvailableMiB: got %d want %d", got, want)
	}
}

func TestValidateControllerRejectsNegativeThresholds(t *testing.T) {
	config := &ControllerConfig{
		MinActual:             512,
		MaxActual:             1024,
		GrowBelowAvailable:    -1,
		ReclaimAboveAvailable: 512,
		Step:                  DefaultControllerStep,
		PollIntervalSeconds:   DefaultControllerPollIntervalSecs,
		ReclaimHoldoffSeconds: DefaultControllerReclaimHoldoff,
	}
	if err := ValidateController(1024, config); err == nil {
		t.Fatal("expected negative grow threshold validation error")
	}

	config = &ControllerConfig{
		MinActual:             512,
		MaxActual:             1024,
		GrowBelowAvailable:    256,
		ReclaimAboveAvailable: -1,
		Step:                  DefaultControllerStep,
		PollIntervalSeconds:   DefaultControllerPollIntervalSecs,
		ReclaimHoldoffSeconds: DefaultControllerReclaimHoldoff,
	}
	if err := ValidateController(1024, config); err == nil {
		t.Fatal("expected negative reclaim threshold validation error")
	}
}
