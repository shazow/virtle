// Package vmtest provides an in-memory vm.Guest for testing code that
// operates on guests without a virtual machine — the fstest.MapFS of
// vm.Guest. Guest serves files from a MapFS and scripted command results,
// and records what was asked of it.
//
//	g := &vmtest.Guest{
//		FS:       fstest.MapFS{"etc/hostname": {Data: []byte("box\n")}},
//		Commands: map[string]vmtest.Result{"make": {Stdout: []byte("ok\n")}},
//	}
//	err := deploy(ctx, g) // code under test takes a vm.Guest
package vmtest

import (
	"context"
	"fmt"
	"io"
	"io/fs"
	"os/exec"
	"strings"
	"sync"
	"testing/fstest"
	"time"

	"github.com/shazow/virtle/vm"
)

// Result scripts the outcome of a guest command.
type Result struct {
	ExitCode       int
	Stdout, Stderr []byte
}

// Guest is an in-memory vm.Guest. The zero value is usable: every command
// is unknown and the filesystem is empty. Guest is safe for concurrent
// use, as vm.Guest requires.
type Guest struct {
	// FS backs Open and Create. Files written through Create appear in FS
	// once the writer is closed. Nil is treated as empty.
	FS fstest.MapFS

	// Commands scripts Run by GuestCmd.Path. A command not listed here
	// fails with an error wrapping exec.ErrNotFound.
	Commands map[string]Result

	mu        sync.Mutex
	commands  []vm.GuestCmd
	shutdowns int
	closes    int
}

var _ vm.Guest = (*Guest)(nil)

// Run delivers the scripted Result for cmd.Path to cmd.Stdout and
// cmd.Stderr, returning a *vm.ExitError for a non-zero exit status (with
// the stderr tail when cmd.Stderr is nil). Stdin, if set, is drained.
func (g *Guest) Run(ctx context.Context, cmd *vm.GuestCmd) error {
	if cmd == nil || cmd.Path == "" {
		return fmt.Errorf("vmtest: guest command path is required")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if cmd.Stdin != nil {
		if _, err := io.Copy(io.Discard, cmd.Stdin); err != nil {
			return fmt.Errorf("vmtest: read stdin: %w", err)
		}
	}
	g.mu.Lock()
	g.commands = append(g.commands, cloneCmd(cmd))
	result, ok := g.Commands[cmd.Path]
	g.mu.Unlock()
	if !ok {
		return fmt.Errorf("vmtest: %q: %w", cmd.Path, exec.ErrNotFound)
	}
	if cmd.Stdout != nil && len(result.Stdout) > 0 {
		if _, err := cmd.Stdout.Write(result.Stdout); err != nil {
			return err
		}
	}
	if cmd.Stderr != nil && len(result.Stderr) > 0 {
		if _, err := cmd.Stderr.Write(result.Stderr); err != nil {
			return err
		}
	}
	if result.ExitCode != 0 {
		exit := &vm.ExitError{Code: result.ExitCode}
		if cmd.Stderr == nil {
			exit.Stderr = append([]byte(nil), result.Stderr...)
		}
		return exit
	}
	return nil
}

// Open opens name in FS. Absolute names are looked up relative to the
// filesystem root, so "/etc/hostname" and "etc/hostname" are the same
// file.
func (g *Guest) Open(ctx context.Context, name string) (io.ReadCloser, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.FS == nil {
		return nil, &fs.PathError{Op: "open", Path: name, Err: fs.ErrNotExist}
	}
	return g.FS.Open(fsName(name))
}

// Create returns a writer whose Close stores the written content in FS
// under name with the given mode.
func (g *Guest) Create(ctx context.Context, name string, mode fs.FileMode) (io.WriteCloser, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if !fs.ValidPath(fsName(name)) {
		return nil, &fs.PathError{Op: "create", Path: name, Err: fs.ErrInvalid}
	}
	return &fileWriter{guest: g, name: fsName(name), mode: mode}, nil
}

// Shutdown records the request. The guest stays usable, as a real guest
// does until it has actually powered down.
func (g *Guest) Shutdown(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	g.shutdowns++
	return nil
}

// Close records the call. The guest stays usable.
func (g *Guest) Close() error {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.closes++
	return nil
}

// CommandsRun returns the commands Run has received, in order. Stdin and the
// output writers are not recorded.
func (g *Guest) CommandsRun() []vm.GuestCmd {
	g.mu.Lock()
	defer g.mu.Unlock()
	return append([]vm.GuestCmd(nil), g.commands...)
}

// Shutdowns returns how many times Shutdown has been called.
func (g *Guest) Shutdowns() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.shutdowns
}

// Closes returns how many times Close has been called.
func (g *Guest) Closes() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.closes
}

func cloneCmd(cmd *vm.GuestCmd) vm.GuestCmd {
	return vm.GuestCmd{
		Path: cmd.Path,
		Args: append([]string(nil), cmd.Args...),
		Env:  append([]string(nil), cmd.Env...),
		Dir:  cmd.Dir,
	}
}

// fsName maps a guest path onto the fs.FS name space.
func fsName(name string) string {
	name = strings.TrimPrefix(name, "/")
	if name == "" {
		return "."
	}
	return name
}

type fileWriter struct {
	guest *Guest
	name  string
	mode  fs.FileMode
	data  []byte
}

func (w *fileWriter) Write(p []byte) (int, error) {
	w.data = append(w.data, p...)
	return len(p), nil
}

func (w *fileWriter) Close() error {
	w.guest.mu.Lock()
	defer w.guest.mu.Unlock()
	if w.guest.FS == nil {
		w.guest.FS = fstest.MapFS{}
	}
	w.guest.FS[w.name] = &fstest.MapFile{Data: w.data, Mode: w.mode, ModTime: time.Now()}
	return nil
}
