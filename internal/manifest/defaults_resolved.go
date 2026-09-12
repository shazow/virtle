package manifest

const defaultKernelPath = "<kernel-path>"

// DefaultManifest returns the fully resolved manifest defaults. The required
// kernel path uses a placeholder so resolution can show the runtime defaults.
func DefaultManifest() (*Manifest, error) {
	document := DefaultDocument()
	document.Kernel.Path = defaultKernelPath
	return document.Manifest()
}
