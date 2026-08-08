package launch

import (
	"context"
	"fmt"
	"path"
	"strings"
)

// GuestDirectoryInstallScript returns a POSIX sh script that ensures guestDir
// and all of its missing ancestors exist in a single command. The script takes
// three positional arguments: the target directory, an optional "user" or
// "user:group" owner, and an optional directory mode. It walks upward to the
// existing ancestor, then creates only the missing directories from top to
// bottom, applying owner and mode to each newly created directory only;
// existing directories are left unchanged.
func GuestDirectoryInstallScript() string {
	return guestDirectoryInstallScript
}

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

// GuestDirectoryInstaller installs the parent directory tree for a guest
// file in a single guest command.
type GuestDirectoryInstaller struct {
	// InstallTree creates guestDir and any missing ancestors, applying owner
	// and mode to each newly created directory only and leaving existing
	// directories unchanged. owner is "user" or "user:group"; mode is the
	// directory mode to apply (see guestDirectoryMode) or empty.
	InstallTree func(ctx context.Context, guestDir, owner, mode string) error
}

// InstallGuestFileDirectory ensures that the parent directory for guestPath
// exists. It runs one scripted guest command that walks upward to the
// existing ancestor and creates only the missing directories from top to
// bottom. owner and mode are applied to newly created directories only;
// existing directories are left unchanged. mode is expected to be a file mode
// and is converted to a directory mode by adding execute bits wherever read
// bits are set.
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
