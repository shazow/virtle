package launch

import (
	"os"
	"os/exec"

	"github.com/shazow/virtle/internal/executor"
	"github.com/shazow/virtle/internal/manifest"
	"github.com/shazow/virtle/internal/sshtools"
)

func buildSSHCommand(launchManifest *manifest.Manifest, cid int, remoteCommand []string) (*exec.Cmd, error) {
	return buildSSHCommandWithArgv(launchManifest, cid, remoteCommand, launchManifest.SSH.Argv)
}

func buildSSHCommandWithArgv(launchManifest *manifest.Manifest, cid int, remoteCommand []string, argv []string) (*exec.Cmd, error) {
	renderer, err := manifest.NewTemplateRenderer(manifest.SSHTemplateProvider{
		CID:         cid,
		User:        launchManifest.SSH.User,
		Destination: sshtools.VSockDestination(launchManifest.SSH.User, cid),
	})
	if err != nil {
		return nil, err
	}
	renderedArgv, err := renderer.RenderArgv(argv)
	if err != nil {
		return nil, err
	}
	command, err := sshtools.NewCommand(sshtools.Config{Exec: renderedArgv, User: launchManifest.SSH.User}, cid, remoteCommand)
	if err != nil {
		return nil, err
	}
	cmd := executor.Command(command.Path, command.Args, renderer.Env())
	cmd.Dir = launchManifest.Paths.WorkingDir
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd, nil
}

func buildSSHCommandHint(launchManifest *manifest.Manifest, cid int) (string, error) {
	renderer, err := manifest.NewTemplateRenderer(manifest.SSHTemplateProvider{
		CID:         cid,
		User:        launchManifest.SSH.User,
		Destination: sshtools.VSockDestination(launchManifest.SSH.User, cid),
	})
	if err != nil {
		return "", err
	}
	argv, err := renderer.RenderArgv(launchManifest.SSH.Argv)
	if err != nil {
		return "", err
	}
	return sshtools.CommandHint(sshtools.Config{Exec: argv, User: launchManifest.SSH.User}, cid), nil
}
