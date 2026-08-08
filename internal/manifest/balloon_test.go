package manifest

import (
	"testing"
	"time"

	"github.com/shazow/virtle/internal/balloon"
)

func TestValidateAppliesBalloonDefaults(t *testing.T) {
	manifest := validManifestWithBalloon()

	if err := manifest.Validate(); err != nil {
		t.Fatalf("unexpected balloon controller validation error: %v", err)
	}
	if manifest.QEMU.Devices.Balloon.Controller == nil {
		t.Fatal("expected balloon controller defaults to be synthesized")
	}
	if got, want := manifest.QEMU.Devices.Balloon.Controller.MinActual.Int(), 512; got != want {
		t.Fatalf("unexpected balloon controller minActualMiB default: got %d want %d", got, want)
	}
	if got, want := manifest.QEMU.Devices.Balloon.Controller.MaxActual, manifest.QEMU.Memory.Size; got != want {
		t.Fatalf("unexpected balloon controller maxActualMiB default: got %d want %d", got, want)
	}
	if got, want := manifest.QEMU.Devices.Balloon.Controller.GrowBelowAvailable.Int(), 256; got != want {
		t.Fatalf("unexpected balloon controller grow threshold default: got %d want %d", got, want)
	}
	if got, want := manifest.QEMU.Devices.Balloon.Controller.ReclaimAboveAvailable.Int(), 512; got != want {
		t.Fatalf("unexpected balloon controller reclaim threshold default: got %d want %d", got, want)
	}
	if got, want := manifest.QEMU.Devices.Balloon.Controller.Step.Int(), 256; got != want {
		t.Fatalf("unexpected balloon controller step default: got %d want %d", got, want)
	}
	if got, want := manifest.QEMU.Devices.Balloon.Controller.PollInterval, 5*time.Second; got != want {
		t.Fatalf("unexpected balloon controller poll interval default: got %s want %s", got, want)
	}
	if got, want := manifest.QEMU.Devices.Balloon.Controller.ReclaimHoldoff, 30*time.Second; got != want {
		t.Fatalf("unexpected balloon controller reclaim holdoff default: got %s want %s", got, want)
	}
}

func TestValidateRejectsInvalidBalloonControllerConfig(t *testing.T) {
	invalidBounds := validManifestWithBalloon()
	invalidBounds.QEMU.Devices.Balloon.Controller = &balloon.ControllerConfig{
		MinActual: invalidBounds.QEMU.Memory.Size + 1,
	}
	if err := invalidBounds.Validate(); err == nil {
		t.Fatal("expected validation error for invalid balloon controller bounds")
	}

	invalidThresholds := validManifestWithBalloon()
	invalidThresholds.QEMU.Devices.Balloon.Controller = &balloon.ControllerConfig{
		GrowBelowAvailable:    512,
		ReclaimAboveAvailable: 512,
	}
	if err := invalidThresholds.Validate(); err == nil {
		t.Fatal("expected validation error for invalid balloon controller thresholds")
	}
}

func validManifestWithBalloon() *Manifest {
	manifest := validManifest()
	manifest.QEMU.Devices.Balloon = &balloon.Device{
		ID:        "balloon0",
		Transport: "pci",
	}
	return manifest
}

func validManifest() *Manifest {
	return &Manifest{
		Identity: Identity{HostName: "agent-sandbox"},
		Paths: Paths{
			WorkingDir: "/tmp/work",
			LockPath:   "/tmp/virtle.lock",
		},
		SSH: SSH{
			Argv:       []string{"/bin/ssh"},
			User:       "agent",
			RetryDelay: 500 * time.Millisecond,
		},
		QEMU: QEMU{
			BinaryPath: "/bin/qemu-system-x86_64",
			Name:       "agent-sandbox",
			Machine: QEMUMachine{
				Type:    "microvm",
				Options: []string{"accel=kvm:tcg"},
			},
			CPU: QEMUCPU{
				Model: "host",
			},
			Memory: QEMUMemory{
				Size: 1024,
			},
			Kernel: QEMUKernel{
				Path:       "/tmp/vmlinuz",
				InitrdPath: "/tmp/initrd",
			},
			SMP: QEMUSMP{
				CPUs: ExplicitCPUs(2),
			},
			QMP: QEMUQMP{
				SocketPath: "qmp.sock",
			},
			Knobs: QEMUKnobs{
				NoGraphic: true,
			},
			Devices: QEMUDevices{
				RNG: QEMURNGDevice{
					ID:        "rng0",
					Transport: "pci",
				},
				VirtioFS: []QEMUVirtioFSShare{
					{
						ID:         "fs0",
						SocketPath: "fs.sock",
						Tag:        "workspace",
						Transport:  "pci",
					},
				},
				Block: []QEMUBlockDevice{
					{
						ID:        "vda",
						ImagePath: "root.img",
						Transport: "pci",
					},
				},
				Network: []QEMUNetDevice{
					{
						ID:         "net0",
						Backend:    "user",
						MacAddress: "02:02:00:00:00:01",
						Transport:  "pci",
					},
				},
				VSOCK: QEMUVSOCKDevice{
					ID:        "vsock0",
					Transport: "pci",
				},
			},
		},
		Volumes: []Volume{
			{
				ImagePath:  "root.img",
				Size:       256,
				FSType:     "ext4",
				AutoCreate: true,
			},
		},
		Run: []Run{
			{
				Exec: []string{"/tmp/virtiofsd-workspace"},
				Vars: map[string]any{
					"Socket":      "/tmp/work/fs.sock",
					"MountTag":    "workspace",
					"MountSource": "/tmp/work",
				},
			},
		},
		CleanupFiles: []string{"fs.sock"},
	}
}

func TestResolveBalloonFreePageReporting(t *testing.T) {
	disabled := false
	device := resolveBalloon(&BalloonInput{Enabled: true, FreePageReporting: &disabled}, "pci")
	if device == nil {
		t.Fatalf("expected balloon device")
	}
	if device.FreePageReporting {
		t.Fatalf("expected free_page_reporting=false to be honored")
	}

	device = resolveBalloon(&BalloonInput{Enabled: true}, "pci")
	if device == nil {
		t.Fatalf("expected balloon device")
	}
	if !device.FreePageReporting {
		t.Fatalf("expected free page reporting to default to enabled")
	}
	if device.ID != "balloon0" || device.Transport != "pci" {
		t.Fatalf("unexpected device identity: %+v", device)
	}
}
