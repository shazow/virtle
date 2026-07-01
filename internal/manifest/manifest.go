// Package manifest defines the internal virtle launch contract.
//
// It owns the JSON schema that Nix emits for virtle, along with the defaulting
// and validation rules that keep the runtime assumptions consistent. The
// package also resolves working-directory and runtime-directory paths into the
// concrete host-side paths that the manager uses for sockets, lock files,
// volumes, QEMU binaries, and run processes.
package manifest

import (
	"log/slog"
	"time"

	"github.com/shazow/virtle/internal/hotplug"
	"github.com/shazow/virtle/internal/units"
)

type ResolveOptions struct {
	Logger *slog.Logger
}

type Manifest struct {
	Identity      Identity         `json:"identity"`
	Paths         Paths            `json:"paths"`
	Persistence   Persistence      `json:"persistence"`
	SSH           SSH              `json:"ssh"`
	QEMU          QEMU             `json:"qemu"`
	Volumes       []Volume         `json:"volumes,omitempty"`
	VSock         VSock            `json:"vsock"`
	Workspace     Workspace        `json:"workspace,omitempty"`
	WriteFiles    WriteFiles       `json:"writeFiles,omitempty"`
	Notifications Notifications    `json:"notifications,omitempty"`
	Run           []Run            `json:"run,omitempty"`
	Hotplug       []hotplug.Device `json:"hotplug,omitempty"`
	CleanupFiles  []string         `json:"cleanupFiles,omitempty"`
}

type Identity struct {
	HostName string `json:"hostName"`
}

type Paths struct {
	WorkingDir string     `json:"workingDir"`
	LockPath   string     `json:"lockPath"`
	RuntimeDir RuntimeDir `json:"runtimeDir,omitempty"`
}

type RuntimeDirMode int

const (
	RuntimeDirWorking RuntimeDirMode = iota
	RuntimeDirXDG
	RuntimeDirPath
)

type RuntimeDir struct {
	Mode RuntimeDirMode `json:"mode,omitempty"`
	Path string         `json:"path,omitempty"`
}

type Persistence struct {
	Directories []string `json:"directories"`
	BaseDir     string   `json:"baseDir,omitempty"`
	StateDir    string   `json:"stateDir,omitempty"`
}

type SSH struct {
	Argv          []string      `json:"argv"`
	User          string        `json:"user"`
	RetryDelay    time.Duration `json:"retryDelay,omitempty"`
	Autoprovision bool          `json:"autoprovision,omitempty"`
}

type VSockCIDRange struct {
	Start int `json:"start"`
	End   int `json:"end"`
}

type VSock struct {
	CIDRange VSockCIDRange `json:"cidRange"`
}

type Workspace struct {
	GuestDir string `json:"guestDir,omitempty"`
	HostDir  string `json:"hostDir,omitempty"`
	MountCWD bool   `json:"mountCWD,omitempty"`
}

type templateWorkspace struct {
	GuestPath string
	HostPath  string
}

type Volume struct {
	ImagePath     string    `json:"imagePath"`
	Size          units.MiB `json:"sizeMiB,omitempty"`
	FSType        string    `json:"fsType,omitempty"`
	AutoCreate    bool      `json:"autoCreate,omitempty"`
	Label         string    `json:"label,omitempty"`
	MkfsExtraArgs []string  `json:"mkfsExtraArgs,omitempty"`
}

type Command struct {
	Path string   `json:"path"`
	Args []string `json:"args,omitempty"`
	Env  []string `json:"env,omitempty"`
}

func (c Command) IsZero() bool {
	return c.Path == "" && len(c.Args) == 0 && len(c.Env) == 0
}

type Notifications struct {
	Command Command  `json:"command,omitempty"`
	States  []string `json:"states,omitempty"`
}

type Run struct {
	Exec []string       `json:"exec"`
	Env  []string       `json:"env,omitempty"`
	Vars map[string]any `json:"vars,omitempty"`
}

type WriteFile struct {
	Chown       string           `json:"chown,omitempty"`
	Mode        string           `json:"mode,omitempty"`
	Overwrite   bool             `json:"overwrite,omitempty"`
	FollowLinks bool             `json:"followLinks,omitempty"`
	WriteBack   bool             `json:"writeBack,omitempty"`
	Content     WriteFileContent `json:"content,omitempty"`
}

type WriteFileContentKind int

const (
	WriteFileContentNone WriteFileContentKind = iota
	WriteFileContentText
	WriteFileContentPath
)

type WriteFileContent struct {
	Kind WriteFileContentKind `json:"kind,omitempty"`
	Text string               `json:"text,omitempty"`
	Path string               `json:"path,omitempty"`
}

type WriteFiles map[string]WriteFile

type ResolvedWriteFile struct {
	GuestPath   string
	Chown       string
	Mode        string
	Overwrite   bool
	FollowLinks bool
	WriteBack   bool
	Content     WriteFileContent
}

type ResolvedRun struct {
	Exec []string
	Env  []string
	Dir  string
}
