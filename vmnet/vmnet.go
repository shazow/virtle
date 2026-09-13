// Package vmnet defines the host side of guest networking. A Link carries
// Ethernet frames for one guest NIC, a Network is what links attach to, and
// an Egress dials every guest-initiated flow. Backends produce links from
// what their VMM offers; consumers choose a Network and, on networks that
// terminate flows in userspace, an Egress.
//
// Like backend, this package holds the contracts and the small adapters
// every implementation needs; the networks themselves live in subpackages
// (vmnet/userspace is the in-process stack, vmnet/egress the standard
// policy), so a consumer that never uses them never links them.
package vmnet

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/netip"
	"strconv"
	"time"

	"github.com/shazow/virtle/internal/dnsproxy"
	"github.com/shazow/virtle/vm"
)

// Link is one L2 attachment: Ethernet frames between a guest NIC and a
// Network. Backends build links from what their VMM offers (for example,
// a QEMU stream socket, see QEMUStream); networks consume them without
// knowing which.
//
// ReadFrame returns exactly one frame per call and io.ErrShortBuffer when a
// frame does not fit p, dropping that frame. MTU is the largest payload the
// link carries; Attach fails when it is below the network's. Close ends
// both directions and unblocks a pending ReadFrame with an error.
type Link interface {
	ReadFrame(p []byte) (int, error)
	WriteFrame(p []byte) error
	MTU() int
	Close() error
}

// Network is a host-side network that guest links attach to. It assigns
// each port a fixed IPv4 address and, unless the caller fixed one, a unique
// MAC; serves DHCP and DNS to the guest; dials guest traffic through its
// Egress; and exposes host->guest forwards. Attach is safe for concurrent
// use. A Network outlives the machines attached to it and is closed by its
// owner. A network may also report MTU() int; QEMU uses a positive result
// for its guest link, otherwise it defaults to an MTU of 1500.
type Network interface {
	Attach(ctx context.Context, link Link, opts AttachOptions) (Port, error)
}

// AttachOptions describe the guest NIC being attached.
type AttachOptions struct {
	// Name is the machine's name, for leases, logs, and audit events.
	Name string
	// MAC fixes the guest's hardware address; nil lets the network allocate
	// one. The backend must give the guest whatever Port.MAC reports.
	MAC net.HardwareAddr
	// Addr fixes the guest's address, so a resumed machine keeps the lease
	// its kernel still holds; the zero value lets the network allocate one.
	// Attach fails when it is outside the network or in use.
	Addr netip.Addr
	// Egress is this guest's policy data; nil means the network's default.
	Egress *vm.Egress
}

// Port is one attached guest NIC. Its address and MAC are fixed at Attach
// (a static DHCP lease keyed by MAC), so a consumer can dial the guest
// before it has booted. Expose rejects a duplicate forward and returns
// net.ErrClosed after Close. Close cancels pending egress dials and closes
// forwarded egress flows before releasing the address, forwards, and link.
// It is idempotent; concurrent calls wait for the same cleanup.
type Port interface {
	Addr() netip.Addr
	MAC() net.HardwareAddr
	Expose(ctx context.Context, f vm.Forward) (io.Closer, error)
	Close() error
}

// Flow identifies one guest-initiated TCP connection or UDP exchange.
type Flow struct {
	Proto vm.Proto       // zero value means TCP, as for vm.Forward
	Src   netip.AddrPort // the guest
	Dst   netip.AddrPort // as the guest addressed it
	Host  string         // the name the guest resolved to Dst, when the network knows it
	Guest string         // AttachOptions.Name of the originating port
	// Egress is the originating port's AttachOptions.Egress: the guest's
	// own policy data for an Egress that honors it. Nil means the network's
	// default.
	Egress *vm.Egress
}

// Network returns the Go network name for the flow's protocol.
func (f Flow) Network() string {
	if f.Proto == vm.UDP {
		return "udp"
	}
	return "tcp"
}

// ErrDenied from Egress.DialFlow refuses the flow before it is accepted: the
// guest sees a reset (TCP) or an unreachable (UDP), never a connection that
// opens and then closes.
var ErrDenied = errors.New("vmnet: egress denied")

// Egress dials guest-initiated flows on a Network that terminates them in
// userspace. The returned conn is spliced to the guest: a passthrough dials
// the destination, an inspecting proxy returns one end of a pipe it serves,
// a redirect dials somewhere else. Any other error also refuses the flow.
// Kernel-backed networks ignore it.
type Egress interface {
	DialFlow(ctx context.Context, f Flow) (net.Conn, error)
}

// DNSAuthorizer is an optional Egress capability that admits a DNS query
// before the network forwards it. Host is the question name; Guest, Src,
// and Egress identify the originating guest. There is no destination
// service port to infer from a DNS query. qtype is the DNS record type.
// A refusal wraps ErrDenied. An Egress without this capability cannot
// authorize forwarded DNS.
type DNSAuthorizer interface {
	AuthorizeDNS(ctx context.Context, f Flow, qtype uint16) error
}

// Passthrough allows everything: it dials the flow's destination by name
// when the network knows the name the guest resolved and by address
// otherwise. Dialer configures those connections; nil uses a zero net.Dialer.
// An explicit Dialer.Resolver overrides outbound name lookup; otherwise the
// network's configured DNS resolver is used when present in the flow context.
// It is the default Egress.
type Passthrough struct{ Dialer *net.Dialer }

// DialFlow implements Egress.
func (p Passthrough) DialFlow(ctx context.Context, f Flow) (net.Conn, error) {
	d := p.Dialer
	if d == nil {
		d = &net.Dialer{}
	}
	if resolver := dnsproxy.FromContext(ctx); f.Host != "" && resolver != nil && d.Resolver == nil {
		// net.Dialer normally budgets lookup and all address attempts
		// together. Preserve that bound when using the network resolver.
		deadline := d.Deadline
		if d.Timeout != 0 {
			if timeout := time.Now().Add(d.Timeout); deadline.IsZero() || timeout.Before(deadline) {
				deadline = timeout
			}
		}
		if !deadline.IsZero() {
			var cancel context.CancelFunc
			ctx, cancel = context.WithDeadline(ctx, deadline)
			defer cancel()
		}
		addrs, err := resolver.LookupNetIP(ctx, "ip", f.Host)
		if err != nil {
			return nil, fmt.Errorf("resolve %s: %w", f.Host, err)
		}
		var firstErr error
		for _, addr := range addrs {
			upstream := netip.AddrPortFrom(addr.Unmap(), f.Dst.Port())
			conn, err := d.DialContext(ctx, f.Network(), upstream.String())
			if err == nil {
				return conn, nil
			}
			if firstErr == nil {
				firstErr = err
			}
		}
		if firstErr == nil {
			firstErr = fmt.Errorf("%s resolves to no address", f.Host)
		}
		return nil, firstErr
	}
	return d.DialContext(ctx, f.Network(), f.Target())
}

// AuthorizeDNS permits every query, as Passthrough permits every flow.
func (Passthrough) AuthorizeDNS(context.Context, Flow, uint16) error { return nil }

// Target is the "host:port" an Egress dials for the flow: the resolved name
// with the destination port when the network knows it, else the address.
func (f Flow) Target() string {
	if f.Host != "" {
		return net.JoinHostPort(f.Host, strconv.Itoa(int(f.Dst.Port())))
	}
	return f.Dst.String()
}

// DenyAll refuses every guest-initiated flow and forwarded DNS query.
type DenyAll struct{}

// DialFlow refuses the flow before connecting.
func (DenyAll) DialFlow(context.Context, Flow) (net.Conn, error) { return nil, ErrDenied }

// AuthorizeDNS refuses the query before contacting an upstream.
func (DenyAll) AuthorizeDNS(context.Context, Flow, uint16) error { return ErrDenied }

var (
	_ Egress        = DenyAll{}
	_ DNSAuthorizer = DenyAll{}
	_ Egress        = Passthrough{}
	_ DNSAuthorizer = Passthrough{}
)
