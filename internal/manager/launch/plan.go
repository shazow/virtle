package launch

import "fmt"

func BuildPlan(spec Spec, resumeState *SuspendState, notifier NotificationSink) (*Plan, error) {
	manifest := spec.Manifest
	remoteCommand := spec.RemoteCommand
	options := spec.Options
	if err := manifest.Validate(); err != nil {
		return nil, err
	}
	if options.SSH && len(remoteCommand) > 0 && len(manifest.SSH.Argv) == 0 {
		return nil, fmt.Errorf("remote command arguments require manifest.ssh.exec")
	}
	virtioFSSocketPaths, err := manifest.ResolvedVirtioFSSocketPaths()
	if err != nil {
		return nil, err
	}
	externalVirtioFSSocketPaths, err := manifest.ResolvedExternalVirtioFSSocketPaths()
	if err != nil {
		return nil, err
	}
	cleanupFiles, err := manifest.ResolvedCleanupFiles()
	if err != nil {
		return nil, err
	}
	qmpSocketPath, err := manifest.ResolvedQMPSocketPath()
	if err != nil {
		return nil, err
	}
	guestAgentSocketPath, err := manifest.ResolvedGuestAgentSocketPath()
	if err != nil {
		return nil, err
	}
	sshReadySocketPath, err := manifest.ResolvedSSHReadySocketPath()
	if err != nil {
		return nil, err
	}
	controlSocketPath, err := manifest.ResolvedControlSocketPath()
	if err != nil {
		return nil, err
	}
	volumes := manifest.ResolvedVolumes()
	volumeImagePaths := make([]string, 0, len(volumes))
	for _, volume := range volumes {
		volumeImagePaths = append(volumeImagePaths, volume.ImagePath)
	}
	return &Plan{
		Manifest:                    manifest,
		RemoteCommand:               append([]string(nil), remoteCommand...),
		Options:                     options,
		ResumeState:                 resumeState,
		Notifier:                    notifier,
		Paths:                       RuntimePaths{StateDir: manifest.ResolvedPersistenceStateDir(), ControlSocket: controlSocketPath, QMPSocket: qmpSocketPath, GuestAgentSocket: guestAgentSocketPath, SSHReadySocket: sshReadySocketPath},
		VirtioFSSocketPaths:         virtioFSSocketPaths,
		ExternalVirtioFSSocketPaths: externalVirtioFSSocketPaths,
		CleanupFiles:                append([]string(nil), cleanupFiles...),
		Volumes:                     volumes,
		VolumeImagePaths:            volumeImagePaths,
	}, nil
}
