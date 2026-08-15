package launch

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestNormalizeResumeMode(t *testing.T) {
	mode, err := NormalizeResumeMode("")
	if err != nil {
		t.Fatalf("normalize empty resume mode: %v", err)
	}
	if mode != ResumeModeNo {
		t.Fatalf("unexpected empty resume mode: got %q want %q", mode, ResumeModeNo)
	}
	if _, err := NormalizeResumeMode("maybe"); err == nil || !strings.Contains(err.Error(), "unsupported resume mode") {
		t.Fatalf("expected unsupported resume mode error, got %v", err)
	}
}

func TestResolveResumeState(t *testing.T) {
	cfg := testManifest(t)
	if state, err := ResolveResumeState(cfg, ResumeModeAuto); err != nil || state != nil {
		t.Fatalf("auto without state: state=%#v err=%v", state, err)
	}
	if _, err := ResolveResumeState(cfg, ResumeModeForce); err == nil || !strings.Contains(err.Error(), "no saved suspend state") {
		t.Fatalf("expected missing state error, got %v", err)
	}

	vmStatePath := VMStatePath(cfg)
	if err := os.MkdirAll(filepath.Dir(vmStatePath), 0o755); err != nil {
		t.Fatalf("create vm state dir: %v", err)
	}
	if err := os.WriteFile(vmStatePath, []byte("state"), 0o644); err != nil {
		t.Fatalf("write vm state: %v", err)
	}
	if err := WriteSuspendStateData(cfg, SuspendState{
		QMPSocketPath: "/run/qmp.sock",
		VMStatePath:   vmStatePath,
		CID:           7,
		Status:        "saved",
	}); err != nil {
		t.Fatalf("write suspend state: %v", err)
	}
	state, err := ResolveResumeState(cfg, ResumeModeForce)
	if err != nil {
		t.Fatalf("resolve resume state: %v", err)
	}
	if state == nil || state.CID != 7 || state.VMStatePath != vmStatePath {
		t.Fatalf("unexpected resume state: %#v", state)
	}
	if state.Version != SuspendStateVersion {
		t.Fatalf("unexpected suspend state version: got %q want %q", state.Version, SuspendStateVersion)
	}
}

func TestResolveResumeStateRejectsVersionMismatch(t *testing.T) {
	cfg := testManifest(t)
	vmStatePath := VMStatePath(cfg)
	if err := os.MkdirAll(filepath.Dir(vmStatePath), 0o755); err != nil {
		t.Fatalf("create vm state dir: %v", err)
	}
	if err := os.WriteFile(vmStatePath, []byte("state"), 0o644); err != nil {
		t.Fatalf("write vm state: %v", err)
	}
	if err := WriteSuspendStateData(cfg, SuspendState{
		Version:       "futurebackend-v9",
		QMPSocketPath: "/run/qmp.sock",
		VMStatePath:   vmStatePath,
		CID:           7,
		Status:        "saved",
	}); err != nil {
		t.Fatalf("write suspend state: %v", err)
	}

	// The mismatch errors even in auto mode: silently booting fresh would
	// abandon the suspended session.
	for _, mode := range []ResumeMode{ResumeModeAuto, ResumeModeForce} {
		if _, err := ResolveResumeState(cfg, mode); err == nil || !strings.Contains(err.Error(), `state version "futurebackend-v9"`) {
			t.Fatalf("mode %q: expected version mismatch error, got %v", mode, err)
		}
	}
}

func TestResolveResumeStateRejectsPreVersionState(t *testing.T) {
	cfg := testManifest(t)
	vmStatePath := VMStatePath(cfg)
	if err := os.MkdirAll(filepath.Dir(vmStatePath), 0o755); err != nil {
		t.Fatalf("create vm state dir: %v", err)
	}
	if err := os.WriteFile(vmStatePath, []byte("state"), 0o644); err != nil {
		t.Fatalf("write vm state: %v", err)
	}
	// A pre-marker state file has no version field and decodes as "".
	statePath := SuspendStatePath(cfg)
	if err := os.MkdirAll(filepath.Dir(statePath), 0o755); err != nil {
		t.Fatalf("create state dir: %v", err)
	}
	stateJSON := `{"hostName":"h","qmpSocketPath":"/run/qmp.sock","vmStatePath":"` + vmStatePath + `","cid":7,"status":"saved"}`
	if err := os.WriteFile(statePath, []byte(stateJSON), 0o644); err != nil {
		t.Fatalf("write suspend state: %v", err)
	}

	if _, err := ResolveResumeState(cfg, ResumeModeForce); err == nil || !strings.Contains(err.Error(), "no state version marker") {
		t.Fatalf("expected pre-version state error, got %v", err)
	}
}
