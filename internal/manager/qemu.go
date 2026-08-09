package manager

import (
	"os/exec"

	"github.com/shazow/virtle/internal/manifest"
	"github.com/shazow/virtle/internal/qemuargs"
)

// buildQEMUCommand renders the QEMU invocation for a resolved manifest. The
// argv construction itself lives in internal/qemuargs, shared with the qemu
// backend.
func buildQEMUCommand(manifest *manifest.Manifest, cid int, incoming bool) (*exec.Cmd, error) {
	return qemuargs.BuildCommand(manifest, cid, incoming)
}
