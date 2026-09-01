package vmtest

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"path"
	"strings"
	"sync"
	"testing/fstest"

	"github.com/shazow/virtle/vm"
)

// Result is a scripted command outcome.
type Result struct {
	Stdout, Stderr []byte
	ExitCode       int
	Err            error
}

// Guest is an in-memory vm.Guest. Commands are keyed by GuestCmd.Path. FS
// paths may be supplied to Guest methods with or without a leading slash.
type Guest struct {
	FS       fstest.MapFS
	Commands map[string]Result

	mu sync.Mutex
}

var _ vm.Guest = (*Guest)(nil)

func (g *Guest) Run(_ context.Context, cmd *vm.GuestCmd) error {
	if cmd == nil || cmd.Path == "" {
		return fmt.Errorf("guest command path is required")
	}
	g.mu.Lock()
	result, ok := g.Commands[cmd.Path]
	result.Stdout = bytes.Clone(result.Stdout)
	result.Stderr = bytes.Clone(result.Stderr)
	g.mu.Unlock()
	if !ok {
		return &fs.PathError{Op: "run", Path: cmd.Path, Err: fs.ErrNotExist}
	}

	var writeErr error
	writeErr = errors.Join(writeErr, writeCommandOutput("stdout", cmd.Stdout, result.Stdout))
	writeErr = errors.Join(writeErr, writeCommandOutput("stderr", cmd.Stderr, result.Stderr))
	if result.Err != nil {
		return errors.Join(result.Err, writeErr)
	}
	if result.ExitCode != 0 {
		exitErr := &vm.ExitError{Code: result.ExitCode}
		if cmd.Stderr == nil {
			exitErr.Stderr = result.Stderr
		}
		return errors.Join(exitErr, writeErr)
	}
	return writeErr
}

func writeCommandOutput(name string, dst io.Writer, data []byte) error {
	if dst == nil || len(data) == 0 {
		return nil
	}
	if _, err := io.Copy(dst, bytes.NewReader(data)); err != nil {
		return fmt.Errorf("write guest %s: %w", name, err)
	}
	return nil
}

func (g *Guest) Open(_ context.Context, name string) (io.ReadCloser, error) {
	guestPath, err := cleanPath(name)
	if err != nil {
		return nil, err
	}
	g.mu.Lock()
	data, err := fs.ReadFile(g.FS, guestPath)
	g.mu.Unlock()
	if err != nil {
		return nil, err
	}
	return io.NopCloser(bytes.NewReader(data)), nil
}

func (g *Guest) Create(_ context.Context, name string, mode fs.FileMode) (io.WriteCloser, error) {
	guestPath, err := cleanPath(name)
	if err != nil {
		return nil, err
	}
	g.mu.Lock()
	if g.FS == nil {
		g.FS = fstest.MapFS{}
	}
	g.FS[guestPath] = &fstest.MapFile{Mode: mode}
	g.mu.Unlock()
	return &fileWriter{guest: g, path: guestPath, mode: mode}, nil
}

func (g *Guest) Shutdown(context.Context) error { return nil }

func (g *Guest) Close() error { return nil }

func cleanPath(name string) (string, error) {
	cleaned := strings.TrimPrefix(path.Clean(name), "/")
	if !fs.ValidPath(cleaned) || cleaned == "." {
		return "", &fs.PathError{Op: "open", Path: name, Err: fs.ErrInvalid}
	}
	return cleaned, nil
}

type fileWriter struct {
	guest  *Guest
	path   string
	mode   fs.FileMode
	buffer bytes.Buffer
	closed bool
}

func (w *fileWriter) Write(p []byte) (int, error) {
	if w.closed {
		return 0, fs.ErrClosed
	}
	return w.buffer.Write(p)
}

func (w *fileWriter) Close() error {
	if w.closed {
		return nil
	}
	w.closed = true
	w.guest.mu.Lock()
	w.guest.FS[w.path] = &fstest.MapFile{Data: bytes.Clone(w.buffer.Bytes()), Mode: w.mode}
	w.guest.mu.Unlock()
	return nil
}
