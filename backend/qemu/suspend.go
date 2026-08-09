package qemu

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/shazow/virtle/backend"
	"github.com/shazow/virtle/internal/qmpclient"
	"github.com/shazow/virtle/vm"
)

const (
	// suspendStateVersion marks the layout of the files Suspend writes. It is
	// checked on resume so a machine suspended by a different virtle fails
	// with an explanation instead of a corrupt restore.
	suspendStateVersion = 1

	suspendStateFile   = "suspend.json"
	suspendVMStateFile = "vmstate"

	// migrationTimeout bounds saving or restoring the VM's memory image.
	migrationTimeout = 5 * time.Minute
)

var _ backend.Suspender = (*qemuBackend)(nil)

// suspendState is what Suspend leaves behind for Resume. The format belongs to
// this backend and is not a public contract: it may change whenever the
// version marker does.
type suspendState struct {
	Version     int       `json:"version"`
	HostName    string    `json:"hostName"`
	CID         int       `json:"cid"`
	VMStatePath string    `json:"vmStatePath"`
	SuspendedAt time.Time `json:"suspendedAt"`
}

// Suspend saves the instance's memory and device state under stateDir through
// QMP migration, then stops it. The instance is unusable afterwards; Resume
// returns a new one.
func (b *qemuBackend) Suspend(ctx context.Context, inst backend.Instance, stateDir string) error {
	i, err := b.instance(inst)
	if err != nil {
		return err
	}
	monitor, err := i.monitor()
	if err != nil {
		return err
	}
	if stateDir == "" {
		return fmt.Errorf("suspend state directory is required")
	}
	if err := os.MkdirAll(stateDir, 0o755); err != nil {
		return fmt.Errorf("create suspend state directory %q: %w", stateDir, err)
	}
	vmStatePath := filepath.Join(stateDir, suspendVMStateFile)

	saveCtx, cancel := context.WithTimeout(ctx, migrationTimeout)
	defer cancel()
	i.logger.Info("saving vm state", "path", vmStatePath)
	if err := qmpclient.SaveToFile(saveCtx, monitor, vmStatePath); err != nil {
		return err
	}

	state := suspendState{
		Version:     suspendStateVersion,
		HostName:    i.manifest.Identity.HostName,
		CID:         i.cid,
		VMStatePath: vmStatePath,
		SuspendedAt: time.Now(),
	}
	if err := writeSuspendState(stateDir, state); err != nil {
		return errors.Join(err, i.Kill())
	}
	// The VM's state is on disk now; the process holding it must go, or the
	// restored machine would collide with it over sockets and its CID.
	return i.Kill()
}

// Resume starts a machine from state previously written by Suspend. The spec
// must describe the same machine that was suspended: QEMU restores memory into
// a machine it expects to match. On success the saved state is consumed, so a
// machine cannot be resumed twice from one suspend.
func (b *qemuBackend) Resume(ctx context.Context, spec *vm.Spec, stateDir string) (backend.Instance, error) {
	state, err := readSuspendState(stateDir)
	if err != nil {
		return nil, err
	}
	document, err := b.config.document(spec)
	if err != nil {
		return nil, err
	}

	inst, err := b.start(ctx, document, &state)
	if err != nil {
		return nil, err
	}
	monitor, err := inst.monitor()
	if err != nil {
		return nil, errors.Join(err, inst.Kill())
	}

	restoreCtx, cancel := context.WithTimeout(ctx, migrationTimeout)
	defer cancel()
	inst.logger.Info("restoring vm state", "path", state.VMStatePath)
	if err := qmpclient.RestoreFromFile(restoreCtx, monitor, state.VMStatePath); err != nil {
		return nil, errors.Join(err, inst.Kill())
	}
	if err := removeSuspendState(stateDir, state); err != nil {
		return nil, errors.Join(err, inst.Kill())
	}
	return inst, nil
}

// instance narrows a backend.Instance to one this backend started, since the
// capability operations reach into its internals.
func (b *qemuBackend) instance(inst backend.Instance) (*instance, error) {
	typed, ok := inst.(*instance)
	if !ok {
		return nil, fmt.Errorf("instance was not started by the qemu backend")
	}
	if typed.backend != b {
		return nil, fmt.Errorf("instance was started by a different qemu backend")
	}
	return typed, nil
}

func writeSuspendState(stateDir string, state suspendState) error {
	data, err := json.Marshal(state)
	if err != nil {
		return fmt.Errorf("encode suspend state: %w", err)
	}
	path := filepath.Join(stateDir, suspendStateFile)
	if err := os.WriteFile(path, append(data, '\n'), 0o644); err != nil {
		return fmt.Errorf("write suspend state %q: %w", path, err)
	}
	return nil
}

func readSuspendState(stateDir string) (suspendState, error) {
	if stateDir == "" {
		return suspendState{}, fmt.Errorf("suspend state directory is required")
	}
	path := filepath.Join(stateDir, suspendStateFile)
	data, err := os.ReadFile(path)
	if err != nil {
		return suspendState{}, fmt.Errorf("read suspend state %q: %w", path, err)
	}
	var state suspendState
	if err := json.Unmarshal(data, &state); err != nil {
		return suspendState{}, fmt.Errorf("decode suspend state %q: %w", path, err)
	}
	if state.Version != suspendStateVersion {
		return suspendState{}, fmt.Errorf(
			"suspend state %q has version %d, this virtle writes version %d: resume with the virtle that suspended it, or discard the state",
			path, state.Version, suspendStateVersion)
	}
	if state.VMStatePath == "" {
		return suspendState{}, fmt.Errorf("suspend state %q names no vm state file", path)
	}
	return state, nil
}

func removeSuspendState(stateDir string, state suspendState) error {
	return errors.Join(
		os.Remove(filepath.Join(stateDir, suspendStateFile)),
		os.Remove(state.VMStatePath),
	)
}
