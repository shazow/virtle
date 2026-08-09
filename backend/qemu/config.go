package qemu

import (
	"log/slog"
	"time"
)

// Console selects what QEMU does with the guest serial console.
type Console string

const (
	// ConsoleOff gives the guest no serial console. It is the default.
	ConsoleOff Console = "off"

	// ConsolePrint writes guest console output to the host's standard error.
	ConsolePrint Console = "print"

	// ConsoleInteractive attaches the host terminal to the guest console.
	// The QEMU process then stays in the foreground process group so it can
	// read the terminal, which only makes sense for a program that owns one.
	ConsoleInteractive Console = "console"
)

// CIDRange is the inclusive range of vsock context IDs a backend may allocate
// from when starting a machine. The zero value uses the backend's default
// range.
type CIDRange struct {
	Min, Max int
}

// Config holds the QEMU-only knobs. The zero value works: every field falls
// back to the same default the virtle CLI applies.
//
// Nothing here appears in [github.com/shazow/virtle/vm] or
// [github.com/shazow/virtle/backend] — QEMU vocabulary stops at this package.
type Config struct {
	// Binary is the QEMU executable. Empty derives qemu-system-<arch> for
	// the host architecture.
	Binary string

	// Machine is the QEMU machine type. Empty means "microvm".
	Machine string

	// MachineOptions are merged into the resolved machine option list, as
	// QEMU -machine key=value pairs.
	MachineOptions map[string]string

	// CPU is the QEMU CPU model string. Empty derives one from the machine
	// type and host.
	CPU string

	// KVM enables or disables hardware acceleration. Nil derives it from the
	// host: on when the host can, off otherwise.
	KVM *bool

	// Console selects the guest serial console mode. Empty means
	// [ConsoleOff].
	Console Console

	// Seccomp enables QEMU's seccomp sandbox.
	Seccomp bool

	// ExtraArgs are appended to the QEMU command line verbatim, after every
	// argument virtle builds.
	ExtraArgs []string

	// HostName is the guest-visible machine name, and the prefix for the
	// runtime files derived from it. Empty means "virtle".
	HostName string

	// VirtiofsdBinary is the virtiofs daemon started for each of the spec's
	// shares. Empty means "virtiofsd" resolved from PATH.
	VirtiofsdBinary string

	// VirtiofsdArgs replaces the arguments passed to the virtiofs daemon.
	// Empty uses virtle's defaults, which wire the daemon to the socket and
	// host directory of each share.
	VirtiofsdArgs []string

	// CIDRange bounds the vsock CIDs the backend allocates from. The zero
	// value uses virtle's default range.
	CIDRange CIDRange

	// Balloon adds a virtio-balloon device, without which
	// backend.MemoryResizer cannot resize a running guest.
	Balloon bool

	// GuestAgentTimeout bounds the wait for guest control to answer, which
	// covers guest boot rather than a round trip. Zero uses the backend's
	// default of two minutes.
	GuestAgentTimeout time.Duration

	// Logger receives the backend's progress messages. Nil discards them.
	Logger *slog.Logger
}

func (c Config) guestAgentTimeout() time.Duration {
	if c.GuestAgentTimeout > 0 {
		return c.GuestAgentTimeout
	}
	return defaultGuestAgentTimeout
}

func (c Config) logger() *slog.Logger {
	if c.Logger != nil {
		return c.Logger
	}
	return slog.New(slog.DiscardHandler)
}
