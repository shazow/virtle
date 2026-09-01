//go:build integration

package qemu_test

import (
	"os"
	"testing"

	"github.com/shazow/virtle/backend"
	"github.com/shazow/virtle/backend/backendtest"
	"github.com/shazow/virtle/backend/qemu"
	"github.com/shazow/virtle/units"
	"github.com/shazow/virtle/vm"
)

func TestIntegrationBackend(t *testing.T) {
	kernel := os.Getenv("VIRTLE_INTEGRATION_KERNEL")
	initrd := os.Getenv("VIRTLE_INTEGRATION_INITRD")
	if kernel == "" || initrd == "" {
		t.Skip("VIRTLE_INTEGRATION_KERNEL and VIRTLE_INTEGRATION_INITRD are required")
	}
	backendtest.TestBackend(t, func(t *testing.T) (backend.Backend, *vm.Spec) {
		return &qemu.Backend{
				Accel:         qemu.AccelTCG,
				MachineType:   os.Getenv("VIRTLE_INTEGRATION_MACHINE"),
				RemoteControl: qemu.QGA{},
			}, &vm.Spec{
				CPUs:   1,
				Memory: 256 * units.Mebibyte,
				Kernel: vm.Kernel{
					Path:    kernel,
					Initrd:  initrd,
					Cmdline: "console=ttyS0 panic=-1 rdinit=/init",
				},
				Dir: t.TempDir(),
			}
	})
}
