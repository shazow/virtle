package control

import (
	"context"
	"encoding/json"
	"fmt"
	"time"
)

// RuntimeState is the lifecycle state reported by the control socket.
type RuntimeState string

const (
	// RuntimeStarting means the manager is still preparing the VM.
	RuntimeStarting RuntimeState = "starting"
	// RuntimeReady means the runtime is available for control requests.
	RuntimeReady RuntimeState = "ready"
	// RuntimeSuspending means a suspend request is in progress.
	RuntimeSuspending RuntimeState = "suspending"
	// RuntimeSuspended means VM state has been saved.
	RuntimeSuspended RuntimeState = "suspended"
	// RuntimeStopping means teardown has started.
	RuntimeStopping RuntimeState = "stopping"
	// RuntimeStopped means teardown has completed.
	RuntimeStopped RuntimeState = "stopped"
)

// StatusRequest asks for the current runtime status.
type StatusRequest struct{}

// StatusResponse reports runtime status and connection paths.
type StatusResponse struct {
	State RuntimeState `json:"state"`
	CID   int          `json:"cid"`
	Paths StatusPaths  `json:"paths"`
	Stats RuntimeStats `json:"stats"`
}

// StatusPaths are host-side sockets associated with the runtime.
type StatusPaths struct {
	ControlSocket    string `json:"controlSocket"`
	QMPSocket        string `json:"qmpSocket"`
	GuestAgentSocket string `json:"guestAgentSocket,omitempty"`
	SSHReadySocket   string `json:"sshReadySocket,omitempty"`
}

// RuntimeStats reports lifecycle timing captured during launch and teardown.
type RuntimeStats struct {
	StartedAt       time.Time `json:"startedAt,omitempty"`
	BootStartedAt   time.Time `json:"bootStartedAt,omitempty"`
	QMPReadyAt      time.Time `json:"qmpReadyAt,omitempty"`
	FilesReadyAt    time.Time `json:"filesReadyAt,omitempty"`
	SSHReadyAt      time.Time `json:"sshReadyAt,omitempty"`
	SSHStartedAt    time.Time `json:"sshStartedAt,omitempty"`
	CompletedAt     time.Time `json:"completedAt,omitempty"`
	SSHAttempts     int       `json:"sshAttempts,omitempty"`
	StartedToBoot   string    `json:"startedToBoot,omitempty"`
	BootToQMP       string    `json:"bootToQMP,omitempty"`
	FilesToSSH      string    `json:"filesToSSH,omitempty"`
	BootToCompleted string    `json:"bootToCompleted,omitempty"`
	Total           string    `json:"total,omitempty"`
}

// SuspendRequest asks the runtime to save VM state and exit.
type SuspendRequest struct{}

// SuspendResponse reports whether suspend state was saved.
type SuspendResponse struct {
	Saved       bool   `json:"saved"`
	VMStatePath string `json:"vmStatePath,omitempty"`
}

// HotplugRequest asks the runtime to attach or detach a configured device.
type HotplugRequest struct {
	ID     string `json:"id"`
	Detach bool   `json:"detach"`
}

// HotplugResponse identifies the hotplug operation that completed.
type HotplugResponse struct {
	ID     string `json:"id"`
	Detach bool   `json:"detach"`
}

// BalloonRequest asks the runtime to resize or query the memory balloon.
type BalloonRequest struct {
	TargetBytes int64 `json:"targetBytes,omitempty"`
}

// BalloonResponse reports the current and requested balloon sizes.
type BalloonResponse struct {
	ActualBytes int64 `json:"actualBytes"`
	TargetBytes int64 `json:"targetBytes,omitempty"`
}

// GuestPSRequest asks for the guest process list.
type GuestPSRequest struct{}

// GuestPSResponse reports the guest process list.
type GuestPSResponse struct {
	ProcessList string `json:"processList,omitempty"`
}

// GuestExecRequest asks the guest agent to execute a process.
type GuestExecRequest struct {
	Path          string   `json:"path"`
	Args          []string `json:"args,omitempty"`
	CaptureOutput bool     `json:"captureOutput,omitempty"`
}

// GuestExecResponse reports the completed guest process status.
type GuestExecResponse struct {
	Exited   bool   `json:"exited"`
	ExitCode int    `json:"exitCode"`
	OutData  string `json:"outData,omitempty"`
	ErrData  string `json:"errData,omitempty"`
}

// GuestReadRequest asks the guest agent to read a file.
type GuestReadRequest struct {
	Path string `json:"path"`
}

// GuestReadResponse reports base64-encoded file data read from the guest.
type GuestReadResponse struct {
	Path       string `json:"path"`
	DataBase64 string `json:"data-base64"`
}

// GuestWriteRequest asks the guest agent to write base64-encoded data to a file.
type GuestWriteRequest struct {
	Path       string `json:"path"`
	DataBase64 string `json:"data-base64"`
}

// GuestWriteResponse reports the guest file path that was written.
type GuestWriteResponse struct {
	Path string `json:"path"`
}

// MethodsRequest asks which RPC methods are available on this control socket.
type MethodsRequest struct{}

// MethodsResponse reports RPC methods available on this control socket.
type MethodsResponse struct {
	Methods []string `json:"methods"`
}

// ErrorCode classifies a control socket RPC failure.
type ErrorCode string

const (
	// ErrInvalidRequest means the request envelope could not be decoded.
	ErrInvalidRequest ErrorCode = "invalid_request"
	// ErrUnknownMethod means the requested RPC method is not implemented.
	ErrUnknownMethod ErrorCode = "unknown_method"
	// ErrInvalidParams means the request params did not match the method.
	ErrInvalidParams ErrorCode = "invalid_params"
	// ErrUnsupported means the runtime was built or configured without a capability.
	ErrUnsupported ErrorCode = "unsupported"
	// ErrFailedPrecondition means the runtime is not ready for the requested operation.
	ErrFailedPrecondition ErrorCode = "failed_precondition"
	// ErrInternal means the request failed with an unexpected internal error.
	ErrInternal ErrorCode = "internal"
)

// RPCError is the structured error returned over the control socket.
type RPCError struct {
	Code    ErrorCode `json:"code"`
	Message string    `json:"message"`
}

func (e *RPCError) Error() string {
	if e == nil {
		return ""
	}
	return fmt.Sprintf("%s: %s", e.Code, e.Message)
}

type rpcMethod string

const (
	rpcStatus     rpcMethod = "status"
	rpcMethods    rpcMethod = "methods"
	rpcSuspend    rpcMethod = "suspend"
	rpcHotplug    rpcMethod = "hotplug"
	rpcBalloon    rpcMethod = "balloon"
	rpcGuestPS    rpcMethod = "guest-ps"
	rpcGuestExec  rpcMethod = "guest-exec"
	rpcGuestRead  rpcMethod = "guest-read"
	rpcGuestWrite rpcMethod = "guest-write"
)

type requestEnvelope struct {
	ID     int             `json:"id"`
	Method rpcMethod       `json:"method"`
	Params json.RawMessage `json:"params"`
}

type responseEnvelope struct {
	ID     int             `json:"id"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  *RPCError       `json:"error,omitempty"`
}

// RuntimeCore is the minimum runtime surface required by a control router.
type RuntimeCore interface {
	Status(context.Context, StatusRequest) (StatusResponse, error)
}

// RuntimeGuest is implemented by handlers that can interact with the guest agent.
type RuntimeGuest interface {
	GuestPS(context.Context, GuestPSRequest) (GuestPSResponse, error)
	GuestExec(context.Context, GuestExecRequest) (GuestExecResponse, error)
	GuestRead(context.Context, GuestReadRequest) (GuestReadResponse, error)
	GuestWrite(context.Context, GuestWriteRequest) (GuestWriteResponse, error)
}

// RuntimeSuspend is implemented by runtimes that can save VM state.
type RuntimeSuspend interface {
	Suspend(context.Context, SuspendRequest) (SuspendResponse, error)
}

// RuntimeHotplug is implemented by runtimes that can attach and detach devices.
type RuntimeHotplug interface {
	Hotplug(context.Context, HotplugRequest) (HotplugResponse, error)
}

// RuntimeBalloon is implemented by runtimes that can control a memory balloon.
type RuntimeBalloon interface {
	Balloon(context.Context, BalloonRequest) (BalloonResponse, error)
}

// Handlers groups the runtime capabilities used by a control router.
type Handlers struct {
	Core    RuntimeCore
	Guest   RuntimeGuest
	Suspend RuntimeSuspend
	Hotplug RuntimeHotplug
	Balloon RuntimeBalloon
}
