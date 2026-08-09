package vm_test

import (
	"archive/tar"
	"errors"
	"io"
	"io/fs"
	"testing"
	"testing/fstest"

	"github.com/shazow/virtle/vm"
)

func TestArchiveFSProducesTarStream(t *testing.T) {
	fsys := fstest.MapFS{
		"main.go":     &fstest.MapFile{Data: []byte("package main\n"), Mode: 0o644},
		"docs/api.md": &fstest.MapFile{Data: []byte("# api\n"), Mode: 0o644},
	}

	stream := vm.ArchiveFS(fsys)
	defer stream.Close()

	contents := map[string]string{}
	reader := tar.NewReader(stream)
	for {
		header, err := reader.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("read archive: %v", err)
		}
		if header.Typeflag != tar.TypeReg {
			continue
		}
		data, err := io.ReadAll(reader)
		if err != nil {
			t.Fatalf("read %q: %v", header.Name, err)
		}
		contents[header.Name] = string(data)
	}

	for name, want := range map[string]string{
		"main.go":     "package main\n",
		"docs/api.md": "# api\n",
	} {
		if got := contents[name]; got != want {
			t.Errorf("archive entry %q = %q, want %q", name, got, want)
		}
	}
}

func TestArchiveFSReportsGenerationErrorFromRead(t *testing.T) {
	stream := vm.ArchiveFS(errFS{err: fs.ErrPermission})
	defer stream.Close()

	if _, err := io.ReadAll(stream); !errors.Is(err, fs.ErrPermission) {
		t.Fatalf("ReadAll() error = %v, want fs.ErrPermission", err)
	}
}

// errFS fails every walk, standing in for an unreadable source directory.
type errFS struct{ err error }

func (f errFS) Open(name string) (fs.File, error) {
	return nil, &fs.PathError{Op: "open", Path: name, Err: f.err}
}
