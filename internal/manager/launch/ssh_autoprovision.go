package launch

import (
	"context"

	"github.com/shazow/virtle/internal/manifest"
	"github.com/shazow/virtle/internal/sshtools"
)

const GuestShellPath = "sh"

type SSHAuthorizedKeyInstaller struct {
	InstallDirectory func(ctx context.Context, guestPath string, owner string, mode string) error
	Chown            func(ctx context.Context, guestPath string, owner string) error
	Chmod            func(ctx context.Context, guestPath string, mode string) error
	WriteFile        func(ctx context.Context, guestPath string, payloadBase64 string) error
	RunCommand       func(ctx context.Context, name string, path string, args []string, inputPath string) error
}

func InstallSSHAuthorizedKey(ctx context.Context, launchManifest *manifest.Manifest, key SSHAutoprovisionKey, installer SSHAuthorizedKeyInstaller) error {
	plan := sshtools.NewAuthorizedKeysInstallPlan(launchManifest.SSH.User, key.AuthorizedKey)
	if err := installer.InstallDirectory(ctx, plan.AuthorizedKeysPath, plan.Owner, "0700"); err != nil {
		return wrapStage("ssh autoprovision", err)
	}
	if err := installer.Chown(ctx, plan.SSHDir, plan.Owner); err != nil {
		return wrapStage("ssh autoprovision", err)
	}
	if err := installer.Chmod(ctx, plan.SSHDir, "0700"); err != nil {
		return wrapStage("ssh autoprovision", err)
	}

	tempFile := manifest.ResolvedWriteFile{
		GuestPath: plan.TempKeyPath,
		Mode:      plan.TempKeyMode,
		Overwrite: true,
		Content: manifest.WriteFileContent{
			Kind: manifest.WriteFileContentText,
			Text: plan.TempKeyText,
		},
	}
	payloadBase64, err := guestFilePayloadBase64(tempFile)
	if err != nil {
		return wrapStage("ssh autoprovision", err)
	}
	if err := installer.WriteFile(ctx, plan.TempKeyPath, payloadBase64); err != nil {
		return wrapStage("ssh autoprovision", err)
	}
	if err := installer.Chmod(ctx, plan.TempKeyPath, plan.TempKeyMode); err != nil {
		return wrapStage("ssh autoprovision", err)
	}

	command := plan.AppendCommand(GuestShellPath)
	if err := installer.RunCommand(ctx, command.Name, command.Path, command.Args, command.InputPath); err != nil {
		return wrapStage("ssh autoprovision", err)
	}
	if err := installer.Chown(ctx, plan.AuthorizedKeysPath, plan.Owner); err != nil {
		return wrapStage("ssh autoprovision", err)
	}
	if err := installer.Chmod(ctx, plan.AuthorizedKeysPath, "0600"); err != nil {
		return wrapStage("ssh autoprovision", err)
	}
	return nil
}
