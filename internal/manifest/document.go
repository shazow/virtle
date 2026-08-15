package manifest

import (
	"github.com/shazow/virtle/internal/manifest/tagged"
	"github.com/shazow/virtle/internal/units"
)

const (
	KernelSerialOff     = "off"
	KernelSerialPrint   = "print"
	KernelSerialConsole = "console"

	defaultSSHCommand = "ssh"
	defaultNetworkID  = "microvm1"
	defaultNetworkMAC = "02:02:00:00:00:01"
)

type Document struct {
	HostName      string             `json:"host_name,omitempty" toml:"host_name" default:"virtle" jsonschema:"Guest-visible VM name used for QEMU naming and derived runtime files."`
	WorkingDir    string             `json:"working_dir,omitempty" toml:"working_dir" default:"." jsonschema:"Host working directory used to resolve relative paths in the manifest."`
	StateDir      string             `json:"state_dir,omitempty" toml:"state_dir" default:".virtle" jsonschema:"Host directory used for runtime state such as locks sockets and generated files."`
	Host          HostInput          `json:"host,omitempty" toml:"host" jsonschema:"Host platform facts used while resolving QEMU defaults."`
	QEMU          QEMUInput          `json:"qemu,omitempty" toml:"qemu" jsonschema:"QEMU executable and host-side socket settings."`
	Machine       MachineInput       `json:"machine,omitempty" toml:"machine" jsonschema:"Virtual machine type CPU memory and acceleration settings."`
	Kernel        KernelInput        `json:"kernel" toml:"kernel" jsonschema:"Guest kernel initrd and kernel command-line settings."`
	Graphics      *GraphicsInput     `json:"graphics,omitempty" toml:"graphics" jsonschema:"Graphical display backend settings."`
	Mounts        MountsInput        `json:"mounts,omitempty" toml:"mounts" jsonschema:"Storage and filesystem devices attached at launch."`
	Workspace     WorkspaceInput     `json:"workspace,omitempty" toml:"workspace" jsonschema:"Workspace directories made available to guest-side templates and helpers."`
	Networks      []NetworkInput     `json:"networks,omitempty" toml:"networks" jsonschema:"Network devices and port forwards attached at launch."`
	Balloon       *BalloonInput      `json:"balloon,omitempty" toml:"balloon" jsonschema:"Virtio memory balloon device and optional controller settings."`
	SSH           SSHInput           `json:"ssh,omitempty" toml:"ssh" jsonschema:"SSH command and readiness settings for attaching to the guest."`
	VSock         VSockInput         `json:"vsock,omitempty" toml:"vsock" jsonschema:"Allowed runtime vsock CID allocation range."`
	WriteFiles    []WriteFileInput   `json:"write_files,omitempty" toml:"write_files" jsonschema:"Files copied into or synchronized with the guest through qemu guest agent."`
	Notifications NotificationsInput `json:"notifications,omitempty" toml:"notifications" jsonschema:"Host command hooks invoked for selected runtime notification states."`
	Run           []RunInput         `json:"run,omitempty" toml:"run" jsonschema:"Host-side processes started before QEMU and stopped during teardown."`
	Hotplug       HotplugInput       `json:"hotplug,omitempty" toml:"hotplug" jsonschema:"Devices that may be attached or detached after launch."`
}

type HostInput struct {
	OS     string `json:"-" toml:"-"`
	Arch   string `json:"-" toml:"-"`
	System string `json:"-" toml:"-"`
}

type QEMUInput struct {
	Exec                []string          `json:"exec,omitempty" toml:"exec" jsonschema:"Command template used to launch QEMU; the first element is the QEMU binary."`
	FwdTunnelExec       []string          `json:"fwd_tunnel_exec,omitempty" toml:"fwd_tunnel_exec" jsonschema:"Command template QEMU uses to forward host or guest ports."`
	User                string            `json:"user,omitempty" toml:"user" jsonschema:"Host user used for QEMU-related process policy when supported."`
	Seccomp             bool              `json:"seccomp,omitempty" toml:"seccomp" jsonschema:"Enable QEMU seccomp sandboxing."`
	MachineOptions      map[string]string `json:"machine_options,omitempty" toml:"machine_options" jsonschema:"Additional QEMU machine options merged into the resolved machine option list."`
	QMPSocket           string            `json:"qmp_socket,omitempty" toml:"qmp_socket" default:"qmp.sock" jsonschema:"Path to the QEMU Machine Protocol socket relative to the runtime state directory unless absolute."`
	GuestAgentSocket    string            `json:"guest_agent_socket,omitempty" toml:"guest_agent_socket" default:"qga.sock" jsonschema:"Path to the QEMU guest agent socket relative to the runtime state directory unless absolute."`
	GuestDefaultTimeout units.Duration    `json:"guest_default_timeout,omitempty" toml:"guest_default_timeout" default:"30s" jsonschema:"Timeout for guest agent commands virtle issues itself such as file writes and workspace mounts. Guest-exec requests sent through the control socket carry their own timeout. Zero disables the timeout."`
	ShutdownExec        []string          `json:"shutdown_exec,omitempty" toml:"shutdown_exec" jsonschema:"Optional guest command tuple invoked through QGA to shut down the VM gracefully."`
	ShutdownTimeout     units.Duration    `json:"shutdown_timeout,omitempty" toml:"shutdown_timeout" default:"90s" jsonschema:"Duration to wait for graceful shutdown before forcing QEMU to quit"`
	HotplugPorts        int               `json:"hotplug_ports,omitempty" toml:"hotplug_ports" jsonschema:"Reserve at least this many PCIe hotplug root ports; defaults to the number of listed hotplug devices. Extra ports allow devices to be attached that the manifest does not describe."`
}

type MachineInput struct {
	Type   string    `json:"type,omitempty" toml:"type" default:"microvm" jsonschema:"QEMU machine type to use when resolving device transports."`
	VCPU   int       `json:"vcpu,omitempty" toml:"vcpu" jsonschema:"Number of virtual CPUs to expose to the guest; zero derives the count."`
	ID     string    `json:"id,omitempty" toml:"id" jsonschema:"Optional machine identifier passed through to QEMU."`
	Memory units.MiB `json:"memory,omitempty" toml:"memory" default:"1024" jsonschema:"Guest memory size in MiB."`
	CPU    string    `json:"cpu,omitempty" toml:"cpu" jsonschema:"QEMU CPU model string."`
	// Pointer preserves omitted vs explicitly false input until resolution.
	KVM *bool `json:"kvm,omitempty" toml:"kvm" jsonschema:"Whether QEMU should enable KVM acceleration when supported."`
}

type KernelInput struct {
	Path       string   `json:"path" toml:"path" jsonschema:"Path to the guest kernel image."`
	InitrdPath string   `json:"initrd_path" toml:"initrd_path" jsonschema:"Path to the guest initrd image."`
	Params     []string `json:"params,omitempty" toml:"params" jsonschema:"Additional kernel command-line parameters appended after virtle defaults."`
	Serial     string   `json:"serial,omitempty" toml:"serial" default:"off" jsonschema:"Serial console mode: off print or console."`
}

type GraphicsInput struct {
	Backend string `json:"backend,omitempty" toml:"backend" jsonschema:"Display backend; headless disables graphical output."`
}

type MountsInput []MountEntry

func (m MountsInput) RequiresPCI() bool {
	return len(m.VirtioFS()) > 0 || len(m.NineP()) > 0
}

type MountEntry interface {
	mountEntry()
	mountType() string
	resolveQEMUMount(*mountResolveContext) QEMUMountDevice
}

const (
	MountTypeVirtioFS = "virtiofs"
	MountTypeNineP    = "9p"
	MountTypeImage    = "image"
)

var mountRegistry = tagged.Registry[MountEntry]{
	tagged.Value[MountEntry, VirtioFSMountInput](MountTypeVirtioFS),
	tagged.Value[MountEntry, NinePMountInput](MountTypeNineP),
	tagged.Value[MountEntry, ImageMountInput](MountTypeImage),
}

func (m MountsInput) VirtioFS() []VirtioFSMountInput {
	return filterMounts[VirtioFSMountInput](m)
}

func (m MountsInput) NineP() []NinePMountInput {
	return filterMounts[NinePMountInput](m)
}

func (m MountsInput) Image() []ImageMountInput {
	return filterMounts[ImageMountInput](m)
}

func (m *MountsInput) UnmarshalJSON(data []byte) error {
	mounts, err := tagged.DecodeJSONList(data, "manifest.mounts", mountRegistry)
	*m = mounts
	return err
}

func (m MountsInput) MarshalJSON() ([]byte, error) {
	return tagged.MarshalJSONList(m, func(mount MountEntry) string {
		return mount.mountType()
	})
}

func (m *MountsInput) UnmarshalTOML(data any) error {
	mounts, err := tagged.DecodeTOMLList(data, "manifest.mounts", mountRegistry)
	*m = mounts
	return err
}

func filterMounts[T MountEntry](mounts MountsInput) []T {
	filtered := make([]T, 0, len(mounts))
	for _, mount := range mounts {
		if typed, ok := mount.(T); ok {
			filtered = append(filtered, typed)
		}
	}
	return filtered
}

type MountInput struct {
	Tag        string `json:"tag" toml:"tag" jsonschema:"Stable QEMU mount tag or device identifier."`
	SourcePath string `json:"source,omitempty" toml:"source" jsonschema:"Host path or image path backing this mount."`
	ReadOnly   bool   `json:"read_only,omitempty" toml:"read_only" jsonschema:"Attach the mount or image read-only."`
}

type VirtioFSMountInput struct {
	Type string `json:"type" toml:"type" jsonschema:"Mount kind; must be virtiofs for this entry."`
	MountInput
	Target string `json:"target,omitempty" toml:"target" jsonschema:"Optional guest mount target used for hotplug guest mount commands."`

	VirtioFS VirtioFSInput `json:"virtiofs,omitempty" toml:"virtiofs" jsonschema:"Virtiofs daemon socket and command settings."`
}

func (VirtioFSMountInput) mountEntry() {}

func (VirtioFSMountInput) mountType() string { return MountTypeVirtioFS }

type VirtioFSInput struct {
	Socket string   `json:"socket,omitempty" toml:"socket" jsonschema:"Virtiofs daemon socket path relative to the runtime state directory unless absolute."`
	Bin    string   `json:"bin,omitempty" toml:"bin" jsonschema:"Virtiofs daemon executable path or wrapper script."`
	Args   []string `json:"args,omitempty" toml:"args" jsonschema:"Arguments for the virtiofs daemon command."`
}

type NinePMountInput struct {
	Type string `json:"type" toml:"type" jsonschema:"Mount kind; must be 9p for this entry."`
	MountInput

	NineP NinePInput `json:"9p,omitempty" toml:"9p" jsonschema:"9p-specific mount options."`
}

func (NinePMountInput) mountEntry() {}

func (NinePMountInput) mountType() string { return MountTypeNineP }

type NinePInput struct {
	SecurityModel string `json:"security_model,omitempty" toml:"security_model" jsonschema:"QEMU 9p security model."`
}

type ImageMountInput struct {
	Type       string     `json:"type" toml:"type" jsonschema:"Mount kind; must be image for this entry."`
	SourcePath string     `json:"source" toml:"source" jsonschema:"Host disk image path."`
	ReadOnly   bool       `json:"read_only,omitempty" toml:"read_only" jsonschema:"Attach the image read-only."`
	Image      ImageInput `json:"image,omitempty" toml:"image" jsonschema:"Disk image creation and format settings."`
}

func (ImageMountInput) mountEntry() {}

func (ImageMountInput) mountType() string { return MountTypeImage }

type ImageInput struct {
	Size       units.MiB `json:"size,omitempty" toml:"size" jsonschema:"Image size in MiB when creating the image."`
	FSType     string    `json:"fs,omitempty" toml:"fs" jsonschema:"Filesystem type used when creating the image."`
	Format     string    `json:"format,omitempty" toml:"format" jsonschema:"QEMU image format such as raw or qcow2."`
	AutoCreate bool      `json:"create,omitempty" toml:"create" jsonschema:"Create and format the image if it does not exist."`
	// Pointer preserves omitted vs explicitly empty input until resolution.
	Label  *string `json:"label,omitempty" toml:"label" jsonschema:"Optional filesystem label used when creating the image."`
	Direct bool    `json:"direct,omitempty" toml:"direct" jsonschema:"Use direct I/O cache settings for the block device."`
	// Pointer preserves omitted vs explicitly empty input until resolution.
	Serial *string `json:"serial,omitempty" toml:"serial" jsonschema:"Optional disk serial exposed to the guest."`
}

type WorkspaceInput struct {
	GuestDir string `json:"guest_dir,omitempty" toml:"guest_dir" jsonschema:"Guest workspace directory used in templates."`
	HostDir  string `json:"host_dir,omitempty" toml:"host_dir" jsonschema:"Host workspace directory used in templates."`
	MountCWD bool   `json:"mount_cwd,omitempty" toml:"mount_cwd" jsonschema:"Mount the current working directory into the guest after launch."`
}

type NetworkInput struct {
	ID      string        `json:"id,omitempty" toml:"id" jsonschema:"QEMU network device identifier."`
	Type    string        `json:"type,omitempty" toml:"type" jsonschema:"Network backend type."`
	MAC     string        `json:"mac,omitempty" toml:"mac" jsonschema:"Guest network interface MAC address."`
	Forward []ForwardPort `json:"forward,omitempty" toml:"forward" jsonschema:"Port forwarding rules for this network backend."`
}

type ForwardPort struct {
	Proto string `json:"proto" toml:"proto" jsonschema:"Transport protocol for the forwarded port."`
	From  string `json:"from" toml:"from" jsonschema:"Forwarding direction: host or guest."`
	Host  string `json:"host" toml:"host" jsonschema:"Host endpoint address in address:port form."`
	Guest string `json:"guest" toml:"guest" jsonschema:"Guest endpoint address in address:port form."`
}

type PortEndpoint struct {
	Address string `json:"address" toml:"address"`
	Port    int    `json:"port" toml:"port"`
}

type BalloonInput struct {
	Enabled      bool `json:"enabled,omitempty" toml:"enabled" jsonschema:"Enable the virtio memory balloon device."`
	DeflateOnOOM bool `json:"deflate_on_oom,omitempty" toml:"deflate_on_oom" jsonschema:"Allow the guest to deflate the balloon under out-of-memory pressure."`
	// Pointer preserves omitted vs explicitly false input until resolution.
	FreePageReporting *bool                   `json:"free_page_reporting,omitempty" toml:"free_page_reporting" jsonschema:"Enable free page reporting for the balloon device; defaults to enabled."`
	Controller        *BalloonControllerInput `json:"controller,omitempty" toml:"controller" jsonschema:"Optional host-side balloon controller thresholds and polling settings."`
}

type BalloonControllerInput struct {
	MinActual             units.MiB      `json:"min_actual,omitempty" toml:"min_actual" jsonschema:"Minimum guest memory target in MiB."`
	MaxActual             units.MiB      `json:"max_actual,omitempty" toml:"max_actual" jsonschema:"Maximum guest memory target in MiB."`
	GrowBelowAvailable    units.MiB      `json:"grow_below_available,omitempty" toml:"grow_below_available" jsonschema:"Grow guest memory when available guest memory falls below this MiB threshold."`
	ReclaimAboveAvailable units.MiB      `json:"reclaim_above_available,omitempty" toml:"reclaim_above_available" jsonschema:"Reclaim guest memory when available guest memory rises above this MiB threshold."`
	Step                  units.MiB      `json:"step,omitempty" toml:"step" jsonschema:"Memory adjustment step size in MiB."`
	PollInterval          units.Duration `json:"poll_interval,omitempty" toml:"poll_interval" jsonschema:"Delay between balloon controller polling cycles as a duration such as 5s."`
	ReclaimHoldoff        units.Duration `json:"reclaim_holdoff,omitempty" toml:"reclaim_holdoff" jsonschema:"Wait after growing memory before reclaiming again as a duration such as 30s."`
}

type SSHInput struct {
	Exec          []string       `json:"exec,omitempty" toml:"exec" jsonschema:"SSH command template used to attach to the guest."`
	User          string         `json:"user,omitempty" toml:"user" default:"agent" jsonschema:"Guest SSH username."`
	ReadySocket   string         `json:"ready_socket,omitempty" toml:"ready_socket" jsonschema:"Guest readiness socket path relative to the runtime state directory unless absolute."`
	RetryDelay    units.Duration `json:"retry_delay,omitempty" toml:"retry_delay" default:"500ms" jsonschema:"Delay between SSH readiness or connection retry attempts; must be greater than zero."`
	Autoprovision bool           `json:"autoprovision,omitempty" toml:"autoprovision" jsonschema:"Automatically provision an SSH key after authentication failure."`
}

type VSockInput struct {
	CIDRange RangeInput `json:"cid_range,omitempty" toml:"cid_range" jsonschema:"Inclusive range of vsock CIDs virtle may allocate at launch."`
}

type RangeInput struct {
	Min int `json:"min,omitempty" toml:"min" jsonschema:"Inclusive minimum value."`
	Max int `json:"max,omitempty" toml:"max" jsonschema:"Inclusive maximum value."`
}

type WriteFileInput struct {
	GuestPath string `json:"guest_path" toml:"guest_path" jsonschema:"Absolute guest path to write."`
	// Pointer preserves omitted vs explicitly empty input until resolution.
	Chown *string `json:"chown,omitempty" toml:"chown" jsonschema:"Optional guest owner and group in owner:group form."`
	// Pointer preserves omitted vs explicitly empty input until resolution.
	Text *string `json:"text,omitempty" toml:"text" jsonschema:"Inline text content to write to the guest path."`
	// Pointer preserves omitted vs explicitly empty input until resolution.
	Mode *string `json:"mode,omitempty" toml:"mode" jsonschema:"Optional file mode matching ^0?[0-7]{3}$."`
	// Pointer preserves omitted vs explicitly false input until resolution.
	Overwrite *bool `json:"overwrite,omitempty" toml:"overwrite" jsonschema:"Overwrite an existing guest file."`
	// Pointer preserves omitted vs explicitly false input until resolution.
	FollowLinks *bool `json:"follow_links,omitempty" toml:"follow_links" jsonschema:"Follow symlinks when reading source content from the host."`
	// Pointer preserves omitted vs explicitly false input until resolution.
	WriteBack *bool `json:"write_back,omitempty" toml:"write_back" jsonschema:"Copy guest changes back to the host source path during teardown."`
	// Pointer preserves omitted vs explicitly empty input until resolution.
	Path *string `json:"source,omitempty" toml:"source" jsonschema:"Host source path for file content."`
}

type NotificationsInput struct {
	Exec   []string `json:"exec,omitempty" toml:"exec" jsonschema:"Host command template invoked for enabled notification states."`
	States []string `json:"states,omitempty" toml:"states" jsonschema:"Notification state names that should trigger the command."`
}

type RunInput struct {
	Exec []string       `json:"exec" toml:"exec" jsonschema:"Host command template to start before QEMU."`
	Vars map[string]any `json:"vars,omitempty" toml:"vars" jsonschema:"Template variables made available to this run command."`
}

type HotplugInput struct {
	Mounts   MountsInput    `json:"mounts,omitempty" toml:"mounts" jsonschema:"Mount devices available for later hotplug attach or detach."`
	Networks []NetworkInput `json:"networks,omitempty" toml:"networks" jsonschema:"Network devices available for later hotplug attach or detach."`
}

func (h HotplugInput) Len() int {
	return len(h.Mounts) + len(h.Networks)
}

func (h HotplugInput) VirtioFS() []VirtioFSMountInput {
	return h.Mounts.VirtioFS()
}
