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
}

// NewServer returns a closable control server for router.
func NewServer(h *Router) (*Server, error) {
	if h == nil {
		return nil, fmt.Errorf("control handler is required")
	}
	return &Server{handler: h}, nil
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
		select {
		case handlerSlots <- struct{}{}:
			go func() {
				defer func() { <-handlerSlots }()
				s.handleConn(conn)
			}()
		default:
			s.rejectConn(conn, &limits.Error{
				Resource: "concurrent control requests",
				Limit:    int64(maxHandlers),
				Unit:     "handlers",
			})
		}
	}
}

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

func (s *Server) handleConn(conn net.Conn) {
	defer conn.Close()
	readTimeout := s.effectiveRequestReadTimeout()
	if err := conn.SetReadDeadline(time.Now().Add(readTimeout)); err != nil {
		writeResponse(conn, responseEnvelope{Error: &RPCError{Code: ErrInternal, Message: err.Error()}})
		return
	}
	var req requestEnvelope
	if err := decodeRequest(conn, s.effectiveMaxRequestSize(), &req); err != nil {
		code := ErrInvalidRequest
		if errors.Is(err, limits.ErrExceeded) {
			code = ErrResourceLimit
		}
		writeResponse(conn, responseEnvelope{Error: &RPCError{Code: code, Message: err.Error()}})
		return
	}
	if err := conn.SetReadDeadline(time.Time{}); err != nil {
		writeResponse(conn, responseEnvelope{Error: &RPCError{Code: ErrInternal, Message: err.Error()}})
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
	writeResponse(conn, s.handler.handle(ctx, req))
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

func (s *Server) rejectConn(conn net.Conn, err error) {
	defer conn.Close()
	_ = conn.SetWriteDeadline(time.Now().Add(s.effectiveRequestReadTimeout()))
	writeResponse(conn, responseEnvelope{Error: &RPCError{Code: ErrResourceLimit, Message: err.Error()}})
}

func decodeRequest(reader io.Reader, maxSize int64, req *requestEnvelope) error {
	limited := &io.LimitedReader{R: reader, N: maxSize + 1}
	decoder := json.NewDecoder(limited)
	err := decoder.Decode(req)
	if limited.N == 0 || decoder.InputOffset() > maxSize {
		return &limits.Error{Resource: "control request", Limit: maxSize}
	}
	return err
}

func writeResponse(conn net.Conn, resp responseEnvelope) {
	_ = json.NewEncoder(conn).Encode(resp)
}
