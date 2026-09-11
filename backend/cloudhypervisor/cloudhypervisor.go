// Package cloudhypervisor implements a virtle backend that launches microVMs
// with Cloud Hypervisor. It boots a kernel (with an optional initrd) and raw
// disk images (creating missing ones from vm.Disk.Size), shares host
// directories over virtio-fs (starting a virtiofsd per share), prints the
// guest serial console on request or serves it as a vm.Term, and offers the
// same lifecycle and status contract as backend/qemu and backend/firecracker.
//
// Cloud Hypervisor runs only on Linux hosts (amd64 or arm64) with access to
// /dev/kvm; there is no software-emulation fallback. Every device is
// virtio-PCI, so guest kernels need PCI and ACPI support besides the usual
// virtio drivers: an ELF vmlinux built with CONFIG_PVH or a bzImage on
// amd64, an uncompressed Image on arm64.
//
// There is no guest-control transport yet, so Machine.RemoteControl reports
// errors.ErrUnsupported and vm.Spec features that need one (Files, Ports)
// fail Start with an error wrapping errors.ErrUnsupported. The guest NIC,
// when Backend.Link asks for one, is a host TAP device the host kernel
// networks.
package cloudhypervisor

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
// zero vm.Spec.CPUs selects every host CPU, as with QEMU.
const DefaultMemory = 1024 * units.Mebibyte

// Console selects how the guest serial console is wired.
type Console string

const (
	ConsoleOff   Console = "off"   // no serial console output (default)
	ConsolePrint Console = "print" // guest console output printed to ConsoleOutput
)

// Backend starts Cloud Hypervisor microVMs. The zero value works: it runs the
// cloud-hypervisor executable from PATH, and virtiofsd from PATH for shares.
// Canceling the context passed to Start kills the returned machine. Fields
// must not be modified after Start is first called.
type Backend struct {
	Binary          string        // cloud-hypervisor executable; default: "cloud-hypervisor" from PATH
	StartupTimeout  time.Duration // bound on API startup, share daemons, and configuration; default: 10s
	ShutdownTimeout time.Duration // bound on graceful Shutdown before the VMM is killed; default: 10s
	Console         Console       // serial console wiring; the zero value keeps the manifest's kernel.serial (default ConsoleOff)
	HostName        string        // VM name; also names the state lock shared with the other backends; default: "virtle"

	// Link gives the guest a NIC: TAP hands a host TAP device to Cloud
	// Hypervisor for the host kernel to network. Nil means no NIC, as does
	// a manifest without [[networks]]; a manifest.Load backend follows its
	// manifest's [[networks]] type = "tap" entries instead. Status reports
	// the NIC with its MAC; the host owns its addressing.
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
// starts a virtiofsd per share, launches Cloud Hypervisor, creates and boots
// the VM over its API, and returns once the boot request is accepted. That
// is not guest readiness: observe the workload itself (for example a marker
// on the serial console). Canceling ctx kills the machine.
func (b *Backend) Start(ctx context.Context, spec *vm.Spec) (backend.Machine, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	// A Go-configured backend without a Spec.Dir keeps its lock, control
	// socket, and share sockets in a temporary state directory that is
	// removed when the machine exits, so nothing lands in the process
	// working directory.
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
