package vm

import (
	"archive/tar"
	"context"
	"errors"
	"io"
	"io/fs"
	"testing"
	"testing/fstest"
)

// echoGuest is a Guest whose Run writes the command's Args to Stdout and
// Stderr and exits with the code given as Dir, exercising Output.
type echoGuest struct {
	Guest
	lastStdout io.Writer
}

func (g *echoGuest) Run(ctx context.Context, cmd *GuestCmd) error {
	g.lastStdout = cmd.Stdout
	for _, w := range []io.Writer{cmd.Stdout, cmd.Stderr} {
		if w != nil {
			io.WriteString(w, cmd.Path)
		}
	}
	if len(cmd.Args) > 0 {
		exit := &ExitError{Code: len(cmd.Args)}
		if cmd.Stderr == nil {
			exit.Stderr = []byte(cmd.Path)
		}
		return exit
	}
	return nil
}

func TestOutput(t *testing.T) {
	g := &echoGuest{}
	out, err := Output(context.Background(), g, &GuestCmd{Path: "hello"})
	if err != nil {
		t.Fatalf("Output: %v", err)
	}
	if string(out) != "hello" {
		t.Errorf("Output = %q, want %q", out, "hello")
	}

	cmd := &GuestCmd{Path: "fail", Args: []string{"x", "y"}}
	out, err = Output(context.Background(), g, cmd)
	var exit *ExitError
	if !errors.As(err, &exit) || exit.Code != 2 || string(exit.Stderr) != "fail" {
		t.Fatalf("Output error = %v, want ExitError{Code: 2, Stderr: fail}", err)
	}
	if string(out) != "fail" {
		t.Errorf("Output on failure = %q, want stdout preserved", out)
	}
	if cmd.Stdout != nil {
		t.Error("Output modified the caller's command")
	}
	if want := "exit status 2: stderr=\"fail\""; exit.Error() != want {
		t.Errorf("ExitError.Error() = %q, want %q", exit.Error(), want)
	}
}

func TestOutputRejectsStdout(t *testing.T) {
	if _, err := Output(context.Background(), &echoGuest{}, &GuestCmd{Path: "x", Stdout: io.Discard}); err == nil {
		t.Fatal("expected Output to reject a command with Stdout set")
	}
}

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
