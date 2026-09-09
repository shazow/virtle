package console

import (
	"bufio"
	"bytes"
	"errors"
	"io"
	"strings"
	"testing"
	"time"
)

const testTimeout = 5 * time.Second

func newTestHub(t *testing.T, output io.Writer) *Hub {
	t.Helper()
	hub, err := New(output)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = hub.Close() })
	return hub
}

// readLine returns the next line from term or fails after the test timeout.
func readLine(t *testing.T, scanner *bufio.Scanner) string {
	t.Helper()
	lines := make(chan string, 1)
	go func() {
		if scanner.Scan() {
			lines <- scanner.Text()
			return
		}
		lines <- ""
	}()
	select {
	case line := <-lines:
		return line
	case <-time.After(testTimeout):
		t.Fatal("no console line arrived")
		return ""
	}
}

func TestOutputReachesWriterHistoryAndTerms(t *testing.T) {
	var output bytes.Buffer
	hub := newTestHub(t, &output)
	if _, err := io.WriteString(hub, "booting\nready\n"); err != nil {
		t.Fatal(err)
	}
	// A late attach still sees what the guest printed before.
	term := hub.Attach()
	defer term.Close()
	scanner := bufio.NewScanner(term)
	if got := readLine(t, scanner); got != "booting" {
		t.Fatalf("first line = %q, want the retained history", got)
	}
	if got := readLine(t, scanner); got != "ready" {
		t.Fatalf("second line = %q", got)
	}
	if _, err := io.WriteString(hub, "live\n"); err != nil {
		t.Fatal(err)
	}
	if got := readLine(t, scanner); got != "live" {
		t.Fatalf("live line = %q", got)
	}
	if output.String() != "booting\nready\nlive\n" {
		t.Fatalf("output writer got %q", output.String())
	}
}

func TestTermWritesReachTheInputPipe(t *testing.T) {
	hub := newTestHub(t, nil)
	term := hub.Attach()
	defer term.Close()
	if _, err := io.WriteString(term, "echo hi\n"); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 16)
	n, err := hub.Stdin().Read(buf)
	if err != nil || string(buf[:n]) != "echo hi\n" {
		t.Fatalf("guest stdin got %q, %v", buf[:n], err)
	}
}

func TestCloseEndsTermsWithEOFAfterPendingOutput(t *testing.T) {
	hub := newTestHub(t, nil)
	term := hub.Attach()
	if _, err := io.WriteString(hub, "last words\n"); err != nil {
		t.Fatal(err)
	}
	if err := hub.Close(); err != nil {
		t.Fatal(err)
	}
	data, err := io.ReadAll(term)
	if err != nil || string(data) != "last words\n" {
		t.Fatalf("ReadAll = %q, %v; want the pending output then EOF", data, err)
	}
	if _, err := io.WriteString(term, "x"); err == nil {
		t.Fatal("write after close succeeded")
	}
	// Attaching afterwards yields the history and EOF.
	late, err := io.ReadAll(hub.Attach())
	if err != nil || string(late) != "last words\n" {
		t.Fatalf("late attach = %q, %v", late, err)
	}
}

func TestSlowReaderIsDroppedNotTheConsole(t *testing.T) {
	hub := newTestHub(t, nil)
	term := hub.Attach()
	chunk := bytes.Repeat([]byte("x"), 64<<10)
	for range pendingLimit/len(chunk) + 1 {
		if _, err := hub.Write(chunk); err != nil {
			t.Fatalf("console write blocked or failed on a slow reader: %v", err)
		}
	}
	if _, err := io.ReadAll(term); !errors.Is(err, ErrFellBehind) {
		t.Fatalf("slow reader ended with %v, want ErrFellBehind", err)
	}
}

func TestHistoryIsBounded(t *testing.T) {
	hub := newTestHub(t, nil)
	if _, err := hub.Write(bytes.Repeat([]byte("a"), historyLimit)); err != nil {
		t.Fatal(err)
	}
	if _, err := io.WriteString(hub, "tail"); err != nil {
		t.Fatal(err)
	}
	term := hub.Attach()
	_ = hub.Close()
	data, _ := io.ReadAll(term)
	if len(data) != historyLimit || !strings.HasSuffix(string(data), "tail") {
		t.Fatalf("history length %d, suffix %q; want %d bytes ending in tail", len(data), data[max(0, len(data)-4):], historyLimit)
	}
}

func TestSerialTermHasNoWindowOrExitStatus(t *testing.T) {
	term := newTestHub(t, nil).Attach()
	defer term.Close()
	if err := term.Resize(80, 24); !errors.Is(err, errors.ErrUnsupported) {
		t.Fatalf("Resize = %v", err)
	}
	if _, err := term.Wait(t.Context()); !errors.Is(err, errors.ErrUnsupported) {
		t.Fatalf("Wait = %v", err)
	}
}
