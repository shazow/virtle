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
	"io"
	"net"
	"net/netip"

	"github.com/shazow/virtle/vm"
)

// Link is one L2 attachment: Ethernet frames between a guest NIC and a
// Network. Backends build links from what their VMM offers (a QEMU stream
// socket, a guest agent's vsock tunnel); networks consume them without
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
// owner.
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
	// Forwards are host->guest forwards exposed for the port's lifetime.
	Forwards []vm.Forward
	// Egress is this guest's policy data; nil means the network's default.
	Egress *vm.Egress
}

// Port is one attached guest NIC. Its address and MAC are fixed at Attach
// (a static DHCP lease keyed by MAC), so a consumer can dial the guest
// before it has booted. Expose rejects a duplicate forward and returns
// net.ErrClosed after Close; Close is idempotent and releases the address,
// the forwards, and the link.
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

// Passthrough allows everything: it dials the flow's destination with the
// Dialer (a zero Dialer when nil). It is the default Egress.
type Passthrough struct{ Dialer *net.Dialer }

// DialFlow implements Egress.
func (p Passthrough) DialFlow(ctx context.Context, f Flow) (net.Conn, error) {
	d := p.Dialer
	if d == nil {
		d = &net.Dialer{}
	}
	return d.DialContext(ctx, f.Network(), f.Dst.String())
}

// DenyAll refuses every guest-initiated flow.
type DenyAll struct{}

// DialFlow implements Egress.
func (DenyAll) DialFlow(context.Context, Flow) (net.Conn, error) { return nil, ErrDenied }

var (
	_ Egress = Passthrough{}
	_ Egress = DenyAll{}
)
