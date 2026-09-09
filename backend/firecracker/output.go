package firecracker

import (
	"io"
	"sync"
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

// Keep a bounded diagnostic tail, even when a VMM floods stderr. The child
// receives the original successful write size, so truncation cannot block it.
type diagnosticWriter struct {
	mu   sync.Mutex
	data []byte
}

func (w *diagnosticWriter) Write(p []byte) (int, error) {
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
func (w *diagnosticWriter) String() string { w.mu.Lock(); defer w.mu.Unlock(); return string(w.data) }
