package launch

import (
	"context"
	"encoding/base64"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"strings"

	"github.com/shazow/virtle/internal/manifest"
)

func guestFilePayloadBase64(file manifest.ResolvedWriteFile) (string, error) {
	if file.Content.Kind == manifest.WriteFileContentText {
		return base64.StdEncoding.EncodeToString([]byte(file.Content.Text)), nil
	}
	if file.Content.Kind != manifest.WriteFileContentPath {
		return "", fmt.Errorf("guest file %q has no text or host path", file.GuestPath)
	}

	data, err := readHostFileForGuest(file)
	if err != nil {
		return "", err
	}
	return base64.StdEncoding.EncodeToString(data), nil
}

func readHostFileForGuest(file manifest.ResolvedWriteFile) ([]byte, error) {
	if file.Content.Kind != manifest.WriteFileContentPath {
		return nil, fmt.Errorf("guest file %q has no host path", file.GuestPath)
	}
	if !file.FollowLinks {
		info, err := os.Lstat(file.Content.Path)
		if err != nil {
			return nil, fmt.Errorf("stat host file %q for guest path %q: %w", file.Content.Path, file.GuestPath, err)
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return nil, fmt.Errorf("host file %q for guest path %q is a symlink and followLinks is false", file.Content.Path, file.GuestPath)
		}
	}
	data, err := os.ReadFile(file.Content.Path)
	if err != nil {
		return nil, fmt.Errorf("read host file %q for guest path %q: %w", file.Content.Path, file.GuestPath, err)
	}
	return data, nil
}

func writeBackHostPath(file manifest.ResolvedWriteFile) (string, error) {
	if file.Content.Kind != manifest.WriteFileContentPath {
		return "", fmt.Errorf("guest file %q has no host path", file.GuestPath)
	}
	info, err := os.Lstat(file.Content.Path)
	if err != nil {
		return "", fmt.Errorf("stat host file %q for guest path %q: %w", file.Content.Path, file.GuestPath, err)
	}
	if info.Mode()&os.ModeSymlink == 0 {
		return file.Content.Path, nil
	}
	if !file.FollowLinks {
		return "", fmt.Errorf("host file %q for guest path %q is a symlink and followLinks is false", file.Content.Path, file.GuestPath)
	}
	resolvedPath, err := filepath.EvalSymlinks(file.Content.Path)
	if err != nil {
		return "", fmt.Errorf("resolve host symlink %q for guest path %q: %w", file.Content.Path, file.GuestPath, err)
	}
	return resolvedPath, nil
}

func WriteHostFileAtomic(hostPath string, data []byte) error {
	dir := filepath.Dir(hostPath)
	mode := os.FileMode(0o644)
	if info, err := os.Stat(hostPath); err == nil {
		mode = info.Mode().Perm()
	}
	temp, err := os.CreateTemp(dir, ".virtie-writeback-*")
	if err != nil {
		return err
	}
	tempPath := temp.Name()
	cleanup := true
	defer func() {
		if cleanup {
			_ = os.Remove(tempPath)
		}
	}()

	if _, err := temp.Write(data); err != nil {
		_ = temp.Close()
		return err
	}
	if err := temp.Chmod(mode); err != nil {
		_ = temp.Close()
		return err
	}
	if err := temp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tempPath, hostPath); err != nil {
		return err
	}
	cleanup = false
	return nil
}

type GuestDirectoryInstaller struct {
	Exists  func(ctx context.Context, guestDir string) (bool, error)
	Install func(ctx context.Context, guestDir string, args []string) error
}

type GuestFileWriter struct {
	PathExists       func(ctx context.Context, guestPath string) (bool, error)
	InstallDirectory func(ctx context.Context, file manifest.ResolvedWriteFile) error
	WriteFile        func(ctx context.Context, guestPath string, payloadBase64 string) error
	Chown            func(ctx context.Context, guestPath string, owner string) error
	Chmod            func(ctx context.Context, guestPath string, mode string) error
	SkipExisting     func(guestPath string)
	Wrote            func(guestPath string)
}

type GuestFileWriteBacker struct {
	ReadFile      func(ctx context.Context, guestPath string) ([]byte, error)
	WriteHostFile func(hostPath string, data []byte) error
	Wrote         func(guestPath string, hostPath string)
}

type WorkspaceCWDMounter struct {
	InstallDir func(ctx context.Context, target string, args []string) error
	MountBind  func(ctx context.Context, source string, target string, args []string) error
	Mounted    func(source string, target string)
}

const workspaceCWDSource = "/mnt/cwd"

func WriteGuestFiles(ctx context.Context, files []manifest.ResolvedWriteFile, writer GuestFileWriter) error {
	for _, file := range files {
		if !file.Overwrite {
			exists, err := writer.PathExists(ctx, file.GuestPath)
			if err != nil {
				return wrapStage("guest file write", err)
			}
			if exists {
				if writer.SkipExisting != nil {
					writer.SkipExisting(file.GuestPath)
				}
				continue
			}
		}
		payloadBase64, err := guestFilePayloadBase64(file)
		if err != nil {
			return wrapStage("guest file write", err)
		}
		if err := writer.InstallDirectory(ctx, file); err != nil {
			return wrapStage("guest file write", err)
		}
		if err := writer.WriteFile(ctx, file.GuestPath, payloadBase64); err != nil {
			return wrapStage("guest file write", err)
		}
		if file.Chown != "" {
			if err := writer.Chown(ctx, file.GuestPath, file.Chown); err != nil {
				return wrapStage("guest file write", err)
			}
		}
		if file.Mode != "" {
			if err := writer.Chmod(ctx, file.GuestPath, file.Mode); err != nil {
				return wrapStage("guest file write", err)
			}
		}
		if writer.Wrote != nil {
			writer.Wrote(file.GuestPath)
		}
	}
	return nil
}

func MountWorkspaceCWD(ctx context.Context, launchManifest *manifest.Manifest, mounter WorkspaceCWDMounter) error {
	baseDir := launchManifest.Workspace.GuestDir
	if baseDir == "" {
		return wrapStage("workspace cwd mount", fmt.Errorf("workspace.guest_dir is required when workspace.mount_cwd is true"))
	}
	name := filepath.Base(launchManifest.Paths.WorkingDir)
	if name == "." || name == string(filepath.Separator) || name == "" {
		return wrapStage("workspace cwd mount", fmt.Errorf("derive workspace cwd name from working directory %q", launchManifest.Paths.WorkingDir))
	}
	target := path.Join(baseDir, name)
	if err := mounter.InstallDir(ctx, target, []string{"-d", baseDir, target}); err != nil {
		return wrapStage("workspace cwd mount", err)
	}
	if err := mounter.MountBind(ctx, workspaceCWDSource, target, []string{"--bind", workspaceCWDSource, target}); err != nil {
		return wrapStage("workspace cwd mount", err)
	}
	if mounter.Mounted != nil {
		mounter.Mounted(workspaceCWDSource, target)
	}
	return nil
}

func GuestWriteBackFiles(files []manifest.ResolvedWriteFile) []manifest.ResolvedWriteFile {
	writeBackFiles := make([]manifest.ResolvedWriteFile, 0, len(files))
	for _, file := range files {
		if file.WriteBack {
			writeBackFiles = append(writeBackFiles, file)
		}
	}
	return writeBackFiles
}

func WriteBackGuestFiles(ctx context.Context, files []manifest.ResolvedWriteFile, backer GuestFileWriteBacker) error {
	for _, file := range files {
		data, err := backer.ReadFile(ctx, file.GuestPath)
		if err != nil {
			return wrapStage("guest file write-back", err)
		}
		hostPath, err := writeBackHostPath(file)
		if err != nil {
			return wrapStage("guest file write-back", err)
		}
		if err := backer.WriteHostFile(hostPath, data); err != nil {
			return wrapStage("guest file write-back", fmt.Errorf("write host file %q from guest path %q: %w", hostPath, file.GuestPath, err))
		}
		if backer.Wrote != nil {
			backer.Wrote(file.GuestPath, hostPath)
		}
	}
	return nil
}

// InstallGuestFileDirectory ensures that the parent directory for guestPath exists.
// It walks upward until it finds an existing ancestor, then creates only the
// missing directories from top to bottom. owner and mode are applied to newly
// created directories only.
func InstallGuestFileDirectory(ctx context.Context, installer GuestDirectoryInstaller, guestPath string, owner string, mode string) error {
	guestDir := path.Clean(path.Dir(guestPath))
	if guestDir == "." || guestDir == "/" {
		return nil
	}

	missingDirs := make([]string, 0, 4)
	current := guestDir
	for {
		exists, err := installer.Exists(ctx, current)
		if err != nil {
			return err
		}
		if exists {
			break
		}
		missingDirs = append(missingDirs, current)
		parent := path.Dir(current)
		if parent == current {
			return fmt.Errorf("resolve existing parent for %q", guestDir)
		}
		current = parent
	}

	for i := len(missingDirs) - 1; i >= 0; i-- {
		dir := missingDirs[i]
		if err := installer.Install(ctx, dir, guestInstallDirectoryArgs(dir, owner, mode)); err != nil {
			return err
		}
	}
	return nil
}

func guestInstallDirectoryArgs(guestDir string, owner string, mode string) []string {
	args := []string{"-d"}
	if owner != "" {
		user, group, _ := strings.Cut(owner, ":")
		if user != "" {
			args = append(args, "-o", user)
		}
		if group != "" {
			args = append(args, "-g", group)
		}
	}
	if mode != "" {
		args = append(args, "-m", guestDirectoryMode(mode))
	}
	return append(args, guestDir)
}

func guestDirectoryMode(mode string) string {
	prefix := ""
	digits := mode
	if strings.HasPrefix(mode, "0") {
		prefix = "0"
		digits = mode[1:]
	}
	if len(digits) != 3 {
		return mode
	}

	out := make([]byte, 3)
	for i := 0; i < 3; i++ {
		d := digits[i] - '0'
		if d&0b100 != 0 {
			d |= 0b001
		}
		out[i] = '0' + d
	}
	return prefix + string(out)
}
