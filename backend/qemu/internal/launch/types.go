package launch

import (
	"context"
	"os/exec"
	"time"

	"github.com/shazow/virtle/internal/manifest"
	"github.com/shazow/virtle/vm"
	"github.com/shazow/virtle/vmnet"
)

type ResumeMode string

const (
	ResumeModeNo    ResumeMode = "no"
	ResumeModeAuto  ResumeMode = "auto"
	ResumeModeForce ResumeMode = "force"
)

type Options struct {
	Resume ResumeMode

	// HasRemoteControl declares whether the VM image runs a guest control
	// agent (QGA today, the virtle guest daemon later). When false, launch
	// skips guest-dependent steps (guest file writes, workspace mounts)
	// and guest control is reported unsupported. Callers set it
	// explicitly: the CLI always expects an agent, backend constructors
	// declare it per guest-control implementation.
	HasRemoteControl bool

	// RemoveStateDir removes the manifest's state directory once runtime
	// state is released. Callers set it for a state directory they created
	// for this launch alone (a Spec without Dir).
	RemoveStateDir bool

	// Egress is the guest's egress policy, handed to the network a virtle
	// NIC attaches to; nil means the network's default.
	Egress *vm.Egress
}

type Spec struct {
	Manifest *manifest.Manifest
	Options  Options
}

// SuspendStatusSaved is the SuspendState.Status of a save whose VM state
// stream completed; only saved states are resumable.
const SuspendStatusSaved = "saved"

type SuspendState struct {
	// Version is the state version token stamped at suspend and compared
	// on resume; only an exact match is resumable. Pre-marker states
	// decode as "".
	Version       string    `json:"version"`
	HostName      string    `json:"hostName"`
	QMPSocketPath string    `json:"qmpSocketPath"`
	VMStatePath   string    `json:"vmStatePath,omitempty"`
	CID           int       `json:"cid,omitempty"`
	Timestamp     time.Time `json:"timestamp"`
	Status        string    `json:"status"`
	// NetworkMAC and NetworkAddr identify the guest NIC on its virtle
	// network, so a resume re-attaches with the lease the guest still holds.
	NetworkMAC  string `json:"networkMac,omitempty"`
	NetworkAddr string `json:"networkAddr,omitempty"`
	// NetworkState preserves synthetic DNS addresses and guest secret tokens
	// that remain cached in the saved VM's memory.
	NetworkState *vmnet.NetworkState `json:"networkState,omitempty"`
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
