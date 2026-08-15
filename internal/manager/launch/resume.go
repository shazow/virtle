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

// ResolveResumeState reads and validates saved suspend state. Only state
// whose version token equals stateVersion is resumable.
func ResolveResumeState(manifest *manifest.Manifest, mode ResumeMode, stateVersion string) (*SuspendState, error) {
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
	// A version mismatch errors even in auto mode: silently booting fresh
	// would abandon the suspended session, which is exactly the surprise
	// the marker exists to prevent.
	if state.Version != stateVersion {
		written := fmt.Sprintf("state version %q", state.Version)
		if state.Version == "" {
			written = "no state version marker (written by an older virtle)"
		}
		return nil, fmt.Errorf(
			"suspend state %q has %s; this virtle resumes %q — resume with the virtle that wrote it or discard the state",
			SuspendStatePath(manifest), written, stateVersion)
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
