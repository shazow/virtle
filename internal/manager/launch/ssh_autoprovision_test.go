package launch

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/shazow/virtle/internal/manifest"
)

func TestInstallSSHAuthorizedKeySequencesGuestInstall(t *testing.T) {
	var events []string
	err := InstallSSHAuthorizedKey(context.Background(), &manifest.Manifest{
		SSH: manifest.SSH{User: "agent"},
	}, SSHAutoprovisionKey{AuthorizedKey: "ssh-ed25519 abc"}, SSHAuthorizedKeyInstaller{
		InstallDirectory: func(_ context.Context, guestPath string, owner string, mode string) error {
			events = append(events, "install:"+guestPath+":"+owner+":"+mode)
			return nil
		},
		Chown: func(_ context.Context, guestPath string, owner string) error {
			events = append(events, "chown:"+guestPath+":"+owner)
			return nil
		},
		Chmod: func(_ context.Context, guestPath string, mode string) error {
			events = append(events, "chmod:"+guestPath+":"+mode)
			return nil
		},
		WriteFile: func(_ context.Context, guestPath string, payloadBase64 string) error {
			events = append(events, "write:"+guestPath+":"+payloadBase64)
			return nil
		},
		RunCommand: func(_ context.Context, name string, path string, args []string, inputPath string) error {
			events = append(events, "run:"+name+":"+path+":"+strings.Join(args, ",")+":"+inputPath)
			return nil
		},
	})
	if err != nil {
		t.Fatalf("install ssh authorized key: %v", err)
	}
	want := []string{
		"install:/home/agent/.ssh/authorized_keys:agent:users:0700",
		"chown:/home/agent/.ssh:agent:users",
		"chmod:/home/agent/.ssh:0700",
		"write:/run/virtie-autoprovision-authorized-key.pub:c3NoLWVkMjU1MTkgYWJjCg==",
		"chmod:/run/virtie-autoprovision-authorized-key.pub:0600",
		"run:append authorized_keys:" + GuestShellPath + ":",
		"chown:/home/agent/.ssh/authorized_keys:agent:users",
		"chmod:/home/agent/.ssh/authorized_keys:0600",
	}
	if len(events) != len(want) {
		t.Fatalf("events: got %#v want %#v", events, want)
	}
	for i := range want {
		if strings.HasSuffix(want[i], ":") {
			if !strings.HasPrefix(events[i], want[i]) {
				t.Fatalf("event %d: got %q want prefix %q", i, events[i], want[i])
			}
			continue
		}
		if events[i] != want[i] {
			t.Fatalf("event %d: got %q want %q", i, events[i], want[i])
		}
	}
}

func TestInstallSSHAuthorizedKeyWrapsStage(t *testing.T) {
	chownErr := errors.New("chown failed")
	err := InstallSSHAuthorizedKey(context.Background(), &manifest.Manifest{
		SSH: manifest.SSH{User: "agent"},
	}, SSHAutoprovisionKey{AuthorizedKey: "ssh-ed25519 abc"}, SSHAuthorizedKeyInstaller{
		InstallDirectory: func(context.Context, string, string, string) error {
			return nil
		},
		Chown: func(context.Context, string, string) error {
			return chownErr
		},
	})
	var stageErr *StageError
	if !errors.As(err, &stageErr) || stageErr.Stage != "ssh autoprovision" || !errors.Is(err, chownErr) {
		t.Fatalf("stage err: got %v", err)
	}
}

func TestInstallSSHAuthorizedKeyRootPath(t *testing.T) {
	var installed []string
	if err := InstallSSHAuthorizedKey(context.Background(), &manifest.Manifest{
		SSH: manifest.SSH{User: "root"},
	}, SSHAutoprovisionKey{AuthorizedKey: "ssh-ed25519 abc"}, SSHAuthorizedKeyInstaller{
		InstallDirectory: func(_ context.Context, guestPath string, owner string, mode string) error {
			installed = append(installed, guestPath, owner, mode)
			return nil
		},
		Chown:      func(context.Context, string, string) error { return nil },
		Chmod:      func(context.Context, string, string) error { return nil },
		WriteFile:  func(context.Context, string, string) error { return nil },
		RunCommand: func(context.Context, string, string, []string, string) error { return nil },
	}); err != nil {
		t.Fatalf("install ssh authorized key: %v", err)
	}
	if want := []string{"/root/.ssh/authorized_keys", "root:users", "0700"}; !reflect.DeepEqual(installed, want) {
		t.Fatalf("installed: got %#v want %#v", installed, want)
	}
}
