package vmtest

import (
	"bytes"
	"context"
	"errors"
	"io"
	"io/fs"
	"os/exec"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/shazow/virtle/vm"
)

func TestGuestRunScriptedResults(t *testing.T) {
	g := &Guest{Commands: map[string]Result{
		"make": {Stdout: []byte("built\n")},
		"fail": {ExitCode: 2, Stderr: []byte("boom")},
	}}
	ctx := context.Background()

	out, err := vm.Output(ctx, g, &vm.GuestCmd{Path: "make", Args: []string{"-j2"}, Dir: "/src"})
	if err != nil {
		t.Fatalf("Output: %v", err)
	}
	if string(out) != "built\n" {
		t.Errorf("Output = %q", out)
	}

	var exit *vm.ExitError
	if _, err := vm.Output(ctx, g, &vm.GuestCmd{Path: "fail"}); !errors.As(err, &exit) || exit.Code != 2 || string(exit.Stderr) != "boom" {
		t.Errorf("Output(fail) error = %v, want ExitError{2, boom}", err)
	}
	var stderr bytes.Buffer
	if err := g.Run(ctx, &vm.GuestCmd{Path: "fail", Stderr: &stderr, Stdin: strings.NewReader("in")}); !errors.As(err, &exit) || exit.Stderr != nil {
		t.Errorf("Run(fail) with Stderr = %v, want ExitError without stderr", err)
	}
	if stderr.String() != "boom" {
		t.Errorf("stderr = %q", stderr.String())
	}

	if err := g.Run(ctx, &vm.GuestCmd{Path: "missing"}); !errors.Is(err, exec.ErrNotFound) {
		t.Errorf("Run(missing) = %v, want exec.ErrNotFound", err)
	}

	got := g.CommandsRun()
	if len(got) != 4 || got[0].Path != "make" || got[0].Args[0] != "-j2" || got[0].Dir != "/src" || got[3].Path != "missing" {
		t.Errorf("CommandsRun = %+v", got)
	}
}

func TestGuestFiles(t *testing.T) {
	g := &Guest{FS: fstest.MapFS{"etc/hostname": {Data: []byte("box\n")}}}
	ctx := context.Background()

	r, err := g.Open(ctx, "/etc/hostname")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	data, err := io.ReadAll(r)
	if err != nil || string(data) != "box\n" {
		t.Errorf("read = %q, %v", data, err)
	}
	r.Close()

	w, err := g.Create(ctx, "/etc/motd", 0o640)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	io.WriteString(w, "hello")
	if err := w.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	info, err := fs.Stat(g.FS, "etc/motd")
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if info.Mode().Perm() != 0o640 {
		t.Errorf("mode = %v, want 0640", info.Mode())
	}
	r, err = g.Open(ctx, "etc/motd")
	if err != nil {
		t.Fatalf("Open written file: %v", err)
	}
	if data, _ := io.ReadAll(r); string(data) != "hello" {
		t.Errorf("written content = %q", data)
	}

	if _, err := g.Open(ctx, "/nope"); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("Open(missing) = %v, want fs.ErrNotExist", err)
	}
}

func TestGuestZeroValue(t *testing.T) {
	var g Guest
	ctx := context.Background()
	if _, err := g.Open(ctx, "/x"); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("Open on zero Guest = %v", err)
	}
	w, err := g.Create(ctx, "/x", 0o644)
	if err != nil {
		t.Fatalf("Create on zero Guest: %v", err)
	}
	w.Close()
	if _, err := g.Open(ctx, "/x"); err != nil {
		t.Errorf("Open after Create = %v", err)
	}
	if err := g.Shutdown(ctx); err != nil || g.Shutdowns() != 1 {
		t.Errorf("Shutdown = %v, count %d", err, g.Shutdowns())
	}
	if err := g.Close(); err != nil || g.Closes() != 1 {
		t.Errorf("Close = %v, count %d", err, g.Closes())
	}
}
