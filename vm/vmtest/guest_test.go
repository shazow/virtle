package vmtest

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/shazow/virtle/vm"
)

func TestGuestRunAndFiles(t *testing.T) {
	g := &Guest{
		FS: fstest.MapFS{"etc/issue": {Data: []byte("hello")}},
		Commands: map[string]Result{
			"make": {Stdout: []byte("building\n"), Stderr: []byte("failed\n"), ExitCode: 2},
		},
	}

	stdout, err := vm.Output(context.Background(), g, &vm.GuestCmd{Path: "make"})
	if string(stdout) != "building\n" {
		t.Fatalf("stdout = %q", stdout)
	}
	var exitErr *vm.ExitError
	if !errors.As(err, &exitErr) || exitErr.Code != 2 || string(exitErr.Stderr) != "failed\n" {
		t.Fatalf("error = %#v", err)
	}

	r, err := g.Open(context.Background(), "/etc/issue")
	if err != nil {
		t.Fatal(err)
	}
	data, err := io.ReadAll(r)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "hello" {
		t.Fatalf("file = %q", data)
	}

	w, err := g.Create(context.Background(), "/tmp/output", 0o640)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.Copy(w, strings.NewReader("result")); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	if got := string(g.FS["tmp/output"].Data); got != "result" {
		t.Fatalf("created file = %q", got)
	}
}
