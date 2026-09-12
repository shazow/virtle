package userspace

import (
	"context"
	"net"
	"net/netip"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"gvisor.dev/gvisor/pkg/tcpip"
	"gvisor.dev/gvisor/pkg/tcpip/network/ipv4"
	"gvisor.dev/gvisor/pkg/tcpip/transport/tcp"
	"gvisor.dev/gvisor/pkg/tcpip/transport/udp"

	"github.com/shazow/virtle/vm"
	"github.com/shazow/virtle/vmnet"
)

// port is one attached guest NIC.
type port struct {
	n            *Network
	ctx          context.Context
	cancel       context.CancelFunc
	name         string
	egress       *vm.Egress // the guest's policy data, carried on its flows
	addr         netip.Addr
	addr4        tcpip.Address
	mac          net.HardwareAddr
	link         vmnet.Link
	out          chan []byte    // frames for the guest
	done         chan struct{}  // closed with the port
	attached     time.Time      // compared with queued UDP receive timestamps
	ingress      sync.Mutex     // serializes packet injection with address release
	dnsTCP       *tcp.Forwarder // independent admission budget for gateway DNS
	tcpForwarder *tcp.Forwarder // capture this attachment before dispatching transport handlers
	udpForwarder *udp.Forwarder

	mu        sync.Mutex
	closed    bool
	exposures map[forwardKey]*exposure
	flows     map[*forwardedFlow]struct{}
	closeOnce sync.Once
	closeErr  error

	dropped     atomic.Uint64 // frames the guest did not read in time
	rejected    atomic.Uint64 // frames with a source that is not the guest's
	warnedSpoof atomic.Bool
	warnedDrop  atomic.Bool
}

func (p *port) Addr() netip.Addr { return p.addr }

func (p *port) MAC() net.HardwareAddr { return slices.Clone(p.mac) }

// enqueue hands a frame to the guest's writer, dropping it when the guest
// has fallen behind.
func (p *port) enqueue(frame []byte) {
	select {
	case p.out <- frame:
	default:
		p.dropped.Add(1)
		p.warnOnce(&p.warnedDrop, "dropping frames the guest does not read in time")
	}
}

func (p *port) warnOnce(flag *atomic.Bool, msg string) {
	if flag.CompareAndSwap(false, true) {
		p.n.logger.Warn(msg, "guest", p.name, "addr", p.addr)
	}
}

func (p *port) isClosed() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.closed
}

// Close implements vmnet.Port.
func (p *port) Close() error {
	p.closeOnce.Do(func() { p.closeErr = p.close() })
	return p.closeErr
}

func (p *port) close() error {
	p.mu.Lock()
	p.closed = true
	p.cancel()
	flows := make([]*forwardedFlow, 0, len(p.flows))
	for f := range p.flows {
		flows = append(flows, f)
	}
	exposures := make([]*exposure, 0, len(p.exposures))
	for _, e := range p.exposures {
		exposures = append(exposures, e)
	}
	close(p.done)
	p.mu.Unlock()
	// Finish gateway packet injection and reply writes before releasing
	// the address for another attachment.
	p.ingress.Lock()
	//lint:ignore SA2001 Lock acquisition is the synchronization barrier.
	p.ingress.Unlock()
	for _, f := range flows {
		f.close()
	}
	for _, e := range exposures {
		_ = e.Close()
	}
	_ = p.n.stack.RemoveNeighbor(nicID, ipv4.ProtocolNumber, p.addr4)
	p.n.forget(p)
	err := p.link.Close()
	p.n.logger.Info("network port closed", "guest", p.name, "addr", p.addr,
		"dropped", p.dropped.Load(), "rejected", p.rejected.Load())
	return err
}

var _ vmnet.Port = (*port)(nil)
