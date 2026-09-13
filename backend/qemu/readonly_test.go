package qemu

import (
	"testing"

	imanifest "github.com/shazow/virtle/internal/manifest"
	"github.com/shazow/virtle/vm"
)

func TestDiskReadOnly(t *testing.T) {
	disk, err := overlayDisk(imanifest.ImageMountInput{}, vm.Disk{Path: "disk", ReadOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	if !disk.ReadOnly {
		t.Fatal("read-only disk became writable")
	}
}
