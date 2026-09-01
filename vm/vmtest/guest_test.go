package vmtest

import (
	"context"
	"errors"
	"io"
	"testing"
	"testing/fstest"

	"github.com/shazow/virtle/vm"
)

func TestGuestCommandsAndFiles(t *testing.T) {
	g := &Guest{
		FS:       fstest.MapFS{"hello": {Data: []byte("world")}},
		Commands: map[string]Result{"false": {Code: 3, Stderr: []byte("failed")}},
	}
	err := g.Run(context.Background(), &vm.GuestCmd{Path: "false"})
	var exitErr *vm.ExitError
	if !errors.As(err, &exitErr) || exitErr.Code != 3 || string(exitErr.Stderr) != "failed" {
		t.Fatalf("Run error = %#v", err)
	}
	r, err := g.Open(context.Background(), "hello")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	data, err := io.ReadAll(r)
	if err != nil || string(data) != "world" {
		t.Fatalf("ReadAll = %q, %v", data, err)
	}
	w, err := g.Create(context.Background(), "new", 0o600)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	_, _ = io.WriteString(w, "data")
	if err := w.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if got := string(g.FS["new"].Data); got != "data" {
		t.Fatalf("created data = %q", got)
	}
}
