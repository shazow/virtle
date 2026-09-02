package vmm

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"time"

	shellquote "github.com/kballard/go-shellquote"
	"github.com/shazow/virtle/backend/qemu/internal/launch"
	"github.com/shazow/virtle/backend/qemu/internal/qga"
	"github.com/shazow/virtle/backend/qemu/limits"
	controlpkg "github.com/shazow/virtle/internal/control"
	"github.com/shazow/virtle/internal/executor"
	"github.com/shazow/virtle/internal/manifest"
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
		return controlpkg.GuestPSResponse{}, guestFeatureError(err)
	}
	return controlpkg.GuestPSResponse{ProcessList: info.ProcessList}, nil
}

func (f managerGuestFeature) GuestExec(ctx context.Context, req controlpkg.GuestExecRequest) (controlpkg.GuestExecResponse, error) {
	if req.Path == "" {
		return controlpkg.GuestExecResponse{}, &controlpkg.RPCError{Code: controlpkg.ErrInvalidParams, Message: "guest exec path is required"}
	}
	if req.Timeout < 0 {
		return controlpkg.GuestExecResponse{}, &controlpkg.RPCError{Code: controlpkg.ErrInvalidParams, Message: "guest exec timeout must not be negative"}
	}
	client, err := f.guestClient(ctx)
	if err != nil {
		return controlpkg.GuestExecResponse{}, guestFeatureError(err)
	}
	defer client.Disconnect()
	path, args := req.Path, req.Args
	if req.Dir != "" || len(req.Env) > 0 {
		script := ""
		if req.Dir != "" {
			script += "cd " + shellquote.Join(req.Dir) + " && "
		}
		script += "exec " + shellquote.Join(append([]string{req.Path}, req.Args...)...)
		if len(req.Env) > 0 {
			script = "export " + shellquote.Join(req.Env...) + " && " + script
		}
		path, args = "/bin/sh", []string{"-c", script}
	}

	ctx, cancel := manifest.GuestCommandContext(ctx, time.Duration(req.Timeout))
	defer cancel()
	status, err := qga.RunCommandStatus(ctx, client, qga.ExecWait{
		Name:          "guest-exec",
		Path:          path,
		Args:          args,
		Subject:       req.Path,
		CaptureOutput: req.CaptureOutput,
	})
	if err != nil {
		return controlpkg.GuestExecResponse{}, guestFeatureError(err)
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
		return controlpkg.GuestReadResponse{}, guestFeatureError(err)
	}
	defer client.Disconnect()

	data, err := f.manager.readGuestFile(ctx, client, req.Path)
	if err != nil {
		return controlpkg.GuestReadResponse{}, guestFeatureError(err)
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
		return controlpkg.GuestWriteResponse{}, guestFeatureError(err)
	}
	defer client.Disconnect()

	if err := f.manager.writeGuestFile(ctx, client, req.Path, req.DataBase64); err != nil {
		return controlpkg.GuestWriteResponse{}, guestFeatureError(err)
	}
	if req.Mode != 0 {
		if err := f.manager.chmodGuestFile(ctx, client, req.Path, fmt.Sprintf("%03o", req.Mode&0o777)); err != nil {
			return controlpkg.GuestWriteResponse{}, guestFeatureError(err)
		}
	}
	return controlpkg.GuestWriteResponse{Path: req.Path}, nil
}

func (f managerGuestFeature) GuestShutdown(ctx context.Context, req controlpkg.GuestShutdownRequest) (controlpkg.GuestShutdownResponse, error) {
	_ = req
	shutdown := f.manager.launchManifest.QEMU.GuestAgent.ShutdownExec
	if err := f.manager.requestGuestShutdown(ctx, f.socketPath, shutdown); err != nil {
		return controlpkg.GuestShutdownResponse{}, guestFeatureError(err)
	}
	return controlpkg.GuestShutdownResponse{}, nil
}

func guestFeatureError(err error) error {
	if errors.Is(err, limits.ErrExceeded) {
		return controlpkg.ResourceLimit(err)
	}
	return controlpkg.FailedPrecondition(err)
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
