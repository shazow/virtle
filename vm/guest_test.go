package vm

import (
	"archive/tar"
	"errors"
	"io"
	"io/fs"
	"testing"
	"testing/fstest"
)

func TestArchiveFS(t *testing.T) {
	fsys := fstest.MapFS{
		"hello.txt":     {Data: []byte("hello"), Mode: 0o644},
		"dir/world.txt": {Data: []byte("world"), Mode: 0o600},
	}

	tr := tar.NewReader(ArchiveFS(fsys))
	got := map[string]string{}
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("reading archive: %v", err)
		}
		if hdr.Typeflag != tar.TypeReg {
			continue
		}
		data, err := io.ReadAll(tr)
		if err != nil {
			t.Fatalf("reading %q: %v", hdr.Name, err)
		}
		got[hdr.Name] = string(data)
	}

	want := map[string]string{"hello.txt": "hello", "dir/world.txt": "world"}
	for name, content := range want {
		if got[name] != content {
			t.Errorf("archive entry %q = %q, want %q", name, got[name], content)
		}
	}
}

func TestArchiveFSPropagatesErrors(t *testing.T) {
	fsys := fstest.MapFS{"ok.txt": {Data: []byte("ok")}}
	rc := ArchiveFS(brokenFS{fsys})
	defer rc.Close()
	if _, err := io.ReadAll(rc); err == nil {
		t.Fatal("expected generation error to surface from Read")
	}
}

// brokenFS fails all Open calls to exercise error propagation.
type brokenFS struct{ fstest.MapFS }

func (b brokenFS) Open(name string) (fs.File, error) {
	return nil, errors.New("boom")
}
