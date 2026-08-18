package launch

import (
	"context"
	"fmt"
	"path"
	"strings"
)

// GuestDirectoryInstaller installs a directory tree inside the guest. It is
// the seam between the host-side install policy in InstallGuestFileDirectory
// and whatever guest-side mechanism realizes it; ScriptGuestDirectoryInstaller
// is the current mechanism.
type GuestDirectoryInstaller struct {
	// InstallTree creates guestDir and any missing ancestors, applying owner
	// and mode to each newly created directory only and leaving existing
	// directories unchanged; directories created concurrently by other guest
	// processes count as existing. owner is "user", "user:group", ":group",
	// or empty; mode is the directory mode to apply (see guestDirectoryMode)
	// or empty.
	InstallTree func(ctx context.Context, guestDir string, owner string, mode string) error
}

// GuestCommandRunner runs a program inside the guest. subject names what the
// command operates on for error reporting; path is the program to execute and
// args are its arguments.
type GuestCommandRunner func(ctx context.Context, subject string, path string, args []string) error

// ScriptGuestDirectoryInstaller returns a GuestDirectoryInstaller that
// realizes InstallTree as a single POSIX sh script executed through run. The
// script and its invocation are implementation details of this constructor:
// nothing outside it depends on them, so the installer can be swapped for a
// different mechanism (such as a guest-side daemon) without touching call
// sites.
func ScriptGuestDirectoryInstaller(run GuestCommandRunner) GuestDirectoryInstaller {
	return GuestDirectoryInstaller{
		InstallTree: func(ctx context.Context, guestDir string, owner string, mode string) error {
			return run(ctx, guestDir, "sh", []string{"-c", guestDirectoryInstallScript, "sh", guestDir, owner, mode})
		},
	}
}

// guestDirectoryInstallScript ensures the directory $1 and all of its missing
// ancestors exist in one guest command. It takes the target directory, an
// optional "user", "user:group", or ":group" owner, and an optional directory
// mode. It walks the target path from the top, creating only the missing
// directories and applying owner and mode to each newly created directory;
// existing directories are left unchanged. mkdir is the atomic creation
// decision: a directory that appears concurrently between the -d probe and
// mkdir is treated as existing and left untouched, so owner and mode are only
// ever applied to directories this script created. Pure POSIX sh (verified
// under dash).
const guestDirectoryInstallScript = `set -eu
dir=$1
owner=$2
mode=$3
case $dir in
  /*) cur= ;;
  *) cur=. ;;
esac
rest=${dir#/}
while [ -n "$rest" ]; do
  comp=${rest%%/*}
  case $rest in
    */*) rest=${rest#*/} ;;
    *) rest= ;;
  esac
  if [ -z "$comp" ]; then
    continue
  fi
  cur=$cur/$comp
  if [ -d "$cur" ]; then
    continue
  fi
  set --
  if [ -n "$mode" ]; then
    set -- -m "$mode"
  fi
  if mkdir "$@" "$cur"; then
    if [ -n "$owner" ]; then
      chown "$owner" "$cur"
    fi
  else
    [ -d "$cur" ]
  fi
done
`

// InstallGuestFileDirectory ensures that the parent directory for guestPath
// exists, creating any missing directories through installer. owner and mode
// apply to newly created directories only; existing directories are left
// unchanged. mode is expected to be a file mode and is converted to a
// directory mode by adding execute bits wherever read bits are set.
func InstallGuestFileDirectory(ctx context.Context, installer GuestDirectoryInstaller, guestPath string, owner string, mode string) error {
	guestDir := path.Clean(path.Dir(guestPath))
	if guestDir == "." || guestDir == "/" {
		return nil
	}
	if installer.InstallTree == nil {
		return fmt.Errorf("guest directory installer is not configured")
	}
	dirMode := ""
	if mode != "" {
		dirMode = guestDirectoryMode(mode)
	}
	return installer.InstallTree(ctx, guestDir, owner, dirMode)
}

// guestDirectoryMode converts a file mode to a directory mode by adding
// execute bits wherever read bits are set. Malformed modes are passed through
// unchanged.
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
