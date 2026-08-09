package qemu

import (
	"bytes"
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"io/fs"
	"strconv"
	"sync"
	"time"

	shellquote "github.com/kballard/go-shellquote"
	"github.com/shazow/virtle/internal/qga"
	"github.com/shazow/virtle/vm"
)

// fileChunkSize bounds one guest-agent file round trip. The agent base64-encodes
// every payload, so chunks trade round trips against encoding overhead.
const fileChunkSize = 512 * 1024

// qgaGuest adapts the QEMU guest agent to [vm.Guest].
//
// The adapter is where QGA's shape stops: callers see os/exec- and io/fs-like
// operations, and the base64 chunking, exec polling, and handle bookkeeping the
// agent requires stay here. Two of the agent's limits are papered over rather
// than hidden — see Run and Create.
type qgaGuest struct {
	client qga.Client

	// timeout bounds one operation when the caller's context carries no
	// deadline of its own, so a wedged agent cannot hang a program that
	// passed context.Background(). Zero means no bound.
	timeout time.Duration

	closeOnce sync.Once
	closeErr  error
}

var _ vm.Guest = (*qgaGuest)(nil)

// Run executes a command in the guest and waits for it to exit.
//
// The guest agent has no notion of a working directory, so a command with Dir
// set runs through the guest's /bin/sh, which changes directory first. Commands
// without Dir are executed directly, with no shell involved.
func (g *qgaGuest) Run(ctx context.Context, cmd *vm.GuestCmd) (*vm.GuestResult, error) {
	if cmd == nil || cmd.Path == "" {
		return nil, fmt.Errorf("guest command path is required")
	}
	ctx, cancel := g.withTimeout(ctx)
	defer cancel()

	path, args := cmd.Path, cmd.Args
	if cmd.Dir != "" {
		path, args = shellCommand(cmd.Dir, cmd.Path, cmd.Args)
	}
	var input []byte
	if cmd.Stdin != nil {
		var err error
		if input, err = io.ReadAll(cmd.Stdin); err != nil {
			return nil, fmt.Errorf("read guest command stdin: %w", err)
		}
	}

	status, err := qga.RunCommandStatus(ctx, g.client, qga.ExecWait{
		Name:          "guest exec",
		Path:          path,
		Args:          args,
		Subject:       cmd.Path,
		CaptureOutput: true,
		Env:           cmd.Env,
		InputData:     input,
	})
	if err != nil {
		return nil, err
	}
	stdout, err := decodeExecStream(status.OutData, "stdout")
	if err != nil {
		return nil, err
	}
	stderr, err := decodeExecStream(status.ErrData, "stderr")
	if err != nil {
		return nil, err
	}
	return &vm.GuestResult{ExitCode: status.ExitCode, Stdout: stdout, Stderr: stderr}, nil
}

// shellCommand wraps argv so it runs with a working directory. exec keeps the
// shell from lingering as a parent process, and "sh" fills $0 so the command's
// own arguments start at $1.
func shellCommand(dir string, path string, args []string) (string, []string) {
	command := shellquote.Join(append([]string{path}, args...)...)
	script := "cd " + shellquote.Join(dir) + " && exec " + command
	return "/bin/sh", []string{"-c", script, "sh"}
}

func decodeExecStream(data string, name string) ([]byte, error) {
	if data == "" {
		return nil, nil
	}
	decoded, err := base64.StdEncoding.DecodeString(data)
	if err != nil {
		return nil, fmt.Errorf("decode guest command %s: %w", name, err)
	}
	return decoded, nil
}

// Open opens a guest file for reading. The returned reader pulls one chunk per
// round trip rather than buffering the whole file, so large reads stream.
func (g *qgaGuest) Open(ctx context.Context, name string) (io.ReadCloser, error) {
	openCtx, cancel := g.withTimeout(ctx)
	defer cancel()

	handle, err := g.client.OpenFileRead(openCtx, name)
	if err != nil {
		return nil, err
	}
	return &guestFileReader{guest: g, name: name, handle: handle}, nil
}

// Create creates or truncates a guest file. Contents are not complete until the
// returned writer is closed.
//
// The guest agent cannot set a mode when opening a file, so a non-zero mode is
// applied with a chmod in the guest after the contents are written.
func (g *qgaGuest) Create(ctx context.Context, name string, mode fs.FileMode) (io.WriteCloser, error) {
	openCtx, cancel := g.withTimeout(ctx)
	defer cancel()

	handle, err := g.client.OpenFile(openCtx, name)
	if err != nil {
		return nil, err
	}
	return &guestFileWriter{guest: g, name: name, handle: handle, mode: mode, ctx: ctx}, nil
}

func (g *qgaGuest) Shutdown(ctx context.Context) error {
	ctx, cancel := g.withTimeout(ctx)
	defer cancel()
	return g.client.Shutdown(ctx)
}

func (g *qgaGuest) Close() error {
	g.closeOnce.Do(func() { g.closeErr = g.client.Disconnect() })
	return g.closeErr
}

// chmod applies mode to a guest path, for the operations the agent cannot
// express directly.
func (g *qgaGuest) chmod(ctx context.Context, name string, mode fs.FileMode) error {
	result, err := g.Run(ctx, &vm.GuestCmd{
		Path: "chmod",
		Args: []string{"0" + strconv.FormatUint(uint64(mode.Perm()), 8), name},
	})
	if err != nil {
		return fmt.Errorf("chmod guest file %q: %w", name, err)
	}
	if result.ExitCode != 0 {
		return fmt.Errorf("chmod guest file %q: exit status %d: %s", name, result.ExitCode, bytes.TrimSpace(result.Stderr))
	}
	return nil
}

func (g *qgaGuest) withTimeout(ctx context.Context) (context.Context, context.CancelFunc) {
	if g.timeout <= 0 {
		return ctx, func() {}
	}
	if _, ok := ctx.Deadline(); ok {
		return ctx, func() {}
	}
	return context.WithTimeout(ctx, g.timeout)
}

// guestFileReader reads a guest file one agent chunk at a time.
type guestFileReader struct {
	guest  *qgaGuest
	name   string
	handle int

	pending []byte
	eof     bool
	closed  bool
}

func (r *guestFileReader) Read(p []byte) (int, error) {
	for len(r.pending) == 0 {
		if r.closed {
			return 0, fs.ErrClosed
		}
		if r.eof {
			return 0, io.EOF
		}
		ctx, cancel := r.guest.withTimeout(context.Background())
		payload, eof, err := r.guest.client.ReadFile(ctx, r.handle, fileChunkSize)
		cancel()
		if err != nil {
			return 0, err
		}
		chunk, err := base64.StdEncoding.DecodeString(payload)
		if err != nil {
			return 0, fmt.Errorf("decode guest file %q chunk: %w", r.name, err)
		}
		r.pending = chunk
		r.eof = eof
	}
	n := copy(p, r.pending)
	r.pending = r.pending[n:]
	return n, nil
}

func (r *guestFileReader) Close() error {
	if r.closed {
		return nil
	}
	r.closed = true
	ctx, cancel := r.guest.withTimeout(context.Background())
	defer cancel()
	return r.guest.client.CloseFile(ctx, r.handle)
}

// guestFileWriter writes a guest file one agent chunk at a time, applying the
// requested mode when the file is complete.
type guestFileWriter struct {
	guest  *qgaGuest
	name   string
	handle int
	mode   fs.FileMode

	// ctx is the caller's Create context, kept for the chmod that Close
	// issues; the writes themselves are bounded by the agent timeout.
	ctx context.Context

	closed bool
}

func (w *guestFileWriter) Write(p []byte) (int, error) {
	if w.closed {
		return 0, fs.ErrClosed
	}
	written := 0
	for written < len(p) {
		end := min(written+fileChunkSize, len(p))
		ctx, cancel := w.guest.withTimeout(context.Background())
		err := w.guest.client.WriteFile(ctx, w.handle, base64.StdEncoding.EncodeToString(p[written:end]))
		cancel()
		if err != nil {
			return written, err
		}
		written = end
	}
	return written, nil
}

func (w *guestFileWriter) Close() error {
	if w.closed {
		return nil
	}
	w.closed = true
	ctx, cancel := w.guest.withTimeout(context.Background())
	err := w.guest.client.CloseFile(ctx, w.handle)
	cancel()
	if err != nil {
		return err
	}
	if w.mode == 0 {
		return nil
	}
	return w.guest.chmod(w.ctx, w.name, w.mode)
}
