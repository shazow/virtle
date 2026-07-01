package manifest

const (
	defaultKernelPath       = "<kernel-path>"
	defaultKernelInitrdPath = "<initrd-path>"
)

// DefaultManifest returns the fully resolved manifest defaults. Since kernel
// paths are required inputs and have no default, placeholder values are used so
// resolution can show all derived runtime defaults.
func DefaultManifest() (*Manifest, error) {
	document := DefaultDocument()
	document.Kernel.Path = defaultKernelPath
	document.Kernel.InitrdPath = defaultKernelInitrdPath
	return document.Manifest()
}
