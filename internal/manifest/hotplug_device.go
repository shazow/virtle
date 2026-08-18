package manifest

// HotplugKind identifies the kind of a resolved hotplug device.
type HotplugKind string

const (
	HotplugKindVirtioFS HotplugKind = "virtiofs"
	HotplugKindNet      HotplugKind = "net"
	HotplugKindBlock    HotplugKind = "block"
)

// HotplugDevice is a resolved hotplug device description. It is the
// manifest's vocabulary for devices that attach to and detach from a
// running VM; backends turn it into their own attach mechanism.
type HotplugDevice struct {
	Kind     HotplugKind     `json:"kind"`
	ID       string          `json:"id"`
	VirtioFS HotplugVirtioFS `json:"virtiofs,omitempty"`
	Net      HotplugNet      `json:"net,omitempty"`
	Block    HotplugBlock    `json:"block,omitempty"`
}

type HotplugVirtioFS struct {
	Source     string   `json:"source"`
	Target     string   `json:"target,omitempty"`
	SocketPath string   `json:"socketPath"`
	Bin        string   `json:"bin"`
	Args       []string `json:"args,omitempty"`
}

type HotplugNet struct {
	Backend string           `json:"backend"`
	MAC     string           `json:"mac"`
	Forward []HotplugForward `json:"forward,omitempty"`
}

type HotplugForward struct {
	Proto string `json:"proto"`
	Host  string `json:"host"`
	Guest string `json:"guest"`
}

type HotplugBlock struct {
	ImagePath string `json:"imagePath"`
	Format    string `json:"format"`
	ReadOnly  bool   `json:"readOnly,omitempty"`
	Serial    string `json:"serial,omitempty"`
}

// DefaultVirtioFSArgs is the default virtiofsd argument list for a
// virtiofs hotplug device.
func DefaultVirtioFSArgs(socketPath string, source string, id string) []string {
	return []string{
		"--socket-path=" + socketPath,
		"--shared-dir=" + source,
		"--tag=" + id,
	}
}
