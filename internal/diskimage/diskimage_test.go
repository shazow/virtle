package diskimage

import (
	"encoding/binary"
	"os"
	"path/filepath"
	"testing"
)

const testSize = 256 << 20

// ext4 keeps its superblock 1024 bytes in; the magic sits at offset 0x38.
func ext4Magic(t *testing.T, path string) uint16 {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return binary.LittleEndian.Uint16(data[1024+0x38:])
}

func TestEnsureCreatesAnExt4ImageOnce(t *testing.T) {
	path := filepath.Join(t.TempDir(), "scratch.img")
	created, err := Ensure(Image{Path: path, Size: testSize, Label: "scratch"})
	if err != nil || !created {
		t.Fatalf("Ensure = %v, %v; want created", created, err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Size() != testSize || info.Mode().Perm() != 0o600 {
		t.Fatalf("image size %d mode %v, want %d bytes and 0600", info.Size(), info.Mode().Perm(), testSize)
	}
	if got := ext4Magic(t, path); got != 0xEF53 {
		t.Fatalf("ext4 magic = %#x, want 0xEF53", got)
	}
	if created, err := Ensure(Image{Path: path, Size: testSize}); err != nil || created {
		t.Fatalf("second Ensure = %v, %v; want the existing image kept", created, err)
	}
}

func TestEnsureRejectsADirectory(t *testing.T) {
	dir := t.TempDir()
	if _, err := Ensure(Image{Path: dir, Size: testSize}); err == nil {
		t.Fatal("a directory was accepted as a disk image")
	}
}

func TestCreateLeavesNothingBehindOnFailure(t *testing.T) {
	path := filepath.Join(t.TempDir(), "scratch.img")
	// An unknown owner fails after the file is created.
	if err := Create(Image{Path: path, Size: testSize, Owner: "no-such-virtle-user"}); err == nil {
		t.Fatal("unknown owner accepted")
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("failed creation left %q behind: %v", path, err)
	}
}
