package control

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/shazow/virtle/backend"
	"github.com/shazow/virtle/units"
	"github.com/shazow/virtle/vm"
)

// RuntimeState is the lifecycle state reported by the control socket.
type RuntimeState = backend.State

const (
	// RuntimeStarting means the manager is still preparing the VM.
	RuntimeStarting RuntimeState = backend.StateStarting
	// RuntimeReady means the runtime is available for control requests.
	RuntimeReady RuntimeState = backend.StateReady
	// RuntimeSuspending means a suspend request is in progress.
	RuntimeSuspending RuntimeState = backend.StateSuspending
	// RuntimeSuspended means VM state has been saved.
	RuntimeSuspended RuntimeState = backend.StateSuspended
	// RuntimeStopping means teardown has started.
	RuntimeStopping RuntimeState = backend.StateStopping
	// RuntimeStopped means teardown has completed.
	RuntimeStopped RuntimeState = backend.StateStopped
)

// StatusRequest asks for the current runtime status.
type StatusRequest struct{}

// StatusResponse reports runtime status and connection paths.
type StatusResponse = backend.Status

// StatusPaths are host-side sockets associated with the runtime.
type StatusPaths = backend.StatusPaths

// RuntimeStats reports lifecycle timing captured during launch and teardown.
type RuntimeStats = backend.RuntimeStats

// SuspendRequest asks the runtime to save VM state and exit.
type SuspendRequest struct{}

// SuspendResponse reports whether suspend state was saved.
type SuspendResponse struct {
	Saved       bool   `json:"saved"`
	VMStatePath string `json:"vmStatePath,omitempty"`
}

// HotplugRequest asks the runtime to attach or detach a configured device.
type HotplugRequest struct {
	ID     string         `json:"id"`
	Detach bool           `json:"detach"`
	Device *DeviceRequest `json:"device,omitempty"`
}

// DeviceRequest carries one neutral device over the control socket.
type DeviceRequest struct {
	Share   *vm.Share   `json:"share,omitempty"`
	Disk    *vm.Disk    `json:"disk,omitempty"`
	Forward *vm.Forward `json:"forward,omitempty"`
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
	Env           []string `json:"env,omitempty"`
	Dir           string   `json:"dir,omitempty"`
	CaptureOutput bool     `json:"captureOutput,omitempty"`
	// Timeout bounds the guest command; zero or omitted waits indefinitely.
	Timeout units.Duration `json:"timeout,omitempty"`
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
	Mode       uint32 `json:"mode,omitempty"`
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

// WaitRequest waits for the machine runtime to finish.
type WaitRequest struct{}

// WaitResponse confirms that the machine runtime finished successfully.
type WaitResponse struct{}

// KillRequest asks the runtime to hard-stop the VM and release its state.
type KillRequest struct{}

// KillResponse confirms the kill completed.
type KillResponse struct{}

// ShutdownRequest asks the runtime for an orderly teardown.
type ShutdownRequest struct{}

// ShutdownResponse confirms the teardown completed.
type ShutdownResponse struct{}

// GuestShutdownRequest asks the guest agent to power the guest off.
type GuestShutdownRequest struct{}

// GuestShutdownResponse confirms the shutdown request was issued.
type GuestShutdownResponse struct{}

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
	// ErrResourceLimit means the server rejected work that crossed a resource limit.
	ErrResourceLimit ErrorCode = "resource_limit"
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

func (e *RPCError) Is(target error) bool {
	return e != nil && e.Code == ErrUnsupported && target == errors.ErrUnsupported
}

type rpcMethod string

const (
	rpcStatus        rpcMethod = "status"
	rpcMethods       rpcMethod = "methods"
	rpcWait          rpcMethod = "wait"
	rpcKill          rpcMethod = "kill"
	rpcShutdown      rpcMethod = "shutdown"
	rpcSuspend       rpcMethod = "suspend"
	rpcHotplug       rpcMethod = "hotplug"
	rpcBalloon       rpcMethod = "balloon"
	rpcGuestPS       rpcMethod = "guest-ps"
	rpcGuestExec     rpcMethod = "guest-exec"
	rpcGuestRead     rpcMethod = "guest-read"
	rpcGuestWrite    rpcMethod = "guest-write"
	rpcGuestShutdown rpcMethod = "guest-shutdown"
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
	Wait(context.Context, WaitRequest) (WaitResponse, error)
}

// RuntimeGuest is implemented by handlers that can interact with the guest agent.
type RuntimeGuest interface {
	GuestPS(context.Context, GuestPSRequest) (GuestPSResponse, error)
	GuestExec(context.Context, GuestExecRequest) (GuestExecResponse, error)
	GuestRead(context.Context, GuestReadRequest) (GuestReadResponse, error)
	GuestWrite(context.Context, GuestWriteRequest) (GuestWriteResponse, error)
	GuestShutdown(context.Context, GuestShutdownRequest) (GuestShutdownResponse, error)
}

// RuntimeKill is implemented by runtimes that can hard-stop the VM.
type RuntimeKill interface {
	Kill(context.Context, KillRequest) (KillResponse, error)
}

// RuntimeShutdown is implemented by runtimes that can tear down gracefully.
// The method is named ShutdownRPC so implementers keep Shutdown(ctx) free
// for their own teardown entry point.
type RuntimeShutdown interface {
	ShutdownRPC(context.Context, ShutdownRequest) (ShutdownResponse, error)
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
	Core     RuntimeCore
	Guest    RuntimeGuest
	Suspend  RuntimeSuspend
	Hotplug  RuntimeHotplug
	Balloon  RuntimeBalloon
	Kill     RuntimeKill
	Shutdown RuntimeShutdown
}
