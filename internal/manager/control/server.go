package control

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"sync"
)

// Server serves control socket requests for a router.
type Server struct {
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
func Serve(l net.Listener, h *Router) error {
	server, err := NewServer(h)
	if err != nil {
		return err
	}
	return server.Serve(l)
}

// ListenAndServe opens path and serves control requests for h.
func ListenAndServe(path string, h *Router) error {
	listener, err := Listen(path)
	if err != nil {
		return err
	}
	return Serve(listener, h)
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
	for {
		conn, err := l.Accept()
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return nil
			}
			return err
		}
		go s.handleConn(conn)
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
	var req requestEnvelope
	if err := json.NewDecoder(conn).Decode(&req); err != nil {
		writeResponse(conn, responseEnvelope{Error: &RPCError{Code: ErrInvalidRequest, Message: err.Error()}})
		return
	}
	resp := s.handler.handle(context.Background(), req)
	writeResponse(conn, resp)
}

func writeResponse(conn net.Conn, resp responseEnvelope) {
	_ = json.NewEncoder(conn).Encode(resp)
}
