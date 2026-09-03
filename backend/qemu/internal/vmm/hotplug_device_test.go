package vmm

import (
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/shazow/virtle/internal/manifest"
	"github.com/shazow/virtle/vm"
)

func TestAdHocHotplugDevicesReceiveExecutablePlansAndDefaults(t *testing.T) {
	tmpDir := t.TempDir()
	resolver := &manifest.Manifest{
		Paths: manifest.Paths{
			WorkingDir: tmpDir,
			RuntimeDir: manifest.RuntimeDir{Mode: manifest.RuntimeDirPath, Path: filepath.Join(tmpDir, "state")},
		},
	}
	share, err := hotplugDeviceFor(resolver, vm.Share{Tag: "data", HostPath: "/host/data", GuestPath: "/data"})
	if err != nil {
		t.Fatalf("share: %v", err)
	}
	if share.ID != "data" || share.VirtioFS.Source != "/host/data" || share.VirtioFS.Target != "/data" || share.VirtioFS.Bin != "virtiofsd" {
		t.Errorf("share device = %+v", share)
	}
	if got, want := share.VirtioFS.SocketPath, filepath.Join(tmpDir, "state", "data.sock"); got != want {
		t.Errorf("share socket = %q, want %q", got, want)
	}
	if got, want := share.VirtioFS.Args, manifest.DefaultVirtioFSArgs(share.VirtioFS.SocketPath, "/host/data", "data"); !reflect.DeepEqual(got, want) {
		t.Errorf("share helper args = %#v, want %#v", got, want)
	}

	disk, err := hotplugDeviceFor(resolver, vm.Disk{Path: "/imgs/scratch.img"})
	if err != nil {
		t.Fatalf("disk: %v", err)
	}
	if disk.Block.ImagePath != "/imgs/scratch.img" || disk.Block.Format != "raw" {
		t.Errorf("disk device = %+v", disk)
	}
	if strings.ContainsAny(disk.ID, "/ ") {
		t.Errorf("disk ID %q contains unsafe characters", disk.ID)
	}
	couldCollide, err := hotplugDeviceFor(resolver, vm.Disk{Path: "/imgs/scratch_img"})
	if err != nil {
		t.Fatalf("second disk: %v", err)
	}
	if disk.ID == couldCollide.ID {
		t.Errorf("distinct disk paths produced the same ID %q", disk.ID)
	}

	fwd, err := hotplugDeviceFor(resolver, vm.Forward{HostAddr: "127.0.0.1:8080", GuestAddr: ":80"})
	if err != nil {
		t.Fatalf("forward: %v", err)
	}
	if fwd.Net.Backend != "user" || fwd.Net.MAC == "" || len(fwd.Net.Forward) != 1 || fwd.Net.Forward[0].Proto != "tcp" || fwd.Net.Forward[0].Host != "127.0.0.1:8080" || fwd.Net.Forward[0].Guest != ":80" {
		t.Errorf("forward device = %+v", fwd)
	}
}
