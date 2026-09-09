// Package diskimage creates the raw disk images virtle attaches to guests,
// for every backend: an empty ext4 filesystem in a sparse file that is
// private to the creating user, or handed to the account a privilege-dropped
// VMM runs as.
package diskimage

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/user"
	"strconv"

	backendfile "github.com/diskfs/go-diskfs/backend/file"
	"github.com/diskfs/go-diskfs/filesystem/ext4"
)

const privateFileMode os.FileMode = 0o600

// Image describes a raw ext4 image to create.
type Image struct {
	Path  string
	Size  int64  // bytes
	Label string // ext4 volume label; empty leaves the image unlabeled
	Owner string // host account that owns the file; empty keeps the caller
}

// Create formats a new ext4 image at img.Path. The path must not exist yet;
// a failure leaves nothing behind.
func Create(img Image) error {
	file, err := os.OpenFile(img.Path, os.O_RDWR|os.O_CREATE|os.O_EXCL, privateFileMode)
	if err != nil {
		return fmt.Errorf("create disk image %q: %w", img.Path, err)
	}
	created := false
	defer func() {
		if !created {
			_ = os.Remove(img.Path)
		}
	}()
	if err := file.Chmod(privateFileMode); err != nil {
		_ = file.Close()
		return fmt.Errorf("create disk image %q: %w", img.Path, err)
	}
	if img.Owner != "" {
		uid, gid, err := lookupOwner(img.Owner)
		if err != nil {
			_ = file.Close()
			return err
		}
		if err := file.Chown(uid, gid); err != nil {
			_ = file.Close()
			return fmt.Errorf("assign disk image %q to %q: %w", img.Path, img.Owner, err)
		}
	}
	if err := file.Close(); err != nil {
		return fmt.Errorf("close disk image %q: %w", img.Path, err)
	}

	// Copy-on-write filesystems fragment disk images badly; opt out where
	// chattr exists and ignore filesystems that do not support it.
	if chattr, err := exec.LookPath("chattr"); err == nil {
		_ = exec.Command(chattr, "+C", img.Path).Run()
	}
	if err := os.Truncate(img.Path, img.Size); err != nil {
		return fmt.Errorf("size disk image %q: %w", img.Path, err)
	}

	backing, err := backendfile.OpenFromPath(img.Path, false)
	if err != nil {
		return fmt.Errorf("open disk image %q: %w", img.Path, err)
	}
	defer backing.Close()
	params := &ext4.Params{SectorsPerBlock: 8}
	if img.Label != "" {
		params.VolumeName = img.Label
	}
	fs, err := ext4.Create(backing, img.Size, 0, int64(ext4.SectorSize512), params)
	if err != nil {
		return fmt.Errorf("format ext4 disk image %q: %w", img.Path, err)
	}
	if img.Label == "" {
		if err := fs.SetLabel(""); err != nil {
			return fmt.Errorf("clear default ext4 label of %q: %w", img.Path, err)
		}
	}
	created = true
	return nil
}

// Ensure creates the image when nothing exists at img.Path and reports
// whether it did. An existing file is used as it is; a directory is an
// error.
func Ensure(img Image) (bool, error) {
	info, err := os.Stat(img.Path)
	switch {
	case err == nil:
		if info.IsDir() {
			return false, fmt.Errorf("disk image %q is a directory", img.Path)
		}
		return false, nil
	case !errors.Is(err, os.ErrNotExist):
		return false, fmt.Errorf("stat disk image %q: %w", img.Path, err)
	}
	if err := Create(img); err != nil {
		return false, err
	}
	return true, nil
}

func lookupOwner(name string) (int, int, error) {
	account, err := user.Lookup(name)
	if err != nil {
		return 0, 0, fmt.Errorf("look up disk image owner %q: %w", name, err)
	}
	uid, err := strconv.Atoi(account.Uid)
	if err != nil {
		return 0, 0, fmt.Errorf("parse uid %q of %q: %w", account.Uid, name, err)
	}
	gid, err := strconv.Atoi(account.Gid)
	if err != nil {
		return 0, 0, fmt.Errorf("parse gid %q of %q: %w", account.Gid, name, err)
	}
	return uid, gid, nil
}
