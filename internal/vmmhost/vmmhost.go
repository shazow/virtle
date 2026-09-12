// Package vmmhost runs a microVM monitor as a child process configured over
// an API socket: Firecracker and Cloud Hypervisor share one lifecycle (the
// VM-name lock and control socket, a private runtime directory for the API
// socket, the serial console on the process's standard streams, startup and
// shutdown deadlines, and teardown) and each supplies what differs: its
// command line, the API calls that configure and boot the guest, and its
// graceful shutdown request.
package vmmhost

import (
	"io"
	"sync"
	"time"
)

const (
	// UnixPathMax is sizeof(sun_path) on Linux; longer socket paths cannot bind.
	UnixPathMax = 108

	socketRetryInterval = 10 * time.Millisecond
	killWaitTimeout     = 2 * time.Second
)

// Writers supplied by callers need not support concurrent stdout/stderr writes.
type lockedWriter struct {
	mu     sync.Mutex
	writer io.Writer
}

func (w *lockedWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.writer.Write(p)
}

// DiagnosticWriter keeps a bounded tail of what a child wrote to stderr, even
// when it floods. The child receives the original successful write size, so
// truncation cannot block it. The zero value is ready to use.
type DiagnosticWriter struct {
	mu   sync.Mutex
	data []byte
}

func (w *DiagnosticWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	n := len(p)
	const limit = 1024
	if len(p) >= limit {
		w.data = append(w.data[:0], p[len(p)-limit:]...)
	} else {
		if extra := len(w.data) + len(p) - limit; extra > 0 {
			copy(w.data, w.data[extra:])
			w.data = w.data[:len(w.data)-extra]
		}
		w.data = append(w.data, p...)
	}
	return n, nil
}

// String returns the retained tail.
func (w *DiagnosticWriter) String() string { w.mu.Lock(); defer w.mu.Unlock(); return string(w.data) }
