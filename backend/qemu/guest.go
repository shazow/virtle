package qemu

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"io/fs"

	shellquote "github.com/kballard/go-shellquote"
	"github.com/shazow/virtle/backend/qemu/internal/qga"
	"github.com/shazow/virtle/vm"
)

// guestHost is the slice of vmm.VM the QGA adapter needs; an interface
// so the adapter is testable against fakes.
type guestHost interface {
	DialGuestAgent(ctx context.Context) (qga.Client, error)
	ShutdownGuest(ctx context.Context) error
}

// qgaGuest adapts the QEMU Guest Agent to vm.Guest. Each operation dials
// the agent socket independently, so the type is safe for concurrent use;
// blocking is bounded by the caller's context.
type qgaGuest struct {
	vm guestHost
}

var _ vm.Guest = (*qgaGuest)(nil)

func (g *qgaGuest) Run(ctx context.Context, cmd *vm.GuestCmd) (*vm.GuestResult, error) {
	if cmd == nil || cmd.Path == "" {
		return nil, fmt.Errorf("guest command path is required")
	}
	if cmd.Stdin != nil {
		return nil, fmt.Errorf("guest command stdin over QGA: %w", errors.ErrUnsupported)
	}
	path, args := cmd.Path, cmd.Args
	if cmd.Dir != "" || len(cmd.Env) > 0 {
		// QGA's guest-exec has no working directory, and its env parameter
		// replaces rather than augments the inherited environment. Lower both
		// operations onto a shell wrapper to preserve GuestCmd's semantics.
		script := ""
		if cmd.Dir != "" {
			script += "cd " + shellquote.Join(cmd.Dir) + " && "
		}
		script += "exec " + shellquote.Join(append([]string{cmd.Path}, cmd.Args...)...)
		wrapped := []string{"-c", script}
		if len(cmd.Env) > 0 {
			wrapped = []string{"-c", "export " + shellquote.Join(cmd.Env...) + " && " + script}
		}
		path, args = "/bin/sh", wrapped
	}

	client, err := g.vm.DialGuestAgent(ctx)
	if err != nil {
		return nil, err
	}
	defer client.Disconnect()

	status, err := qga.RunCommandStatus(ctx, client, qga.ExecWait{
		Name:          "guest command",
		Path:          path,
		Args:          args,
		Subject:       cmd.Path,
		CaptureOutput: true,
	})
	if err != nil {
		return nil, err
	}
	stdout, err := base64.StdEncoding.DecodeString(status.OutData)
	if err != nil {
		return nil, fmt.Errorf("decode guest stdout: %w", err)
	}
	stderr, err := base64.StdEncoding.DecodeString(status.ErrData)
	if err != nil {
		return nil, fmt.Errorf("decode guest stderr: %w", err)
	}
	return &vm.GuestResult{ExitCode: status.ExitCode, Stdout: stdout, Stderr: stderr}, nil
}

func (g *qgaGuest) Open(ctx context.Context, name string) (io.ReadCloser, error) {
	client, err := g.vm.DialGuestAgent(ctx)
	if err != nil {
		return nil, err
	}
	handle, err := client.OpenFileRead(ctx, name)
	if err != nil {
		return nil, errors.Join(fmt.Errorf("open guest file %q: %w", name, err), client.Disconnect())
	}
	return &guestFileReader{ctx: ctx, client: client, handle: handle}, nil
}

func (g *qgaGuest) Create(ctx context.Context, name string, mode fs.FileMode) (io.WriteCloser, error) {
	client, err := g.vm.DialGuestAgent(ctx)
	if err != nil {
		return nil, err
	}
	handle, err := client.OpenFile(ctx, name)
	if err != nil {
		return nil, errors.Join(fmt.Errorf("create guest file %q: %w", name, err), client.Disconnect())
	}
	return &guestFileWriter{ctx: ctx, guest: g, client: client, handle: handle, name: name, mode: mode}, nil
}

func (g *qgaGuest) Shutdown(ctx context.Context) error {
	return g.vm.ShutdownGuest(ctx)
}

// Close releases the host side of the guest connection. The QGA adapter
// dials per operation and holds no persistent connection.
func (g *qgaGuest) Close() error { return nil }

// guestFileReader streams a guest file through chunked QGA reads.
type guestFileReader struct {
	ctx    context.Context
	client qga.Client
	handle int
	buf    []byte
	eof    bool
}

func (r *guestFileReader) Read(p []byte) (int, error) {
	for len(r.buf) == 0 {
		if r.eof {
			return 0, io.EOF
		}
		encoded, eof, err := r.client.ReadFile(r.ctx, r.handle, qga.DefaultFileReadChunkSize)
		if err != nil {
			return 0, err
		}
		chunk, err := base64.StdEncoding.DecodeString(encoded)
		if err != nil {
			return 0, fmt.Errorf("decode guest file chunk: %w", err)
		}
		r.buf = chunk
		r.eof = eof
	}
	n := copy(p, r.buf)
	r.buf = r.buf[n:]
	return n, nil
}

func (r *guestFileReader) Close() error {
	return errors.Join(r.client.CloseFile(r.ctx, r.handle), r.client.Disconnect())
}

// guestFileWriter streams a guest file through chunked QGA writes and
// applies the requested mode on Close.
type guestFileWriter struct {
	ctx    context.Context
	guest  *qgaGuest
	client qga.Client
	handle int
	name   string
	mode   fs.FileMode
}

func (w *guestFileWriter) Write(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	if err := w.client.WriteFile(w.ctx, w.handle, base64.StdEncoding.EncodeToString(p)); err != nil {
		return 0, err
	}
	return len(p), nil
}

func (w *guestFileWriter) Close() error {
	err := errors.Join(w.client.CloseFile(w.ctx, w.handle), w.client.Disconnect())
	if err == nil && w.mode != 0 {
		_, err = w.guest.Run(w.ctx, &vm.GuestCmd{
			Path: "chmod",
			Args: []string{fmt.Sprintf("%03o", w.mode.Perm()), w.name},
		})
	}
	return err
}
