package launch

import (
	"context"
	"os/exec"
	"time"

	"github.com/shazow/virtle/internal/manifest"
)

type ResumeMode string

const (
	ResumeModeNo    ResumeMode = "no"
	ResumeModeAuto  ResumeMode = "auto"
	ResumeModeForce ResumeMode = "force"
)

type Options struct {
	Resume    ResumeMode
	SSH       bool
	Verbosity int

	// HasRemoteControl declares whether the VM image runs a guest control
	// agent (QGA today, the virtle guest daemon later). When false, launch
	// skips guest-dependent steps (guest file writes, workspace mounts)
	// and guest control is reported unsupported. Callers set it
	// explicitly: the CLI always expects an agent, backend constructors
	// declare it per guest-control implementation.
	HasRemoteControl bool
}

func (o Options) WaitMode() WaitMode {
	if o.SSH {
		return WaitSSH
	}
	return WaitVM
}

func PlanForWaitMode(plan *Plan, mode WaitMode) *Plan {
	if plan == nil || (mode != WaitSSH && mode != WaitVM) {
		return plan
	}
	copyPlan := *plan
	copyOptions := copyPlan.Options
	copyOptions.SSH = mode == WaitSSH
	copyPlan.Options = copyOptions
	return &copyPlan
}

type WaitMode string

const (
	WaitAuto WaitMode = "auto"
	WaitSSH  WaitMode = "ssh"
	WaitVM   WaitMode = "vm"
)

type Spec struct {
	Manifest      *manifest.Manifest
	RemoteCommand []string
	Options       Options
}

type SuspendState struct {
	// Version marks the suspend-state format with the writing backend's
	// state version (e.g. "qemu-v1"). The format is backend-owned — the
	// saved VM state is a QEMU migration stream today — so only equality
	// matters: a state written by any other backend or version is not
	// resumable, and the marker turns that into a clear error instead of
	// a corrupt-state crash. Pre-marker states decode as "".
	Version       string    `json:"version"`
	HostName      string    `json:"hostName"`
	QMPSocketPath string    `json:"qmpSocketPath"`
	VMStatePath   string    `json:"vmStatePath,omitempty"`
	CID           int       `json:"cid,omitempty"`
	Timestamp     time.Time `json:"timestamp"`
	Status        string    `json:"status"`
}

type NotificationSink interface {
	Notify(ctx context.Context, state string, message string, values map[string]string)
}

type RuntimePaths struct {
	StateDir         string
	RuntimeDir       string
	ControlSocket    string
	QMPSocket        string
	GuestAgentSocket string
	SSHReadySocket   string
}

type Plan struct {
	Manifest                    *manifest.Manifest
	RemoteCommand               []string
	Options                     Options
	ResumeState                 *SuspendState
	Notifier                    NotificationSink
	Paths                       RuntimePaths
	VirtioFSSocketPaths         []string
	ExternalVirtioFSSocketPaths []string
	CleanupFiles                []string
	Volumes                     []manifest.Volume
	VolumeImagePaths            []string
	CID                         int
	QEMUCommand                 *exec.Cmd
}

func (p *Plan) RuntimeSocketCleanupFiles() []string {
	paths := make([]string, 0, 4+len(p.CleanupFiles))
	if p.Paths.QMPSocket != "" {
		paths = append(paths, p.Paths.QMPSocket)
	}
	if p.Paths.GuestAgentSocket != "" {
		paths = append(paths, p.Paths.GuestAgentSocket)
	}
	if p.Paths.SSHReadySocket != "" {
		paths = append(paths, p.Paths.SSHReadySocket)
	}
	if p.Paths.ControlSocket != "" {
		paths = append(paths, p.Paths.ControlSocket)
	}
	return append(paths, p.CleanupFiles...)
}
