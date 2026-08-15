package launch

import (
	"fmt"
	"os"

	"github.com/shazow/virtle/internal/manifest"
)

func NormalizeResumeMode(mode ResumeMode) (ResumeMode, error) {
	switch mode {
	case "", ResumeModeNo:
		return ResumeModeNo, nil
	case ResumeModeAuto, ResumeModeForce:
		return mode, nil
	default:
		return "", fmt.Errorf("unsupported resume mode %q", mode)
	}
}

func ResolveResumeState(manifest *manifest.Manifest, mode ResumeMode) (*SuspendState, error) {
	if mode == ResumeModeNo {
		return nil, nil
	}

	state, err := ReadSuspendState(manifest)
	if err != nil {
		if os.IsNotExist(err) && mode == ResumeModeAuto {
			return nil, nil
		}
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("no saved suspend state found at %q; run virtle suspend first", SuspendStatePath(manifest))
		}
		return nil, err
	}
	if state.Status != "saved" {
		if mode == ResumeModeAuto {
			return nil, nil
		}
		return nil, fmt.Errorf("suspend state %q has status %q, not saved; run virtle suspend first", SuspendStatePath(manifest), state.Status)
	}
	// Backend and version mismatches error even in auto mode: silently
	// booting fresh would abandon the suspended session, which is exactly
	// the surprise the markers exist to prevent. The backend check runs
	// first — a foreign backend's state is the more fundamental mismatch.
	if state.Backend != "" && state.Backend != SuspendStateBackend {
		return nil, fmt.Errorf(
			"suspend state %q was written by the %q backend and cannot be resumed by the %q backend; resume with that backend or discard the state",
			SuspendStatePath(manifest), state.Backend, SuspendStateBackend)
	}
	if state.Version != SuspendStateVersion {
		return nil, fmt.Errorf(
			"suspend state %q was written by a different virtle (state version %d, this virtle resumes version %d); resume with that version or discard the state with --resume=no and a fresh suspend",
			SuspendStatePath(manifest), state.Version, SuspendStateVersion)
	}
	if state.CID <= 0 {
		if mode == ResumeModeAuto {
			return nil, nil
		}
		return nil, fmt.Errorf("saved suspend state %q does not include a valid vsock CID", SuspendStatePath(manifest))
	}
	if state.VMStatePath == "" {
		state.VMStatePath = VMStatePath(manifest)
	}
	if _, err := os.Stat(state.VMStatePath); err != nil {
		if mode == ResumeModeAuto {
			return nil, nil
		}
		return nil, fmt.Errorf("saved vm state %q is not available: %w", state.VMStatePath, err)
	}
	return &state, nil
}
