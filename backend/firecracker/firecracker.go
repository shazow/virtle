// Package firecracker implements direct Linux/KVM microVM boot using the
// Firecracker HTTP API. It supports raw block devices and serial output.
// Firecracker has no QEMU guest agent; RemoteControl reports ErrUnsupported.
package firecracker

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"runtime"
	"time"

	"github.com/shazow/virtle/backend"
	imanifest "github.com/shazow/virtle/internal/manifest"
	"github.com/shazow/virtle/vm"
)

// Backend configures Firecracker. The zero value selects the firecracker
// executable from PATH. Fields must not change while Start is in use.
// The host must be Linux amd64 or arm64 with accessible KVM. Images must
// match the host architecture; Firecracker does not emulate other CPUs.
type Backend struct {
	Binary          string
	StartupTimeout  time.Duration // zero: 10s
	ShutdownTimeout time.Duration // zero: 10s, further bounded by Shutdown's context
	ConsoleOutput   io.Writer     // serial output when Console is "print"; default os.Stderr
	Console         string        // "off" (default) or "print"; kernel cmdline controls the guest console
	Logger          *slog.Logger  // nil discards lifecycle diagnostics
	doc             *imanifest.Document
}

// Start configures and starts a microVM. Success means InstanceStart was
// accepted, not that the guest workload is ready. Canceling ctx kills the VM.
func (b *Backend) Start(ctx context.Context, spec *vm.Spec) (backend.Machine, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if runtime.GOOS != "linux" || (runtime.GOARCH != "amd64" && runtime.GOARCH != "arm64") {
		return nil, fmt.Errorf("firecracker requires Linux amd64 or arm64 with KVM")
	}
	mf, err := b.resolveSpec(spec)
	if err != nil {
		return nil, err
	}
	return b.start(ctx, mf, b.doc == nil && (spec == nil || spec.Dir == ""))
}

// NewBackendFromDocument is the module-internal bridge used by manifest.Load.
func NewBackendFromDocument(doc imanifest.Document) backend.Backend {
	return &Backend{doc: &doc}
}

var _ backend.Backend = (*Backend)(nil)
