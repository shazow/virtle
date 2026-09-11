package cloudhypervisor

import (
	"errors"
	"fmt"

	imanifest "github.com/shazow/virtle/internal/manifest"
	"github.com/shazow/virtle/internal/vmmhost"
	"github.com/shazow/virtle/units"
	"github.com/shazow/virtle/vm"
)

// resolveSpec lowers a neutral vm.Spec plus the Backend configuration onto the
// internal manifest document (overlaid on the loaded document for
// manifest.Load backends) and resolves it. A non-empty stateDir replaces the
// document's state directory. Shares become virtiofs mounts; Spec features
// that need a guest transport or QEMU-only machinery are rejected with
// errors.ErrUnsupported rather than silently dropped.
func (b *Backend) resolveSpec(spec *vm.Spec, stateDir string) (*imanifest.Manifest, error) {
	if spec == nil {
		spec = &vm.Spec{}
	}
	switch b.Console {
	case "", ConsoleOff, ConsolePrint:
	default:
		return nil, fmt.Errorf("cloud-hypervisor: Console %q is not ConsoleOff or ConsolePrint: %w", b.Console, errors.ErrUnsupported)
	}
	doc := imanifest.Document{Backend: imanifest.BackendCloudHypervisor}
	if b.doc != nil {
		doc = *b.doc
	}
	if err := vmmhost.ApplySpec(&doc, spec, vmmhost.SpecOptions{
		Backend:       "cloud-hypervisor",
		MaxCPUs:       imanifest.MaxCloudHypervisorCPUs,
		DefaultMemory: DefaultMemory,
		Loaded:        b.doc != nil,
		Shares:        true,
		HostName:      b.HostName,
		StateDir:      stateDir,
	}); err != nil {
		return nil, err
	}
	if b.Binary != "" {
		doc.CloudHypervisor.Binary = b.Binary
	}
	if b.StartupTimeout != 0 {
		doc.CloudHypervisor.StartupTimeout = units.Duration(b.StartupTimeout)
	}
	if b.ShutdownTimeout != 0 {
		doc.CloudHypervisor.ShutdownTimeout = units.Duration(b.ShutdownTimeout)
	}
	if b.Console != "" {
		doc.Kernel.Serial = string(b.Console)
	}
	if err := applySpecLink(&doc, b.Link); err != nil {
		return nil, err
	}
	// Share resolution probes existing sockets and logs what it finds.
	mf, err := doc.ManifestWithOptions(imanifest.ResolveOptions{Logger: b.logger().With("package", "manifest")})
	if err != nil {
		return nil, fmt.Errorf("resolve vm spec: %w", err)
	}
	return mf, nil
}

// applySpecLink lowers Backend.Link onto the document's NICs: a TAP makes
// every declared network (or one default entry) a tap network on that
// device. A nil Link leaves the manifest's own entries in place.
func applySpecLink(doc *imanifest.Document, link Link) error {
	if link == nil {
		return nil
	}
	tap, ok := link.(TAP)
	if !ok {
		return fmt.Errorf("cloud-hypervisor: Link %T: %w", link, errors.ErrUnsupported)
	}
	if tap.Name == "" {
		return fmt.Errorf("cloud-hypervisor: Link TAP requires the device Name")
	}
	vmmhost.ApplyTAP(doc, tap.Name)
	return nil
}
