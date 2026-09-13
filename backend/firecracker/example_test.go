package firecracker_test

import (
	"context"
	"log"
	"os"

	"github.com/shazow/virtle/backend/firecracker"
	"github.com/shazow/virtle/units"
	"github.com/shazow/virtle/vm"
)

// ExampleBackend boots a microVM from an uncompressed kernel and a raw root
// disk, printing the guest's serial console until the guest reboots (which
// exits Firecracker) or Shutdown stops it.
func ExampleBackend() {
	ctx := context.Background()
	b := &firecracker.Backend{
		Console:       firecracker.ConsolePrint,
		ConsoleOutput: os.Stdout,
	}
	m, err := b.Start(ctx, &vm.Spec{
		CPUs:   2,
		Memory: 512 * units.Mebibyte,
		Kernel: vm.Kernel{Path: "vmlinux", Cmdline: "pci=off"},
		// The image mounted at "/" is the root device: virtle passes
		// root=/dev/vda ro for it, so no initrd is needed.
		Disks: []vm.Disk{{Path: "rootfs.ext4", Format: "raw", ReadOnly: true, GuestPath: "/"}},
	})
	if err != nil {
		log.Print(err)
		return
	}
	defer func() {
		if err := m.Shutdown(ctx); err != nil {
			log.Print(err)
		}
	}()

	// Start returns once Firecracker has accepted the configuration, not
	// when the guest is ready: watch the console (Machine.Console) for that.
	// Wait returns when the guest reboots or panics, or after Shutdown.
	if err := m.Wait(ctx); err != nil {
		log.Print(err)
	}
}
