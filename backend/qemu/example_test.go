package qemu_test

import (
	"context"
	"fmt"
	"log"
	"os"
	"time"

	"github.com/shazow/virtle/backend"
	"github.com/shazow/virtle/backend/qemu"
	"github.com/shazow/virtle/manifest"
	"github.com/shazow/virtle/units"
	"github.com/shazow/virtle/vm"
)

// Boot a machine, run a command in it, tear it down.
func Example() {
	ctx := context.Background()

	spec := &vm.Spec{
		Kernel: vm.Kernel{Path: "vmlinuz", Initrd: "initrd.img"},
		Shares: []vm.Share{{Tag: "src", HostPath: ".", GuestPath: "/workspace"}},
		Memory: 2048 * units.Mebibyte,
	}
	b, err := qemu.BackendWithQGA(qemu.Config{})
	if err != nil {
		log.Fatal(err)
	}
	inst, err := b.Start(ctx, spec)
	if err != nil {
		log.Fatal(err)
	}
	defer backend.Shutdown(ctx, inst)

	guest, err := inst.RemoteControl()
	if err != nil {
		log.Fatal(err) // this VM has no guest agent
	}
	out, err := guest.Run(ctx, &vm.GuestCmd{Path: "make", Dir: "/workspace"})
	if err != nil {
		log.Fatal(err)
	}
	fmt.Printf("exit=%d\n%s", out.ExitCode, out.Stdout)
}

// Suspend the machine if the backend can, and stop it gracefully if it cannot.
// Optional functionality is a package-qualified type assertion, the way
// database/sql consumers reach driver capabilities.
func Example_optionalCapabilities() {
	ctx := context.Background()
	b, _ := qemu.BackendWithQGA(qemu.Config{})
	inst, _ := b.Start(ctx, &vm.Spec{Kernel: vm.Kernel{Path: "vmlinuz"}})
	stateDir := "/var/lib/virtle/suspended"

	if suspender, ok := b.(backend.Suspender); ok {
		if err := suspender.Suspend(ctx, inst, stateDir); err != nil {
			log.Fatal(err)
		}
		// Later, possibly from another process:
		inst, _ = suspender.Resume(ctx, &vm.Spec{Kernel: vm.Kernel{Path: "vmlinuz"}}, stateDir)
		_ = inst
		return
	}
	if err := backend.Shutdown(ctx, inst); err != nil {
		log.Fatal(err)
	}
}

// Give the guest more memory while it runs, when the machine has a balloon.
func Example_resizeMemory() {
	ctx := context.Background()
	b, _ := qemu.BackendWithQGA(qemu.Config{Balloon: true})
	inst, _ := b.Start(ctx, &vm.Spec{Kernel: vm.Kernel{Path: "vmlinuz"}, Memory: 2048 * units.Mebibyte})
	defer backend.Shutdown(ctx, inst)

	if resizer, ok := b.(backend.MemoryResizer); ok {
		if err := resizer.ResizeMemory(ctx, inst, 4*units.Gibibyte); err != nil {
			log.Fatal(err)
		}
	}
}

// Start from a manifest — what the CLI describes, loaded as a library.
func Example_manifest() {
	ctx := context.Background()

	file, err := os.Open("manifest.toml")
	if err != nil {
		log.Fatal(err)
	}
	defer file.Close()

	spec, b, err := manifest.Load(file)
	if err != nil {
		log.Fatal(err)
	}
	inst, err := b.Start(ctx, spec)
	if err != nil {
		log.Fatal(err)
	}

	// Give the guest time to flush and unmount before it is killed.
	stopCtx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	defer backend.Shutdown(stopCtx, inst)

	if err := inst.Wait(ctx); err != nil {
		log.Fatal(err)
	}
}
