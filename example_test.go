package main_test

import (
	"context"
	"fmt"
	"log"
	"os"

	"github.com/shazow/virtle/backend/qemu"
	"github.com/shazow/virtle/units"
	"github.com/shazow/virtle/vm"
)

func Example() {
	ctx := context.Background()
	spec := &vm.Spec{
		Kernel: vm.Kernel{Path: "vmlinuz", Initrd: "initrd.img"},
		Shares: []vm.Share{{
			Tag:       "src",
			HostPath:  ".",
			GuestPath: "/workspace",
		}},
		Memory: 2048 * units.Mebibyte,
	}

	b := &qemu.Backend{RemoteControl: qemu.QGA{}}
	inst, err := b.Start(ctx, spec)
	if err != nil {
		log.Print(err)
		return
	}
	defer func() {
		if err := inst.Shutdown(ctx); err != nil {
			log.Print(err)
		}
	}()

	guest, err := inst.RemoteControl()
	if err != nil {
		log.Print(err)
		return
	}
	// A non-zero exit is an error (*vm.ExitError), as with exec.Cmd.Run.
	err = guest.Run(ctx, &vm.GuestCmd{
		Path:   "make",
		Dir:    "/workspace",
		Stdout: os.Stdout,
	})
	if err != nil {
		log.Print(err)
		return
	}
	fmt.Println("make succeeded")
}
