package manifest

const (
	defaultGraphicsBackend = "headless"
	defaultNetworkType     = "user"
)

// DefaultDocument returns the manifest input defaults that virtle assumes when
// optional fields are omitted. Required fields without defaults, such as
// kernel.path and kernel.initrd_path, are intentionally left unset.
func DefaultDocument() Document {
	doc := Document{
		Graphics: &GraphicsInput{
			Backend: defaultGraphicsBackend,
		},
		Networks: []NetworkInput{
			{
				ID:   defaultNetworkID,
				Type: defaultNetworkType,
				MAC:  defaultNetworkMAC,
			},
		},
		SSH: SSHInput{
			Exec: []string{defaultSSHCommand},
		},
		VSock: VSockInput{
			CIDRange: RangeInput{
				Min: defaultVSockCIDStart,
				Max: defaultVSockCIDEnd,
			},
		},
	}
	applyDefaultTags(&doc)
	return doc
}

// DocumentWithDefaults returns document overlaid on DefaultDocument. Pointer,
// slice, and map fields keep omission semantics: nil means omitted and receives
// the default; non-nil empty values are preserved. Scalar durations whose zero
// is meaningful (guest_default_timeout, shutdown_timeout) or invalid
// (ssh.retry_delay) are always taken from document; DecodeDocumentBytes seeds
// their defaults before decoding, so documents built in code must set them
// explicitly.
func DocumentWithDefaults(document Document) Document {
	defaults := DefaultDocument()

	if document.HostName != "" {
		defaults.HostName = document.HostName
	}
	if document.WorkingDir != "" {
		defaults.WorkingDir = document.WorkingDir
	}
	if document.StateDir != "" {
		defaults.StateDir = document.StateDir
	}
	defaults.Host = mergeHostInput(defaults.Host, document.Host)
	defaults.QEMU = mergeQEMUInput(defaults.QEMU, document.QEMU)
	defaults.Machine = mergeMachineInput(defaults.Machine, document.Machine)
	defaults.Kernel = mergeKernelInput(defaults.Kernel, document.Kernel)
	if document.Graphics != nil {
		defaults.Graphics = document.Graphics
	}
	if document.Mounts != nil {
		defaults.Mounts = document.Mounts
	}
	defaults.Workspace = mergeWorkspaceInput(defaults.Workspace, document.Workspace)
	if document.Networks != nil {
		defaults.Networks = document.Networks
	}
	if document.Balloon != nil {
		defaults.Balloon = document.Balloon
	}
	defaults.SSH = mergeSSHInput(defaults.SSH, document.SSH)
	defaults.VSock = mergeVSockInput(defaults.VSock, document.VSock)
	if document.WriteFiles != nil {
		defaults.WriteFiles = document.WriteFiles
	}
	defaults.Notifications = mergeNotificationsInput(defaults.Notifications, document.Notifications)
	if document.Run != nil {
		defaults.Run = document.Run
	}
	defaults.Hotplug = mergeHotplugInput(defaults.Hotplug, document.Hotplug)

	return defaults
}

func mergeHostInput(base HostInput, override HostInput) HostInput {
	if override.OS != "" {
		base.OS = override.OS
	}
	if override.Arch != "" {
		base.Arch = override.Arch
	}
	if override.System != "" {
		base.System = override.System
	}
	return base
}

func mergeQEMUInput(base QEMUInput, override QEMUInput) QEMUInput {
	if override.Exec != nil {
		base.Exec = override.Exec
	}
	if override.FwdTunnelExec != nil {
		base.FwdTunnelExec = override.FwdTunnelExec
	}
	if override.User != "" {
		base.User = override.User
	}
	if override.Seccomp {
		base.Seccomp = override.Seccomp
	}
	if override.MachineOptions != nil {
		base.MachineOptions = override.MachineOptions
	}
	if override.QMPSocket != "" {
		base.QMPSocket = override.QMPSocket
	}
	if override.GuestAgentSocket != "" {
		base.GuestAgentSocket = override.GuestAgentSocket
	}
	// Always taken from the override: zero means no timeout, and decode seeds
	// the default for omitted keys.
	base.GuestDefaultTimeout = override.GuestDefaultTimeout
	if override.ShutdownExec != nil {
		base.ShutdownExec = override.ShutdownExec
	}
	// Always taken from the override: zero disables the graceful-shutdown
	// wait, and decode seeds the default for omitted keys.
	base.ShutdownTimeout = override.ShutdownTimeout
	return base
}

func mergeMachineInput(base MachineInput, override MachineInput) MachineInput {
	if override.Type != "" {
		base.Type = override.Type
	}
	if override.VCPU != 0 {
		base.VCPU = override.VCPU
	}
	if override.ID != "" {
		base.ID = override.ID
	}
	if override.Memory != 0 {
		base.Memory = override.Memory
	}
	if override.CPU != "" {
		base.CPU = override.CPU
	}
	if override.KVM != nil {
		base.KVM = override.KVM
	}
	return base
}

func mergeKernelInput(base KernelInput, override KernelInput) KernelInput {
	if override.Path != "" {
		base.Path = override.Path
	}
	if override.InitrdPath != "" {
		base.InitrdPath = override.InitrdPath
	}
	if override.Params != nil {
		base.Params = override.Params
	}
	if override.Serial != "" {
		base.Serial = override.Serial
	}
	return base
}

func mergeWorkspaceInput(base WorkspaceInput, override WorkspaceInput) WorkspaceInput {
	if override.GuestDir != "" {
		base.GuestDir = override.GuestDir
	}
	if override.HostDir != "" {
		base.HostDir = override.HostDir
	}
	if override.MountCWD {
		base.MountCWD = override.MountCWD
	}
	return base
}

func mergeSSHInput(base SSHInput, override SSHInput) SSHInput {
	if override.Exec != nil {
		base.Exec = override.Exec
	}
	if override.User != "" {
		base.User = override.User
	}
	if override.ReadySocket != "" {
		base.ReadySocket = override.ReadySocket
	}
	// Always taken from the override: decode seeds the default for omitted
	// keys, and validation rejects a zero delay, so a zero here means the
	// document skipped decode-time seeding.
	base.RetryDelay = override.RetryDelay
	if override.Autoprovision {
		base.Autoprovision = override.Autoprovision
	}
	return base
}

func mergeVSockInput(base VSockInput, override VSockInput) VSockInput {
	if override.CIDRange.Min != 0 {
		base.CIDRange.Min = override.CIDRange.Min
	}
	if override.CIDRange.Max != 0 {
		base.CIDRange.Max = override.CIDRange.Max
	}
	return base
}

func mergeNotificationsInput(base NotificationsInput, override NotificationsInput) NotificationsInput {
	if override.Exec != nil {
		base.Exec = override.Exec
	}
	if override.States != nil {
		base.States = override.States
	}
	return base
}

func mergeHotplugInput(base HotplugInput, override HotplugInput) HotplugInput {
	if override.Mounts != nil {
		base.Mounts = override.Mounts
	}
	if override.Networks != nil {
		base.Networks = override.Networks
	}
	return base
}
