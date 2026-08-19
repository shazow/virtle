package main

import (
	"fmt"
	"io"
	"runtime/debug"
)

// version is a build-time override for the reported version, set with
// -ldflags "-X main.version=...". Only the Nix package needs it, because it
// builds from a source copy with no VCS metadata to stamp; release archives
// and `go install` builds leave it empty and rely on the version the Go
// toolchain records from the semver tag it was built from.
var version = ""

// versionString describes the running binary as precisely as its build allows.
func versionString() string {
	info, ok := debug.ReadBuildInfo()
	if !ok {
		info = nil
	}
	return buildVersion(version, info)
}

func buildVersion(stamped string, info *debug.BuildInfo) string {
	if stamped != "" {
		return stamped
	}
	if info == nil {
		return "unknown"
	}
	// A build with no VCS stamps, such as the Nix package built from a source
	// copy without .git, leaves the module version as the "(devel)" placeholder.
	if moduleVersion := info.Main.Version; moduleVersion != "" && moduleVersion != "(devel)" {
		return moduleVersion
	}
	return "devel"
}

func printVersion(w io.Writer) error {
	_, err := fmt.Fprintln(w, "virtle "+versionString())
	return err
}
