// Package firecracker implements a virtle backend that launches microVMs with
// Firecracker. It boots a kernel (with an optional initrd) and raw disk
// images (creating missing ones from vm.Disk.Size), prints the guest serial
// console on request or serves it as a vm.Term, and offers the same lifecycle
// and status contract as backend/qemu.
//
// Firecracker runs only on Linux hosts (amd64 or arm64) with access to
// /dev/kvm; there is no software-emulation fallback. Guest kernels must match
// the host architecture: an ELF vmlinux on amd64, an uncompressed Image on
// arm64.
//
// There is no guest-control transport yet, so Machine.RemoteControl reports
// errors.ErrUnsupported and vm.Spec features that need one (Files, Shares,
// Ports) fail Start with an error wrapping errors.ErrUnsupported. The guest
// NIC, when Backend.Link asks for one, is a host TAP device the host kernel
// networks.
package firecracker

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"time"

	"github.com/shazow/virtle/backend"
	imanifest "github.com/shazow/virtle/internal/manifest"
	"github.com/shazow/virtle/units"
	"github.com/shazow/virtle/vm"
)

// DefaultMemory is the guest memory size used when vm.Spec.Memory is zero. A
// zero vm.Spec.CPUs selects every host CPU, as with QEMU, within
// Firecracker's limit of 32 vCPUs.
const DefaultMemory = 1024 * units.Mebibyte

// Console selects how the guest serial console is wired.
type Console string

const (
	ConsoleOff   Console = "off"   // no serial console output (default)
	ConsolePrint Console = "print" // guest console output printed to ConsoleOutput
)

// Backend starts Firecracker microVMs. The zero value works: it runs the
// firecracker executable from PATH. Canceling the context passed to Start
// kills the returned machine. Fields must not be modified after Start is
// first called.
type Backend struct {
	Binary          string        // firecracker executable; default: "firecracker" from PATH
	ExtraArgs       []string      // passthrough Firecracker arguments, after virtle's own; no shell expansion
	StartupTimeout  time.Duration // bound on API startup and configuration; default: 10s
	ShutdownTimeout time.Duration // bound on graceful Shutdown before the VMM is killed; default: 10s
	Console         Console       // serial console wiring; the zero value keeps the manifest's kernel.serial (default ConsoleOff)
	HostName        string        // VM name; also names the state lock shared with QEMU; default: "virtle"

	// Link gives the guest a NIC: TAP hands a host TAP device to Firecracker
	// for the host kernel to network. Nil means no NIC, as does a manifest
	// without [[networks]]; a manifest.Load backend follows its manifest's
	// [[networks]] type = "tap" entries instead. Status reports the NIC
	// with its MAC; the host owns its addressing.
	Link Link

	// Logger receives lifecycle logs. The default discards logs.
	Logger *slog.Logger

	// ConsoleOutput receives guest console output when Console is
	// ConsolePrint. The default is os.Stderr.
	ConsoleOutput io.Writer

	doc *imanifest.Document // base document of a manifest.Load backend; nil when configured in Go
}

// Start implements backend.Backend: it lowers spec through the manifest
// pipeline (overlaid on the loaded document for manifest.Load backends),
// launches Firecracker, configures it over its API, and returns once the
// InstanceStart action is accepted. That is not guest readiness: observe the
// workload itself (for example a marker on the serial console). Canceling
// ctx kills the machine.
func (b *Backend) Start(ctx context.Context, spec *vm.Spec) (backend.Machine, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	// A Go-configured backend without a Spec.Dir keeps its lock and control
	// socket in a temporary state directory that is removed when the machine
	// exits, so nothing lands in the process working directory.
	ephemeralState := ""
	if b.doc == nil && (spec == nil || spec.Dir == "") {
		dir, err := os.MkdirTemp("", "virtle-state-")
		if err != nil {
			return nil, fmt.Errorf("create state directory: %w", err)
		}
		ephemeralState = dir
	}
	mf, err := b.resolveSpec(spec, ephemeralState)
	if err != nil {
		if ephemeralState != "" {
			_ = os.RemoveAll(ephemeralState)
		}
		return nil, err
	}
	return b.start(ctx, mf, ephemeralState)
}

func (b *Backend) logger() *slog.Logger {
	if b.Logger != nil {
		return b.Logger
	}
	return slog.New(slog.DiscardHandler)
}

func (b *Backend) consoleOutput() io.Writer {
	if b.ConsoleOutput != nil {
		return b.ConsoleOutput
	}
	return os.Stderr
}

// NewBackendFromDocument is the bridge for the public manifest package:
// the returned backend starts from the loaded document and overlays the Spec
// passed to Start on top. The document type is internal, so this is not
// callable (and not supported) outside the module.
func NewBackendFromDocument(doc imanifest.Document, b Backend) backend.Backend {
	b.doc = &doc
	return &b
}

var _ backend.Backend = (*Backend)(nil)
