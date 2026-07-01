package manager

import (
	"context"
	"encoding/base64"
	"fmt"

	"github.com/shazow/virtle/internal/executor"
	controlpkg "github.com/shazow/virtle/internal/manager/control"
	"github.com/shazow/virtle/internal/manager/launch"
	"github.com/shazow/virtle/internal/qga"
)

type managerGuestFeature struct {
	manager    *manager
	socketPath string
	processes  *launch.ProcessSet
}

func (m *manager) guestFeature(socketPath string, processes *launch.ProcessSet) managerGuestFeature {
	return managerGuestFeature{manager: m, socketPath: socketPath, processes: processes}
}

func (f managerGuestFeature) GuestPS(ctx context.Context, req controlpkg.GuestPSRequest) (controlpkg.GuestPSResponse, error) {
	_ = req
	watchers := executor.Group{}
	if f.processes != nil {
		watchers = f.processes.Watchers()
	}
	info, err := f.manager.collectGuestInfo(ctx, f.socketPath, watchers)
	if err != nil {
		return controlpkg.GuestPSResponse{}, controlpkg.FailedPrecondition(err)
	}
	return controlpkg.GuestPSResponse{ProcessList: info.ProcessList}, nil
}

func (f managerGuestFeature) GuestExec(ctx context.Context, req controlpkg.GuestExecRequest) (controlpkg.GuestExecResponse, error) {
	if req.Path == "" {
		return controlpkg.GuestExecResponse{}, &controlpkg.RPCError{Code: controlpkg.ErrInvalidParams, Message: "guest exec path is required"}
	}
	client, err := f.guestClient(ctx)
	if err != nil {
		return controlpkg.GuestExecResponse{}, controlpkg.FailedPrecondition(err)
	}
	defer client.Disconnect()

	status, err := qga.RunCommandStatus(ctx, client, qga.ExecWait{
		Timeout:       f.manager.effectiveQMPCommandTimeout(),
		PollDelay:     defaultMigrationPollDelay,
		Name:          "guest-exec",
		Path:          req.Path,
		Args:          req.Args,
		Subject:       req.Path,
		CaptureOutput: req.CaptureOutput,
	})
	if err != nil {
		return controlpkg.GuestExecResponse{}, controlpkg.FailedPrecondition(err)
	}
	return controlpkg.GuestExecResponse{
		Exited:   status.Exited,
		ExitCode: status.ExitCode,
		OutData:  status.OutData,
		ErrData:  status.ErrData,
	}, nil
}

func (f managerGuestFeature) GuestRead(ctx context.Context, req controlpkg.GuestReadRequest) (controlpkg.GuestReadResponse, error) {
	if req.Path == "" {
		return controlpkg.GuestReadResponse{}, &controlpkg.RPCError{Code: controlpkg.ErrInvalidParams, Message: "guest read path is required"}
	}
	client, err := f.guestClient(ctx)
	if err != nil {
		return controlpkg.GuestReadResponse{}, controlpkg.FailedPrecondition(err)
	}
	defer client.Disconnect()

	data, err := f.manager.readGuestFile(client, req.Path)
	if err != nil {
		return controlpkg.GuestReadResponse{}, controlpkg.FailedPrecondition(err)
	}
	return controlpkg.GuestReadResponse{Path: req.Path, DataBase64: base64.StdEncoding.EncodeToString(data)}, nil
}

func (f managerGuestFeature) GuestWrite(ctx context.Context, req controlpkg.GuestWriteRequest) (controlpkg.GuestWriteResponse, error) {
	if req.Path == "" {
		return controlpkg.GuestWriteResponse{}, &controlpkg.RPCError{Code: controlpkg.ErrInvalidParams, Message: "guest write path is required"}
	}
	if _, err := base64.StdEncoding.DecodeString(req.DataBase64); err != nil {
		return controlpkg.GuestWriteResponse{}, &controlpkg.RPCError{Code: controlpkg.ErrInvalidParams, Message: fmt.Sprintf("guest write data must be base64: %v", err)}
	}
	client, err := f.guestClient(ctx)
	if err != nil {
		return controlpkg.GuestWriteResponse{}, controlpkg.FailedPrecondition(err)
	}
	defer client.Disconnect()

	if err := f.manager.writeGuestFile(client, req.Path, req.DataBase64); err != nil {
		return controlpkg.GuestWriteResponse{}, controlpkg.FailedPrecondition(err)
	}
	return controlpkg.GuestWriteResponse{Path: req.Path}, nil
}

func (f managerGuestFeature) guestClient(ctx context.Context) (qga.Client, error) {
	if f.socketPath == "" {
		return nil, fmt.Errorf("guest agent socket is not configured")
	}
	watchers := executor.Group{}
	if f.processes != nil {
		watchers = f.processes.Watchers()
	}
	return f.manager.waitForGuestAgent(ctx, f.socketPath, watchers)
}
