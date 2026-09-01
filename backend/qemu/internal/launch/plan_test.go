package launch

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/shazow/virtle/internal/manifest"
)

func TestBuildPlanResolvesRuntimeInputs(t *testing.T) {
	tmpDir := t.TempDir()
	cfg := validPlanManifest(tmpDir)
	notifier := fakeNotifier{}
	plan, err := BuildPlan(Spec{
		Manifest:      cfg,
		RemoteCommand: []string{"uname", "-a"},
		Options:       Options{Resume: ResumeModeNo, SSH: true},
	}, nil, notifier)
	if err != nil {
		t.Fatalf("build plan: %v", err)
	}

	if plan.Manifest != cfg || plan.Notifier != notifier {
		t.Fatalf("plan did not preserve manifest/notifier")
	}
	if got, want := plan.RemoteCommand, []string{"uname", "-a"}; len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("unexpected remote command: %#v", got)
	}
	if plan.Paths.QMPSocket == "" || plan.Paths.ControlSocket == "" || plan.Paths.StateDir == "" {
		t.Fatalf("expected resolved runtime paths, got %#v", plan.Paths)
	}
	if len(plan.VolumeImagePaths) != 1 || plan.VolumeImagePaths[0] != filepath.Join(tmpDir, "root.img") {
		t.Fatalf("unexpected volume image paths: %#v", plan.VolumeImagePaths)
	}
}

type fakeNotifier struct{}

func (fakeNotifier) Notify(context.Context, string, string, map[string]string) {}

func validPlanManifest(workingDir string) *manifest.Manifest {
	return &manifest.Manifest{
		Identity: manifest.Identity{HostName: "agent-sandbox"},
		Paths: manifest.Paths{
			WorkingDir: workingDir,
			LockPath:   filepath.Join(workingDir, "virtle.lock"),
		},
		SSH: manifest.SSH{
			Argv:       []string{"/bin/ssh"},
			User:       "agent",
			RetryDelay: 500 * time.Millisecond,
		},
		QEMU: manifest.QEMU{
			BinaryPath: "/bin/qemu-system-x86_64",
			Memory:     manifest.QEMUMemory{Size: 128},
			QMP: manifest.QEMUQMP{
				SocketPath: "qmp.sock",
			},
			Devices: manifest.QEMUDevices{
				RNG: manifest.QEMURNGDevice{
					Transport: "pci",
				},
				VSOCK: manifest.QEMUVSOCKDevice{
					Transport: "pci",
				},
			},
		},
		Volumes: []manifest.Volume{
			{
				ImagePath:  "root.img",
				Size:       256,
				FSType:     "ext4",
				AutoCreate: true,
			},
		},
	}
}
