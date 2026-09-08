package control

import (
	"context"
	"errors"
	"fmt"

	"github.com/shazow/virtle/backend"
)

// NewMachineRouter exposes the common machine lifecycle over the control
// socket. Optional guest/device protocols are registered by their adapters.
func NewMachineRouter(m backend.Machine) (*Router, error) {
	adapter := machineHandler{m: m}
	return NewRouter(Handlers{Core: adapter, Kill: adapter, Shutdown: adapter})
}

type machineHandler struct{ m backend.Machine }

func (h machineHandler) Status(ctx context.Context, _ StatusRequest) (StatusResponse, error) {
	r, ok := h.m.(backend.StatusReporter)
	if !ok {
		return StatusResponse{}, fmt.Errorf("machine status: %w", errors.ErrUnsupported)
	}
	return r.Status(ctx)
}
func (h machineHandler) Wait(ctx context.Context, _ WaitRequest) (WaitResponse, error) {
	return WaitResponse{}, h.m.Wait(ctx)
}
func (h machineHandler) Kill(context.Context, KillRequest) (KillResponse, error) {
	return KillResponse{}, h.m.Kill()
}
func (h machineHandler) ShutdownRPC(ctx context.Context, _ ShutdownRequest) (ShutdownResponse, error) {
	return ShutdownResponse{}, h.m.Shutdown(ctx)
}
