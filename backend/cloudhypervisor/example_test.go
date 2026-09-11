package cloudhypervisor_test

import (
	"context"
	"log"
	"os"

	"github.com/shazow/virtle/backend/cloudhypervisor"
	"github.com/shazow/virtle/units"
	"github.com/shazow/virtle/vm"
)

// ExampleBackend boots a microVM from a PVH-capable vmlinux and a raw root
// disk, shares a host directory over virtio-fs, and prints the guest's
// serial console until the guest powers off (which exits Cloud Hypervisor)
// or Shutdown presses its power button.
func ExampleBackend() {
	ctx := context.Background()
	b := &cloudhypervisor.Backend{
		Console:       cloudhypervisor.ConsolePrint,
		ConsoleOutput: os.Stdout,
	}
	m, err := b.Start(ctx, &vm.Spec{
		CPUs:   2,
		Memory: 512 * units.Mebibyte,
		Kernel: vm.Kernel{Path: "vmlinux"},
		// The image mounted at "/" is the root device: virtle passes
		// root=/dev/vda ro for it, so no initrd is needed.
		Disks: []vm.Disk{{Path: "rootfs.ext4", Format: "raw", ReadOnly: true, GuestPath: "/"}},
		// virtle starts a virtiofsd for the share; the guest mounts it with
		// mount -t virtiofs src /mnt.
		Shares: []vm.Share{{Tag: "src", HostPath: ".", GuestPath: "/mnt"}},
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

	// Start returns once Cloud Hypervisor has accepted the boot request, not
	// when the guest is ready: watch the console (Machine.Console) for that.
	// Wait returns when the guest powers off, or after Shutdown.
	if err := m.Wait(ctx); err != nil {
		log.Print(err)
	}
}
