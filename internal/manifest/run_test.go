package manifest

import (
	"reflect"
	"strings"
	"testing"
)

func TestRunTemplatesAcrossBackends(t *testing.T) {
	for _, backend := range []string{BackendQEMU, BackendFirecracker, BackendCloudHypervisor} {
		t.Run(backend, func(t *testing.T) {
			doc := seededDocument()
			doc.Backend = backend
			doc.WorkingDir = "/work"
			doc.Kernel.Path = "kernel"
			doc.Workspace = WorkspaceInput{GuestDir: "/workspace", HostDir: "/source"}
			doc.Run = []RunInput{{
				Exec: []string{"helper", "{{.StateDir}}", "{{with .Workspace}}{{.GuestPath}}:{{.HostPath}}{{end}}", "{{.Label}}"},
				Vars: map[string]any{"Label": "test"},
			}}
			mf, err := doc.Manifest()
			if err != nil {
				t.Fatal(err)
			}
			runs, err := mf.ResolvedRuns(0)
			if err != nil {
				t.Fatal(err)
			}
			want := []string{"helper", "/work/.virtle", "/workspace:/source", "test"}
			if len(runs) != 1 || !reflect.DeepEqual(runs[0].Exec, want) || runs[0].Dir != "/work" {
				t.Fatalf("runs = %+v, want command %q in /work", runs, want)
			}
		})
	}
}

func TestVirtioFSHelperTemplateValidation(t *testing.T) {
	for _, backend := range []string{BackendQEMU, BackendCloudHypervisor} {
		t.Run(backend, func(t *testing.T) {
			doc := seededDocument()
			doc.Backend = backend
			doc.WorkingDir = "/work"
			doc.Kernel.Path = "kernel"
			doc.Mounts = MountsInput{VirtioFSMountInput{
				MountInput: MountInput{Tag: "src", SourcePath: "/source"},
				VirtioFS:   VirtioFSInput{Args: []string{"--socket-path={{.Socket"}},
			}}
			if _, err := doc.Manifest(); err == nil || !strings.Contains(err.Error(), "manifest.run[0].exec[1]") {
				t.Fatalf("malformed daemon template: %v", err)
			}
		})
	}
}
