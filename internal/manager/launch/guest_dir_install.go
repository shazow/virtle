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
	// directories unchanged. owner is "user", "user:group", ":group", or
	// empty; mode is the directory mode to apply (see guestDirectoryMode) or
	// empty.
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
// mode. It walks upward to the existing ancestor, then creates only the
// missing directories from top to bottom, applying owner and mode to each
// newly created directory; existing directories are left unchanged. Pure
// POSIX sh (verified under dash).
const guestDirectoryInstallScript = `set -e
d=$1
owner=$2
mode=$3
case $owner in
  *:*) user=${owner%%:*}; group=${owner#*:} ;;
  *) user=$owner; group= ;;
esac
anc=$d
while [ ! -d "$anc" ]; do
  case $anc in
    */*) anc=${anc%/*} ;;
    *) exit 1 ;;
  esac
done
cur=$anc
rest=${d#"$anc"}
while [ -n "$rest" ]; do
  rest=${rest#/}
  comp=${rest%%/*}
  cur=$cur/$comp
  set -- -d
  [ -n "$user" ] && set -- "$@" -o "$user"
  [ -n "$group" ] && set -- "$@" -g "$group"
  [ -n "$mode" ] && set -- "$@" -m "$mode"
  install "$@" "$cur"
  rest=${rest#"$comp"}
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
