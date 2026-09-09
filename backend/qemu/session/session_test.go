package session

import (
	"context"
	"errors"
	"net"
	"path/filepath"
	"sync"
	"testing"

	"github.com/shazow/virtle/backend"
	"github.com/shazow/virtle/backend/backendtest"
	"github.com/shazow/virtle/backend/qemu/internal/launch"
	"github.com/shazow/virtle/internal/manifest"
	shared "github.com/shazow/virtle/internal/session"
	"github.com/shazow/virtle/internal/sessionbridge"
	"github.com/shazow/virtle/vm"
	"github.com/shazow/virtle/vm/vmtest"
)

// commitTracker resumes in-memory machines and records whether the session
// committed the restored state, the one bridge hook the SSH attach drives.
type commitTracker struct {
	backend.Backend
	mu        sync.Mutex
	committed bool
}

func (b *commitTracker) Resume(ctx context.Context, spec *vm.Spec) (backend.Machine, error) {
	if bridge := sessionbridge.FromContext(ctx); bridge != nil {
		bridge.Bind(sessionbridge.Hooks{CommitResume: func() error {
			b.mu.Lock()
			b.committed = true
			b.mu.Unlock()
			return nil
		}})
	}
	return b.Backend.Start(ctx, spec)
}

func (*commitTracker) StateVersion() string { return "test-v1" }

func (b *commitTracker) committedResume() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.committed
}

func TestRunPreservesResumeStateWhenSSHStartFails(t *testing.T) {
	b := &commitTracker{Backend: backendtest.NewMemoryBackend(nil)}
	mf := &manifest.Manifest{SSH: manifest.SSH{Argv: []string{filepath.Join(t.TempDir(), "missing-ssh")}}}
	err := shared.Run(context.Background(), b, &vm.Spec{}, mf, shared.Options{Resume: shared.ResumeForce, SSH: true, Hooks: Hooks()})
	if err == nil {
		t.Fatal("Run unexpectedly started a missing SSH executable")
	}
	if b.committedResume() {
		t.Fatal("Run committed restored state before the SSH process started")
	}
}

func TestInstallSSHKeyWritesTemporaryAuthorizedKey(t *testing.T) {
	guest := &vmtest.Guest{Commands: map[string]vmtest.Result{
		"/bin/sh": {},
		"chown":   {},
		"chmod":   {},
	}}
	m, err := backendtest.NewMemoryBackend(guest).Start(context.Background(), &vm.Spec{})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	want := "ssh-ed25519 AAAA-test virtle-test\n"
	if err := installSSHKey(context.Background(), m, &manifest.Manifest{
		SSH: manifest.SSH{User: "agent"},
	}, launch.SSHAutoprovisionKey{AuthorizedKey: want[:len(want)-1]}); err != nil {
		t.Fatalf("installSSHKey: %v", err)
	}
	file := guest.FS["/run/virtle-autoprovision-authorized-key.pub"]
	if file == nil || string(file.Data) != want || file.Mode != 0o600 {
		t.Fatalf("temporary key = %#v, want data %q and mode 0600", file, want)
	}
}

// readyReporter reports a readiness socket for an in-memory machine.
type readyReporter struct {
	backend.Machine
	readyPath string
}

func (m *readyReporter) Status(ctx context.Context) (backend.Status, error) {
	status, err := m.Machine.(backend.StatusReporter).Status(ctx)
	status.Paths.ReadySocket = m.readyPath
	return status, err
}

func TestReadyHasDeadline(t *testing.T) {
	t.Setenv("VIRTLE_SSH_READY_TIMEOUT", "20ms")
	path := filepath.Join(t.TempDir(), "ready.sock")
	listener, err := net.Listen("unix", path)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer listener.Close()
	m, err := backendtest.NewMemoryBackend(nil).Start(context.Background(), &vm.Spec{})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	// The listener accepts but never writes the token.
	err = ready(context.Background(), &readyReporter{Machine: m, readyPath: path})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("ready error = %v, want context.DeadlineExceeded", err)
	}
}
