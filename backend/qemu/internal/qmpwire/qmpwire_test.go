package qmpwire

import (
	"context"
	"errors"
	"io"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/shazow/virtle/backend/qemu/limits"
)

func TestAppendDelimiter(t *testing.T) {
	if got := string(AppendDelimiter([]byte(`{"execute":"x"}`))); got != "{\"execute\":\"x\"}\n" {
		t.Fatalf("unexpected delimited command: %q", got)
	}
	if got := string(AppendDelimiter([]byte("{}\n"))); got != "{}\n" {
		t.Fatalf("expected already-delimited command unchanged, got %q", got)
	}
}

func TestDecoderAcceptsFramesAtLimit(t *testing.T) {
	frame := `{"return":"ok"}`
	decoder := NewDecoder(strings.NewReader(frame+"\n"+frame+"\n"), int64(len(frame)))
	for i := 0; i < 2; i++ {
		envelope, err := DecodeEnvelope(decoder)
		if err != nil {
			t.Fatalf("decode frame %d: %v", i, err)
		}
		if got := string(envelope.Return); got != `"ok"` {
			t.Fatalf("unexpected return: %s", got)
		}
	}
}

func TestDecoderRejectsOversizedFrameWithoutReadingPastLimit(t *testing.T) {
	const maxFrameSize = 32
	input := &countingReader{Reader: strings.NewReader(`{"return":"` + strings.Repeat("x", 1024) + `"}` + "\n")}
	_, err := DecodeEnvelope(NewDecoder(input, maxFrameSize))
	if !errors.Is(err, limits.ErrExceeded) || !IsWireError(err) {
		t.Fatalf("expected wire-level limit error, got %v", err)
	}
	if input.bytesRead > maxFrameSize+1 {
		t.Fatalf("oversized decode read %d bytes past %d-byte limit", input.bytesRead, maxFrameSize)
	}
}

type countingReader struct {
	io.Reader
	bytesRead int64
}

func (r *countingReader) Read(p []byte) (int, error) {
	n, err := r.Reader.Read(p)
	r.bytesRead += int64(n)
	return n, err
}

func TestDialWithRetryProbesAndCloses(t *testing.T) {
	dials := 0
	probes := 0
	closes := 0
	client, err := DialWithRetry(context.Background(), Retry[int]{
		Dial: func(context.Context) (int, error) {
			dials++
			return dials, nil
		},
		Probe: func(int) error {
			probes++
			if probes < 3 {
				return errors.New("not ready")
			}
			return nil
		},
		Close: func(int) { closes++ },
		Delay: time.Millisecond,
	})
	if err != nil {
		t.Fatalf("dial with retry: %v", err)
	}
	if client != 3 || dials != 3 || closes != 2 {
		t.Fatalf("unexpected retry accounting: client=%d dials=%d closes=%d", client, dials, closes)
	}
}

func TestDialWithRetryCheckAborts(t *testing.T) {
	watcherErr := errors.New("watched process exited")
	_, err := DialWithRetry(context.Background(), Retry[int]{
		Dial:  func(context.Context) (int, error) { return 0, errors.New("refused") },
		Check: func() error { return watcherErr },
		Delay: time.Millisecond,
	})
	if !errors.Is(err, watcherErr) {
		t.Fatalf("expected check error, got %v", err)
	}
}

func TestDialWithRetryHonorsContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := DialWithRetry(ctx, Retry[int]{
		Dial:  func(context.Context) (int, error) { return 0, errors.New("refused") },
		Delay: time.Millisecond,
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context error, got %v", err)
	}
}

// sessionTestRPCTimeout and sessionTestLockHold pin the queued-operation
// invariant: the hold outlasts the RPC bound, so a deadline armed before lock
// acquisition would already be expired when the queued operation runs.
const (
	sessionTestRPCTimeout = 100 * time.Millisecond
	sessionTestLockHold   = 3 * sessionTestRPCTimeout
)

func TestSessionQueuedOperationDeadlineStartsAtLockAcquisition(t *testing.T) {
	serverConn, clientConn := net.Pipe()
	defer serverConn.Close()
	defer clientConn.Close()

	// Echo one byte so the queued operation performs a real round trip.
	go func() {
		buf := make([]byte, 1)
		if _, err := serverConn.Read(buf); err != nil {
			return
		}
		_, _ = serverConn.Write(buf)
	}()

	session := &Session{Conn: clientConn, RPCTimeout: sessionTestRPCTimeout}

	entered := make(chan struct{})
	release := make(chan struct{})
	slowDone := make(chan error, 1)
	go func() {
		slowDone <- session.DoSlow(context.Background(), func() error {
			close(entered)
			<-release
			return nil
		})
	}()
	<-entered

	queuedDone := make(chan error, 1)
	go func() {
		queuedDone <- session.Do(context.Background(), func() error {
			if _, err := clientConn.Write([]byte("x")); err != nil {
				return &WireError{Err: err}
			}
			buf := make([]byte, 1)
			if _, err := clientConn.Read(buf); err != nil {
				return &WireError{Err: err}
			}
			return nil
		})
	}()

	// Mutex queuing is unobservable, so a timed hold longer than the RPC bound
	// is the only way to guarantee the queued operation waited past it.
	time.Sleep(sessionTestLockHold)
	close(release)

	if err := <-slowDone; err != nil {
		t.Fatalf("slow operation: %v", err)
	}
	if err := <-queuedDone; err != nil {
		t.Fatalf("queued operation should start with a fresh deadline, got %v", err)
	}
}

func TestSessionRejectsEndedContextWithoutPoisoning(t *testing.T) {
	serverConn, clientConn := net.Pipe()
	defer serverConn.Close()
	defer clientConn.Close()
	session := &Session{Conn: clientConn, RPCTimeout: sessionTestRPCTimeout}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	ran := false
	err := session.Do(ctx, func() error {
		ran = true
		return nil
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("ended context: got %v want %v", err, context.Canceled)
	}
	if ran {
		t.Fatal("operation ran despite the ended context")
	}
	if err := session.Do(context.Background(), func() error { return nil }); err != nil {
		t.Fatalf("session unusable after an ended-context call: %v", err)
	}
}
