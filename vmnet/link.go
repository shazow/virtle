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

// maxFrame is the longest frame any link accepts from its peer, whatever its
// MTU: the largest IPv4 datagram behind an Ethernet header. A longer length
// prefix is not a frame but a stream that has lost its framing.
const maxFrame = 65535 + EthernetHeader

// QEMUStream wraps one end of the socket behind a QEMU "-netdev stream"
// device: every frame is prefixed with its length as a 4-byte big-endian
// integer. A frame that does not fit the read buffer is dropped, so a guest
// that sends more than the MTU loses those frames and keeps the link.
func QEMUStream(c net.Conn, mtu int) Link {
	return &qemuStream{conn: c, mtu: mtu}
}

type qemuStream struct {
	conn net.Conn
	mtu  int

	readMu  sync.Mutex
	hdr     [4]byte
	writeMu sync.Mutex
	wbuf    []byte
}

func (l *qemuStream) MTU() int { return l.mtu }

func (l *qemuStream) ReadFrame(p []byte) (int, error) {
	l.readMu.Lock()
	defer l.readMu.Unlock()
	if _, err := io.ReadFull(l.conn, l.hdr[:]); err != nil {
		return 0, err
	}
	n := int(binary.BigEndian.Uint32(l.hdr[:]))
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

func (l *qemuStream) WriteFrame(p []byte) error {
	if len(p) > l.mtu+EthernetHeader {
		return fmt.Errorf("vmnet: frame of %d bytes exceeds the link MTU %d", len(p), l.mtu)
	}
	l.writeMu.Lock()
	defer l.writeMu.Unlock()
	// One write per frame keeps the header and payload contiguous for the
	// peer, which reads them with a single length-prefixed read.
	l.wbuf = binary.BigEndian.AppendUint32(l.wbuf[:0], uint32(len(p)))
	l.wbuf = append(l.wbuf, p...)
	_, err := l.conn.Write(l.wbuf)
	return err
}

func (l *qemuStream) Close() error { return l.conn.Close() }

var _ Link = (*qemuStream)(nil)
