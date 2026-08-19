package launch

import (
	"os"
	"os/exec"

	"github.com/shazow/virtle/internal/executor"
	"github.com/shazow/virtle/internal/manifest"
	"github.com/shazow/virtle/internal/sshtools"
)

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
	return cmd, nil
}

// BuildSSHCommandHint renders the copy-pasteable SSH command hint shown after
// launch.
func BuildSSHCommandHint(launchManifest *manifest.Manifest, cid int) (string, error) {
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
	return sshtools.Config{Exec: argv, User: launchManifest.SSH.User}.Hint(cid), nil
}
