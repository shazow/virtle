// Package vmtest provides in-memory doubles for vm package contracts.
package vmtest

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"sync"
	"testing/fstest"

	"github.com/shazow/virtle/vm"
)

// Result scripts one command result.
type Result struct {
	Stdout, Stderr []byte
	Code           int
	Err            error
}

// Guest is an in-memory vm.Guest backed by MapFS and scripted commands. The
// zero value is ready to use.
type Guest struct {
	// FS backs Open and receives files written through Create when the
	// writer is closed. It is created on first write when nil.
	FS fstest.MapFS
	// Commands scripts Run results by GuestCmd.Path; Args, Env, Dir, and
	// Stdin are ignored. An unscripted path returns an error wrapping
	// errors.ErrUnsupported.
	Commands map[string]Result

	// ShutdownErr is returned by every Shutdown call after it is counted.
	ShutdownErr error
	mu          sync.Mutex
	shutdowns   int
}

func (g *Guest) Run(ctx context.Context, cmd *vm.GuestCmd) error {
	if err := context.Cause(ctx); err != nil {
		return err
	}
	if cmd == nil || cmd.Path == "" {
		return fmt.Errorf("guest command path is required")
	}
	result, ok := g.Commands[cmd.Path]
	if !ok {
		return fmt.Errorf("guest command %q: %w", cmd.Path, errors.ErrUnsupported)
	}
	if cmd.Stdout != nil {
		if _, err := cmd.Stdout.Write(result.Stdout); err != nil {
			return err
		}
	}
	if cmd.Stderr != nil {
		if _, err := cmd.Stderr.Write(result.Stderr); err != nil {
			return err
		}
	}
	if result.Err != nil {
		return result.Err
	}
	if result.Code != 0 {
		exitErr := &vm.ExitError{Code: result.Code}
		if cmd.Stderr == nil {
			exitErr.Stderr = append([]byte(nil), result.Stderr...)
		}
		return exitErr
	}
	return nil
}

func (g *Guest) Open(ctx context.Context, name string) (io.ReadCloser, error) {
	if err := context.Cause(ctx); err != nil {
		return nil, err
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	data, err := fs.ReadFile(g.FS, name)
	if err != nil {
		return nil, err
	}
	return io.NopCloser(bytes.NewReader(data)), nil
}

func (g *Guest) Create(ctx context.Context, name string, mode fs.FileMode) (io.WriteCloser, error) {
	if err := context.Cause(ctx); err != nil {
		return nil, err
	}
	return &writer{ctx: ctx, guest: g, name: name, mode: mode}, nil
}

type writer struct {
	bytes.Buffer
	ctx   context.Context
	guest *Guest
	name  string
	mode  fs.FileMode
}

func (w *writer) Close() error {
	if err := context.Cause(w.ctx); err != nil {
		return err
	}
	w.guest.mu.Lock()
	defer w.guest.mu.Unlock()
	if w.guest.FS == nil {
		w.guest.FS = fstest.MapFS{}
	}
	w.guest.FS[w.name] = &fstest.MapFile{Data: append([]byte(nil), w.Bytes()...), Mode: w.mode}
	return nil
}

func (g *Guest) Shutdown(ctx context.Context) error {
	if err := context.Cause(ctx); err != nil {
		return err
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	g.shutdowns++
	return g.ShutdownErr
}

// Shutdowns reports how many shutdown requests the guest received.
func (g *Guest) Shutdowns() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.shutdowns
}

func (*Guest) Close() error { return nil }

var _ vm.Guest = (*Guest)(nil)
