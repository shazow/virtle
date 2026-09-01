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

// exitErrorStderrLimit bounds the stderr tail carried by vm.ExitError when
// the caller did not collect stderr itself.
const exitErrorStderrLimit = 32 << 10

// Run executes cmd through guest-exec. QGA delivers output only once the
// command has exited, so cmd.Stdout and cmd.Stderr receive the buffered
// output at completion; a non-zero exit is returned as *vm.ExitError.
func (g *qgaGuest) Run(ctx context.Context, cmd *vm.GuestCmd) error {
	if cmd == nil || cmd.Path == "" {
		return fmt.Errorf("guest command path is required")
	}
	if cmd.Stdin != nil {
		return fmt.Errorf("guest command stdin over QGA: %w", errors.ErrUnsupported)
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
		return err
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
		return err
	}
	stdout, err := base64.StdEncoding.DecodeString(status.OutData)
	if err != nil {
		return fmt.Errorf("decode guest stdout: %w", err)
	}
	stderr, err := base64.StdEncoding.DecodeString(status.ErrData)
	if err != nil {
		return fmt.Errorf("decode guest stderr: %w", err)
	}
	if cmd.Stdout != nil && len(stdout) > 0 {
		if _, err := cmd.Stdout.Write(stdout); err != nil {
			return fmt.Errorf("write guest stdout: %w", err)
		}
	}
	if cmd.Stderr != nil && len(stderr) > 0 {
		if _, err := cmd.Stderr.Write(stderr); err != nil {
			return fmt.Errorf("write guest stderr: %w", err)
		}
	}
	if status.ExitCode != 0 {
		exit := &vm.ExitError{Code: status.ExitCode}
		if cmd.Stderr == nil {
			if len(stderr) > exitErrorStderrLimit {
				stderr = stderr[len(stderr)-exitErrorStderrLimit:]
			}
			exit.Stderr = stderr
		}
		return exit
	}
	return nil
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
	return &guestFileWriter{ctx: ctx, client: client, handle: handle, name: name, mode: mode}, nil
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
	err := w.client.CloseFile(w.ctx, w.handle)
	if err == nil && w.mode != 0 {
		status, chmodErr := qga.RunCommandStatus(w.ctx, w.client, qga.ExecWait{
			Name:          "chmod",
			Path:          "chmod",
			Args:          []string{fmt.Sprintf("%03o", w.mode.Perm()), w.name},
			Env:           []string{qga.InternalCommandPathEnv},
			Subject:       w.name,
			CaptureOutput: true,
		})
		err = chmodErr
		if err == nil && status.ExitCode != 0 {
			err = fmt.Errorf("chmod %q exited with status %d%s", w.name, status.ExitCode, qga.ExecOutputSuffix(status))
		}
	}
	return errors.Join(err, w.client.Disconnect())
}
