package manager

import (
	"context"

	"github.com/shazow/virtle/internal/executor"
	"github.com/shazow/virtle/internal/manager/launch"
	"github.com/shazow/virtle/internal/manifest"
	"github.com/shazow/virtle/internal/sshtools"
)

func (m *manager) ensureSSHAutoprovisionKey(launchManifest *manifest.Manifest) (launch.SSHAutoprovisionKey, error) {
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

func (m *manager) installSSHAutoprovisionKey(ctx context.Context, launchManifest *manifest.Manifest, key launch.SSHAutoprovisionKey, watchers executor.Group) error {
	socketPath, err := launchManifest.ResolvedGuestAgentSocketPath()
	if err != nil {
		return &launch.StageError{Stage: "ssh autoprovision", Err: err}
	}
	timeout := launchManifest.QEMU.GuestAgent.CommandTimeout
	client, err := m.waitForGuestAgentStage(ctx, "ssh autoprovision", timeout, socketPath, watchers)
	if err != nil {
		return err
	}
	defer client.Disconnect()

	return launch.InstallSSHAuthorizedKey(ctx, launchManifest, key, launch.SSHAuthorizedKeyInstaller{
		InstallDirectory: func(ctx context.Context, guestPath string, owner string, mode string) error {
			return m.installGuestFileDirectory(ctx, client, timeout, guestPath, owner, mode)
		},
		Chown: func(ctx context.Context, guestPath string, owner string) error {
			return m.chownGuestFile(ctx, client, timeout, guestPath, owner)
		},
		Chmod: func(ctx context.Context, guestPath string, mode string) error {
			return m.chmodGuestFile(ctx, client, timeout, guestPath, mode)
		},
		WriteFile: func(_ context.Context, guestPath string, payloadBase64 string) error {
			return m.writeGuestFile(client, timeout, guestPath, payloadBase64)
		},
		RunCommand: func(ctx context.Context, name string, path string, args []string, inputPath string) error {
			return m.runGuestFileCommand(ctx, client, timeout, name, path, args, inputPath)
		},
	})
}
