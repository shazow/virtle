// Package vm holds the consumer-facing vocabulary for describing and driving
// virtual machines: [Spec] describes a machine, [Guest] operates inside a
// running one.
//
// Nothing here starts a VM. Starting one takes a backend implementation, which
// lives under github.com/shazow/virtle/backend — the same split as
// database/sql and database/sql/driver. A consumer builds a Spec here, hands
// it to a backend it names explicitly, and gets back an instance whose guest
// control is a [Guest]:
//
//	spec := &vm.Spec{
//		Kernel: vm.Kernel{Path: "vmlinuz", Initrd: "initrd.img"},
//		Memory: 2048 * units.Mebibyte,
//	}
//	b, err := qemu.BackendWithQGA(qemu.Config{})
//	inst, err := b.Start(ctx, spec)
//	g, err := inst.RemoteControl()
//	out, err := g.Run(ctx, &vm.GuestCmd{Path: "make", Dir: "/workspace"})
//
// The types here name no hypervisor, protocol, or host process: the same Spec
// describes a VM for an exec'd backend such as QEMU and for an in-process one
// such as libkrun.
//
// This package is experimental. The module is untagged v0 and the API may
// change until it settles.
package vm
