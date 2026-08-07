package manager

import (
	"context"

	"github.com/shazow/virtle/internal/executor"
	"github.com/shazow/virtle/internal/manager/launch"
	"github.com/shazow/virtle/internal/manifest"
	"github.com/shazow/virtle/internal/qga"
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
	client, err := m.waitForGuestAgentStage(ctx, "ssh autoprovision", socketPath, watchers)
	if err != nil {
		return err
	}
	defer client.Disconnect()

	guestCtx := guestContextFor(launchManifest)
	return launch.InstallSSHAuthorizedKey(ctx, launchManifest, key, launch.SSHAuthorizedKeyInstaller{
		InstallDirectory: func(ctx context.Context, guestPath string, owner string, mode string) error {
			ctx, cancel := guestCtx(ctx)
			defer cancel()
			return m.installGuestFileDirectory(ctx, client, guestPath, owner, mode)
		},
		Chown: func(ctx context.Context, guestPath string, owner string) error {
			ctx, cancel := guestCtx(ctx)
			defer cancel()
			return m.chownGuestFile(ctx, client, guestPath, owner)
		},
		Chmod: func(ctx context.Context, guestPath string, mode string) error {
			ctx, cancel := guestCtx(ctx)
			defer cancel()
			return m.chmodGuestFile(ctx, client, guestPath, mode)
		},
		WriteFile: func(ctx context.Context, guestPath string, payloadBase64 string) error {
			ctx, cancel := guestCtx(ctx)
			defer cancel()
			return qga.WriteFile(ctx, client, guestPath, payloadBase64)
		},
		RunCommand: func(ctx context.Context, name string, path string, args []string, inputPath string) error {
			ctx, cancel := guestCtx(ctx)
			defer cancel()
			return m.runGuestFileCommand(ctx, client, name, path, args, inputPath)
		},
	})
}
