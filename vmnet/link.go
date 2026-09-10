package vmnet

import (
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"sync"
)

// EthernetHeader is the size of an Ethernet frame header; a link for MTU m
// carries frames of up to m+EthernetHeader bytes.
const EthernetHeader = 14

// frameSize is the buffer a link needs for one frame at the given MTU.
func frameSize(mtu int) int { return mtu + EthernetHeader }

// QEMUStream wraps one end of the socket behind a QEMU "-netdev stream"
// device: every frame is prefixed with its length as a 4-byte big-endian
// integer. Frames larger than mtu+EthernetHeader are dropped on read.
func QEMUStream(c net.Conn, mtu int) Link {
	return &framed{
		conn: c, mtu: mtu, header: 4,
		put: func(b []byte, n int) { binary.BigEndian.PutUint32(b, uint32(n)) },
		get: func(b []byte) int { return int(binary.BigEndian.Uint32(b)) },
	}
}

// Tunnel wraps a guest agent's frame tunnel (over vsock or any stream):
// every frame is prefixed with its length as a 2-byte little-endian integer,
// the framing gvisor-tap-vsock's forwarder speaks.
func Tunnel(c net.Conn, mtu int) Link {
	return &framed{
		conn: c, mtu: mtu, header: 2,
		put: func(b []byte, n int) { binary.LittleEndian.PutUint16(b, uint16(n)) },
		get: func(b []byte) int { return int(binary.LittleEndian.Uint16(b)) },
	}
}

// framed carries length-prefixed frames over a stream connection.
type framed struct {
	conn   net.Conn
	mtu    int
	header int
	put    func(b []byte, n int)
	get    func(b []byte) int

	readMu  sync.Mutex
	hdr     [4]byte
	writeMu sync.Mutex
	wbuf    []byte
}

func (l *framed) MTU() int { return l.mtu }

func (l *framed) ReadFrame(p []byte) (int, error) {
	l.readMu.Lock()
	defer l.readMu.Unlock()
	if _, err := io.ReadFull(l.conn, l.hdr[:l.header]); err != nil {
		return 0, err
	}
	n := l.get(l.hdr[:l.header])
	if n > frameSize(l.mtu) {
		return 0, fmt.Errorf("vmnet: frame of %d bytes exceeds the link MTU %d: %w", n, l.mtu, io.ErrUnexpectedEOF)
	}
	if n > len(p) {
		// Keep the stream aligned: consume the frame we cannot deliver.
		if _, err := io.CopyN(io.Discard, l.conn, int64(n)); err != nil {
			return 0, err
		}
		return 0, io.ErrShortBuffer
	}
	if _, err := io.ReadFull(l.conn, p[:n]); err != nil {
		return 0, err
	}
	return n, nil
}

func (l *framed) WriteFrame(p []byte) error {
	if len(p) > frameSize(l.mtu) {
		return fmt.Errorf("vmnet: frame of %d bytes exceeds the link MTU %d", len(p), l.mtu)
	}
	l.writeMu.Lock()
	defer l.writeMu.Unlock()
	// One write per frame keeps the header and payload contiguous for the
	// peer, which reads them with a single length-prefixed read.
	l.wbuf = append(l.wbuf[:0], make([]byte, l.header)...)
	l.put(l.wbuf, len(p))
	l.wbuf = append(l.wbuf, p...)
	_, err := l.conn.Write(l.wbuf)
	return err
}

func (l *framed) Close() error { return l.conn.Close() }

// Deferred is a Link whose peer arrives after Attach: a backend attaches it
// at Start so the port's address and MAC are known before the guest boots,
// then binds the real link once the guest agent connects. Reads block until
// then; writes before then are dropped, as a NIC with no cable would.
type Deferred struct {
	mtu int

	mu     sync.Mutex
	cond   *sync.Cond
	link   Link
	closed bool
}

// NewDeferred returns an unbound link for the given MTU.
func NewDeferred(mtu int) *Deferred {
	d := &Deferred{mtu: mtu}
	d.cond = sync.NewCond(&d.mu)
	return d
}

// Bind connects the peer. It fails once bound or closed.
func (d *Deferred) Bind(link Link) error {
	if link.MTU() < d.mtu {
		return fmt.Errorf("vmnet: bound link MTU %d is below the deferred link's %d", link.MTU(), d.mtu)
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	switch {
	case d.closed:
		return net.ErrClosed
	case d.link != nil:
		return fmt.Errorf("vmnet: deferred link is already bound")
	}
	d.link = link
	d.cond.Broadcast()
	return nil
}

// Bound reports whether a peer is connected.
func (d *Deferred) Bound() bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.link != nil
}

func (d *Deferred) MTU() int { return d.mtu }

func (d *Deferred) ReadFrame(p []byte) (int, error) {
	d.mu.Lock()
	for d.link == nil && !d.closed {
		d.cond.Wait()
	}
	link, closed := d.link, d.closed
	d.mu.Unlock()
	if closed {
		return 0, net.ErrClosed
	}
	return link.ReadFrame(p)
}

func (d *Deferred) WriteFrame(p []byte) error {
	d.mu.Lock()
	link, closed := d.link, d.closed
	d.mu.Unlock()
	switch {
	case closed:
		return net.ErrClosed
	case link == nil:
		return nil // no peer yet: dropped
	}
	return link.WriteFrame(p)
}

// Close ends the link, closing the bound peer if any, and unblocks readers.
func (d *Deferred) Close() error {
	d.mu.Lock()
	if d.closed {
		d.mu.Unlock()
		return nil
	}
	d.closed = true
	link := d.link
	d.cond.Broadcast()
	d.mu.Unlock()
	if link != nil {
		return link.Close()
	}
	return nil
}

var (
	_ Link = (*framed)(nil)
	_ Link = (*Deferred)(nil)
)
