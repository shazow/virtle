package control

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"sync"
	"time"

	"github.com/shazow/virtle/backend/qemu/limits"
)

// Server serves control socket requests for a router.
type Server struct {
	// MaxRequestSize bounds one request envelope. Zero uses
	// limits.DefaultMaxRequestSize.
	MaxRequestSize int64
	// MaxHandlers bounds concurrent request handlers. Zero uses
	// limits.DefaultMaxHandlers.
	MaxHandlers int
	// RequestReadTimeout bounds receipt of one request. Zero uses
	// limits.DefaultRequestReadTimeout.
	RequestReadTimeout time.Duration

	handler  *Router
	mu       sync.Mutex
	listener net.Listener
	closed   bool
	done     chan struct{}
	started  chan struct{}
	start    sync.Once
	handlers sync.WaitGroup
	// Count accepted connections before decoding, then release requests that
	// are not lifecycle RPCs before dispatching their handlers.
	lifecycle sync.WaitGroup
}

// NewServer returns a closable control server for router.
func NewServer(h *Router) (*Server, error) {
	if h == nil {
		return nil, fmt.Errorf("control handler is required")
	}
	return &Server{handler: h, started: make(chan struct{})}, nil
}

// Listen opens a private Unix socket at path for control requests.
func Listen(path string) (net.Listener, error) {
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	listener, err := net.Listen("unix", path)
	if err != nil {
		return nil, err
	}
	if err := os.Chmod(path, 0o600); err != nil {
		_ = listener.Close()
		return nil, err
	}
	return listener, nil
}

// Serve handles control requests from l until the listener closes.
func (s *Server) Serve(l net.Listener) error {
	if s.handler == nil {
		return fmt.Errorf("control handler is required")
	}
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return l.Close()
	}
	s.listener = l
	s.done = make(chan struct{})
	done := s.done
	s.mu.Unlock()
	s.start.Do(func() { close(s.started) })
	defer func() {
		s.mu.Lock()
		if s.listener == l {
			s.listener = nil
		}
		if s.done == done {
			s.done = nil
		}
		s.mu.Unlock()
		close(done)
	}()
	maxHandlers := s.MaxHandlers
	if maxHandlers <= 0 {
		maxHandlers = limits.DefaultMaxHandlers
	}
	handlerSlots := make(chan struct{}, maxHandlers)
	for {
		conn, err := l.Accept()
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return nil
			}
			return err
		}
		s.handlers.Add(1)
		s.lifecycle.Add(1)
		select {
		case handlerSlots <- struct{}{}:
			go func() {
				defer s.handlers.Done()
				defer func() { <-handlerSlots }()
				s.handleConn(conn, s.lifecycle.Done)
			}()
		default:
			// Reject asynchronously so a peer that never reads its response
			// cannot stall the accept loop for the write deadline.
			go func() {
				defer s.handlers.Done()
				defer s.lifecycle.Done()
				s.rejectConn(conn, &limits.Error{
					Resource: "concurrent control requests",
					Limit:    int64(maxHandlers),
					Unit:     "handlers",
				})
			}()
		}
	}
}

// Started closes once Serve has registered its listener.
func (s *Server) Started() <-chan struct{} { return s.started }

// Close stops accepting new control socket connections.
func (s *Server) Close() error {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	s.closed = true
	listener := s.listener
	s.mu.Unlock()
	if listener == nil {
		return nil
	}
	return listener.Close()
}

// Wait waits for Serve and all accepted connections, including response
// writes, to finish. Call Close first and release any blocked handlers before
// waiting. Close itself never waits, so handlers can safely initiate teardown.
func (s *Server) Wait() {
	if s == nil {
		return
	}
	s.waitServe()
	s.handlers.Wait()
}

// WaitLifecycle waits for Serve and accepted wait, kill, shutdown, and suspend
// responses to finish. Unclassified requests and rejected connections are also
// drained, bounded by the transport read/write deadlines. Other handlers are
// excluded as soon as their request is decoded.
//
// Call Close first and release blocked lifecycle handlers before waiting.
// Lifecycle handlers must not call WaitLifecycle themselves.
func (s *Server) WaitLifecycle() {
	if s == nil {
		return
	}
	s.waitServe()
	s.lifecycle.Wait()
}

func (s *Server) waitServe() {
	// All acceptance counts are added by Serve before it returns, so neither
	// drain can race a new WaitGroup.Add after Close has stopped acceptance.
	s.mu.Lock()
	done := s.done
	s.mu.Unlock()
	if done != nil {
		<-done
	}
}

func (s *Server) handleConn(conn net.Conn, releaseLifecycle func()) {
	defer func() {
		if releaseLifecycle != nil {
			releaseLifecycle()
		}
	}()
	defer conn.Close()
	// Bound writes separately from handler execution: lifecycle requests can
	// legitimately wait longer than the transport timeout for the VM to exit.
	reply := func(resp responseEnvelope) {
		_ = conn.SetWriteDeadline(time.Now().Add(s.effectiveRequestReadTimeout()))
		writeResponse(conn, resp)
	}
	readTimeout := s.effectiveRequestReadTimeout()
	if err := conn.SetReadDeadline(time.Now().Add(readTimeout)); err != nil {
		reply(responseEnvelope{Error: &RPCError{Code: ErrInternal, Message: err.Error()}})
		return
	}
	var req requestEnvelope
	if err := decodeRequest(conn, s.effectiveMaxRequestSize(), &req); err != nil {
		code := ErrInvalidRequest
		if errors.Is(err, limits.ErrExceeded) {
			code = ErrResourceLimit
		}
		reply(responseEnvelope{Error: &RPCError{Code: code, Message: err.Error()}})
		return
	}
	switch req.Method {
	case rpcWait, rpcKill, rpcShutdown, rpcSuspend:
		// Retain the acceptance count until the response write completes.
	default:
		if releaseLifecycle != nil {
			releaseLifecycle()
			releaseLifecycle = nil
		}
	}
	if err := conn.SetReadDeadline(time.Time{}); err != nil {
		reply(responseEnvelope{Error: &RPCError{Code: ErrInternal, Message: err.Error()}})
		return
	}
	// Cancel the handler when the peer goes away so an abandoned request does
	// not keep polling the guest forever. Requests are single-shot, so any
	// further read result means the client is done.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		defer cancel()
		var buf [1]byte
		_, _ = conn.Read(buf[:])
	}()
	reply(s.handler.handle(ctx, req))
}

func (s *Server) effectiveMaxRequestSize() int64 {
	if s.MaxRequestSize > 0 {
		return s.MaxRequestSize
	}
	return limits.DefaultMaxRequestSize
}

func (s *Server) effectiveRequestReadTimeout() time.Duration {
	if s.RequestReadTimeout > 0 {
		return s.RequestReadTimeout
	}
	return limits.DefaultRequestReadTimeout
}

// rejectConn answers a connection accepted over MaxHandlers with a
// resource-limit error. The request is read (and discarded) first, bounded
// like a served request: closing a Unix socket before the peer has written
// fails the peer's write with EPIPE, and it would never see the response
// that explains the rejection.
func (s *Server) rejectConn(conn net.Conn, err error) {
	defer conn.Close()
	timeout := s.effectiveRequestReadTimeout()
	_ = conn.SetReadDeadline(time.Now().Add(timeout))
	var req requestEnvelope
	_ = decodeRequest(conn, s.effectiveMaxRequestSize(), &req)
	_ = conn.SetWriteDeadline(time.Now().Add(timeout))
	writeResponse(conn, responseEnvelope{ID: req.ID, Error: &RPCError{Code: ErrResourceLimit, Message: err.Error()}})
}

func decodeRequest(reader io.Reader, maxSize int64, req *requestEnvelope) error {
	limited := &io.LimitedReader{R: reader, N: maxSize + 1}
	decoder := json.NewDecoder(limited)
	err := decoder.Decode(req)
	if err == nil {
		if decoder.InputOffset() > maxSize {
			return &limits.Error{Resource: "control request", Limit: maxSize}
		}
		return nil
	}
	// The value did not complete within the limit; the decoder's own error
	// only reflects the truncation.
	if limited.N == 0 {
		return &limits.Error{Resource: "control request", Limit: maxSize}
	}
	return err
}

func writeResponse(conn net.Conn, resp responseEnvelope) {
	_ = json.NewEncoder(conn).Encode(resp)
}
