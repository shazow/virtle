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
}
