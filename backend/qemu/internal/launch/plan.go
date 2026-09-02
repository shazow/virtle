package launch

func BuildPlan(spec Spec, resumeState *SuspendState, notifier NotificationSink) (*Plan, error) {
	manifest := spec.Manifest
	options := spec.Options
	if err := manifest.Validate(); err != nil {
		return nil, err
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
