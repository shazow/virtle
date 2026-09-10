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

// maxFrame is the longest frame any link accepts from its peer, whatever its
// MTU: the largest IPv4 datagram behind an Ethernet header. A longer length
// prefix is not a frame but a stream that has lost its framing.
const maxFrame = 65535 + EthernetHeader

// QEMUStream wraps one end of the socket behind a QEMU "-netdev stream"
// device: every frame is prefixed with its length as a 4-byte big-endian
// integer. A frame that does not fit the read buffer is dropped, so a guest
// that sends more than the MTU loses those frames and keeps the link.
func QEMUStream(c net.Conn, mtu int) Link {
	return &framed{
		conn: c, mtu: mtu, header: 4,
		put: func(b []byte, n int) { binary.BigEndian.PutUint32(b, uint32(n)) },
		get: func(b []byte) int { return int(binary.BigEndian.Uint32(b)) },
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
	if n > maxFrame {
		return 0, fmt.Errorf("vmnet: frame length %d is not a frame; the stream has lost its framing: %w", n, io.ErrUnexpectedEOF)
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

var _ Link = (*framed)(nil)
