package userspace

import (
	"net"
	"sync"

	"gvisor.dev/gvisor/pkg/tcpip"
	"gvisor.dev/gvisor/pkg/tcpip/adapters/gonet"
	"gvisor.dev/gvisor/pkg/tcpip/network/ipv4"
	"gvisor.dev/gvisor/pkg/tcpip/stack"
	"gvisor.dev/gvisor/pkg/tcpip/transport/tcp"
	"gvisor.dev/gvisor/pkg/tcpip/transport/udp"
	"gvisor.dev/gvisor/pkg/waiter"

	"github.com/shazow/virtle/vm"
	"github.com/shazow/virtle/vmnet"
)

// forwardedFlow owns both ends of a guest-initiated flow. It is registered
// before dialing, so detaching the port cancels pending dials and closes
// existing flows before the port's address can be assigned again.
type forwardedFlow struct {
	p    *port
	id   stack.TransportEndpointID
	flow vmnet.Flow

	mu               sync.Mutex
	closed           bool
	upstream         net.Conn
	endpoint         tcpip.Endpoint
	handshakeStarted bool
	published        chan struct{}
	publish          sync.Once
	closeOnce        sync.Once
}

// close joins concurrent cleanup, so finish cannot release ownership while
// another caller is still closing a connection.
func (f *forwardedFlow) close() { f.closeOnce.Do(f.abort) }

func (f *forwardedFlow) abort() {
	f.mu.Lock()
	f.closed = true
	upstream, handshakeStarted := f.upstream, f.handshakeStarted
	f.mu.Unlock()
	if upstream != nil {
		_ = upstream.Close()
	}
	if handshakeStarted {
		// Wait only for endpoint publication, never for the guest's ACK.
		// The link signals before admitting SYN-ACK to its output queue.
		<-f.published
	}
	f.mu.Lock()
	ep := f.endpoint
	f.mu.Unlock()
	if ep != nil {
		ep.Abort()
	} else if handshakeStarted {
		if ep := f.p.n.stack.FindTransportEndpoint(ipv4.ProtocolNumber, tcp.ProtocolNumber, f.id, nicID); ep != nil {
			ep.Abort()
		}
	}
}

func (f *forwardedFlow) markPublished() { f.publish.Do(func() { close(f.published) }) }

func (f *forwardedFlow) finish() {
	f.close()
	f.p.mu.Lock()
	delete(f.p.flows, f)
	f.p.mu.Unlock()
}

// newFlow registers ownership before any dial or endpoint creation. DNS and
// external flows share this admission gate, which port Close seals before
// it tears down connections and releases the address.
func (p *port) newFlow(proto vm.Proto, id stack.TransportEndpointID) (*forwardedFlow, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return nil, net.ErrClosed
	}
	f := &forwardedFlow{p: p, id: id, flow: p.flow(proto, id), published: make(chan struct{})}
	p.flows[f] = struct{}{}
	return f, nil
}

// rejectTCP only resets a connection while its original attachment is live.
// A delayed handler must not reset a replacement guest's connection.
func (p *port) rejectTCP(r *tcp.ForwarderRequest) {
	p.ingress.Lock()
	defer p.ingress.Unlock()
	r.Complete(!p.isClosed())
}

// acceptTCP publishes an endpoint while remaining cancellable by port Close.
// DNS and external flows use the same handshake lifetime and teardown.
// The flow is registered first; SYN-ACK signals endpoint publication; only
// after the guest's ACK does CreateEndpoint return the established endpoint.
// Close waits for publication and aborts the endpoint, never waiting for ACK.
func (f *forwardedFlow) acceptTCP(r *tcp.ForwarderRequest) (net.Conn, error) {
	f.mu.Lock()
	if f.closed {
		f.mu.Unlock()
		r.Complete(false)
		return nil, net.ErrClosed
	}
	f.handshakeStarted = true
	f.mu.Unlock()
	var wq waiter.Queue
	ep, terr := r.CreateEndpoint(&wq)
	f.markPublished() // Also wake Close when creation fails before SYN-ACK.
	r.Complete(false)
	if terr != nil {
		return nil, tcpipError("TCP endpoint", terr)
	}
	f.mu.Lock()
	if f.closed {
		f.mu.Unlock()
		ep.Abort()
		return nil, net.ErrClosed
	}
	f.endpoint = ep
	f.mu.Unlock()
	return gonet.NewTCPConn(&wq, ep), nil
}

// acceptUDP creates and publishes the endpoint synchronously. Unlike TCP,
// UDP needs no guest handshake, so Close can be excluded for the whole call.
func (f *forwardedFlow) acceptUDP(r *udp.ForwarderRequest) (net.Conn, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.closed {
		return nil, net.ErrClosed
	}
	var wq waiter.Queue
	ep, err := r.CreateEndpoint(&wq)
	if err != nil {
		return nil, tcpipError("UDP endpoint", err)
	}
	f.endpoint = ep
	return gonet.NewUDPConn(&wq, ep), nil
}
