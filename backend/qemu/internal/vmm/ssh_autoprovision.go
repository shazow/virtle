package vmm

import (
	"context"

	"github.com/shazow/virtle/backend/qemu/internal/launch"
	"github.com/shazow/virtle/internal/executor"
	"github.com/shazow/virtle/internal/sshtools"
)

func (m *manager) ensureSSHAutoprovisionKey() (launch.SSHAutoprovisionKey, error) {
	launchManifest := m.launchManifest
	key, err := (sshtools.KeyStore{
		Dir:     launchManifest.ResolvedPersistenceStateDir(),
		Comment: "virtle-autoprovision-" + launchManifest.Identity.HostName,
	}).Ensure()
	if err != nil {
		return launch.SSHAutoprovisionKey{}, err
	}
	return launch.SSHAutoprovisionKey{
		IdentityFile:  key.IdentityFile,
		PublicKeyFile: key.PublicKeyFile,
		AuthorizedKey: key.AuthorizedKey,
	}, nil
}

func (m *manager) installSSHAutoprovisionKey(ctx context.Context, key launch.SSHAutoprovisionKey, watchers executor.Group) error {
	launchManifest := m.launchManifest
	socketPath, err := launchManifest.ResolvedGuestAgentSocketPath()
	if err != nil {
		return &launch.StageError{Stage: "ssh autoprovision", Err: err}
	}
	client, err := m.waitForGuestAgentStage(ctx, "ssh autoprovision", socketPath, watchers)
	if err != nil {
		return err
	}
	defer client.Disconnect()

	return launch.InstallSSHAuthorizedKey(ctx, launchManifest, key, launch.SSHAuthorizedKeyInstaller{
		InstallDirectory: func(ctx context.Context, guestPath string, owner string, mode string) error {
			return m.installGuestFileDirectory(ctx, client, guestPath, owner, mode)
		},
		Chown: func(ctx context.Context, guestPath string, owner string) error {
			return m.chownGuestFile(ctx, client, guestPath, owner)
		},
		Chmod: func(ctx context.Context, guestPath string, mode string) error {
			return m.chmodGuestFile(ctx, client, guestPath, mode)
		},
		WriteFile: func(ctx context.Context, guestPath string, payloadBase64 string) error {
			return m.writeGuestFile(ctx, client, guestPath, payloadBase64)
		},
		RunCommand: func(ctx context.Context, name string, path string, args []string, inputPath string) error {
			return m.runGuestFileCommand(ctx, client, name, path, args, inputPath)
		},
	})
}
