package manifest

import (
	"time"

	"github.com/shazow/virtle/units"
)

type QEMU struct {
	BinaryPath      string         `json:"binaryPath"`
	RunAsUser       string         `json:"runAsUser,omitempty"`
	Name            string         `json:"name"`
	Machine         QEMUMachine    `json:"machine"`
	CPU             QEMUCPU        `json:"cpu"`
	Memory          QEMUMemory     `json:"memory"`
	Kernel          QEMUKernel     `json:"kernel"`
	SMP             QEMUSMP        `json:"smp"`
	Console         QEMUConsole    `json:"console"`
	Knobs           QEMUKnobs      `json:"knobs"`
	Graphics        QEMUGraphics   `json:"graphics,omitempty"`
	QMP             QEMUQMP        `json:"qmp"`
	GuestAgent      QEMUGuestAgent `json:"guestAgent,omitempty"`
	SSHReady        QEMUSSHReady   `json:"sshReady,omitempty"`
	Hotplug         QEMUHotplug    `json:"hotplug,omitempty"`
	Devices         QEMUDevices    `json:"devices"`
	MachineID       string         `json:"machineId,omitempty"`
	PassthroughArgs []string       `json:"passthroughArgs,omitempty"`
}

type QEMUMachine struct {
	Type    string   `json:"type"`
	Options []string `json:"options,omitempty"`
}

type QEMUCPU struct {
	Model     string `json:"model"`
	EnableKVM bool   `json:"enableKvm,omitempty"`
}

type QEMUMemory struct {
	Size    units.MiB `json:"sizeMiB"`
	Backend string    `json:"backend,omitempty"`
	Shared  bool      `json:"shared,omitempty"`
}

type QEMUKernel struct {
	Path       string `json:"path"`
	InitrdPath string `json:"initrdPath"`
	Params     string `json:"params,omitempty"`
}

type QEMUSMP struct {
	CPUs CPUCount `json:"cpus,omitempty"`
}

type CPUCount struct {
	Value int  `json:"value,omitempty"`
	Set   bool `json:"set,omitempty"`
}

func ExplicitCPUs(value int) CPUCount {
	return CPUCount{Value: value, Set: true}
}

func (c CPUCount) QEMUValue() int {
	if c.Set {
		return c.Value
	}
	return 0
}

type QEMUConsole string

const (
	QEMUConsoleOff         QEMUConsole = KernelSerialOff
	QEMUConsolePrint       QEMUConsole = KernelSerialPrint
	QEMUConsoleInteractive QEMUConsole = KernelSerialConsole
)

func (c QEMUConsole) Enabled() bool {
	return c == QEMUConsolePrint || c == QEMUConsoleInteractive
}

func (c QEMUConsole) Interactive() bool {
	return c == QEMUConsoleInteractive
}

type QEMUKnobs struct {
	NoDefaults     bool `json:"noDefaults,omitempty"`
	NoUserConfig   bool `json:"noUserConfig,omitempty"`
	NoReboot       bool `json:"noReboot,omitempty"`
	NoGraphic      bool `json:"noGraphic,omitempty"`
	SeccompSandbox bool `json:"seccompSandbox,omitempty"`
}

type QEMUGraphics struct {
	Backend string `json:"backend"`
}

func (g QEMUGraphics) IsZero() bool {
	return g.Backend == ""
}

type QEMUQMP struct {
	SocketPath string `json:"socketPath"`
}

type QEMUGuestAgent struct {
	SocketPath      string        `json:"socketPath,omitempty"`
	CommandTimeout  time.Duration `json:"commandTimeout,omitempty"`
	ShutdownExec    []string      `json:"shutdownExec,omitempty"`
	ShutdownTimeout time.Duration `json:"shutdownTimeout,omitempty"`
}

type QEMUSSHReady struct {
	SocketPath string `json:"socketPath,omitempty"`
}

type QEMUHotplug struct {
	PCIEPorts int `json:"pciePorts,omitempty"`
}

type QEMUDevices struct {
	RNG      QEMURNGDevice       `json:"rng"`
	I8042    bool                `json:"i8042,omitempty"`
	Balloon  *BalloonDevice      `json:"balloon,omitempty"`
	VirtioFS []QEMUVirtioFSShare `json:"virtiofs,omitempty"`
	NineP    []QEMUNinePShare    `json:"9p,omitempty"`
	Block    []QEMUBlockDevice   `json:"block,omitempty"`
	Mounts   []QEMUMountDevice   `json:"mounts,omitempty"`
	Network  []QEMUNetDevice     `json:"network,omitempty"`
	VSOCK    QEMUVSOCKDevice     `json:"vsock"`
}

type QEMURNGDevice struct {
	ID        string `json:"id"`
	Transport string `json:"transport"`
}

type QEMUVirtioFSShare struct {
	ID         string `json:"id"`
	SocketPath string `json:"socketPath"`
	Tag        string `json:"tag"`
	Transport  string `json:"transport"`
}

type QEMUNinePShare struct {
	ID            string `json:"id"`
	SourcePath    string `json:"sourcePath"`
	Tag           string `json:"tag"`
	SecurityModel string `json:"securityModel"`
	ReadOnly      bool   `json:"readOnly"`
	Transport     string `json:"transport"`
}

type QEMUBlockDevice struct {
	ID        string `json:"id"`
	ImagePath string `json:"imagePath"`
	Format    string `json:"format,omitempty"`
	AIO       string `json:"aio,omitempty"`
	Cache     string `json:"cache,omitempty"`
	ReadOnly  bool   `json:"readOnly,omitempty"`
	Serial    string `json:"serial,omitempty"`
	Transport string `json:"transport"`
}

type QEMUMountDevice struct {
	Type     string             `json:"type"`
	VirtioFS *QEMUVirtioFSShare `json:"virtiofs,omitempty"`
	NineP    *QEMUNinePShare    `json:"9p,omitempty"`
	Block    *QEMUBlockDevice   `json:"block,omitempty"`
}

type QEMUNetDevice struct {
	ID            string   `json:"id"`
	Backend       string   `json:"backend"`
	MacAddress    string   `json:"macAddress"`
	Transport     string   `json:"transport"`
	RomFile       string   `json:"romFile,omitempty"`
	DisableROM    bool     `json:"disableROM,omitempty"`
	NetdevOptions []string `json:"netdevOptions,omitempty"`
	MQVectors     int      `json:"mqVectors,omitempty"`
}

type QEMUVSOCKDevice struct {
	ID        string `json:"id"`
	Transport string `json:"transport"`
}

func (q QEMU) NoGraphicEnabled() bool {
	return q.Knobs.NoGraphic
}
