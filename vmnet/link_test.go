package vmnet

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"net/netip"
	"strconv"
	"testing"

	"github.com/shazow/virtle/vm"
)

func TestFramedLinksRoundTrip(t *testing.T) {
	a, b := net.Pipe()
	la, lb := QEMUStream(a, 1500), QEMUStream(b, 1500)
	defer la.Close()
	defer lb.Close()
	frames := [][]byte{bytes.Repeat([]byte{0xab}, 60), bytes.Repeat([]byte{0xcd}, 1514), {}}
	go func() {
		for _, f := range frames {
			if err := la.WriteFrame(f); err != nil {
				t.Error(err)
			}
		}
	}()
	buf := make([]byte, 1514)
	for _, want := range frames {
		n, err := lb.ReadFrame(buf)
		if err != nil {
			t.Fatalf("ReadFrame: %v", err)
		}
		if !bytes.Equal(buf[:n], want) {
			t.Fatalf("frame = %d bytes, want %d", n, len(want))
		}
	}
	if err := la.WriteFrame(make([]byte, 1515)); err == nil {
		t.Fatal("a frame above the MTU was written")
	}
}

func TestFramedLinkSkipsFramesThatDoNotFit(t *testing.T) {
	a, b := net.Pipe()
	la, lb := QEMUStream(a, 1500), QEMUStream(b, 1500)
	defer la.Close()
	defer lb.Close()
	go func() {
		_ = la.WriteFrame(bytes.Repeat([]byte{1}, 1000))
		_ = la.WriteFrame([]byte{2, 2})
	}()
	small := make([]byte, 64)
	if _, err := lb.ReadFrame(small); !errors.Is(err, io.ErrShortBuffer) {
		t.Fatalf("ReadFrame into a short buffer = %v, want io.ErrShortBuffer", err)
	}
	n, err := lb.ReadFrame(small)
	if err != nil || n != 2 || small[0] != 2 {
		t.Fatalf("next frame = %v %v; the stream fell out of alignment", small[:n], err)
	}

	// A peer sending beyond the reader's MTU loses those frames, not the
	// link: the reader's buffer is sized for its MTU.
	big, other := net.Pipe()
	lbig, lother := QEMUStream(big, 9000), QEMUStream(other, 1500)
	defer lbig.Close()
	defer lother.Close()
	go func() {
		_ = lbig.WriteFrame(bytes.Repeat([]byte{3}, 2000))
		_ = lbig.WriteFrame([]byte{4})
	}()
	buf := make([]byte, 1500+EthernetHeader)
	if _, err := lother.ReadFrame(buf); !errors.Is(err, io.ErrShortBuffer) {
		t.Fatalf("frame beyond the MTU = %v, want io.ErrShortBuffer", err)
	}
	if n, err := lother.ReadFrame(buf); err != nil || n != 1 || buf[0] != 4 {
		t.Fatalf("frame after the oversized one = %v %v", buf[:n], err)
	}
	// A length no frame can have is a broken stream, which ends the link.
	go func() { _, _ = big.Write([]byte{0x7f, 0xff, 0xff, 0xff}) }()
	if _, err := lother.ReadFrame(buf); !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("impossible frame length = %v, want a framing error", err)
	}
}

func TestPassthroughAndDenyAll(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		c, err := ln.Accept()
		if err == nil {
			_, _ = c.Write([]byte("hi"))
			c.Close()
		}
	}()
	dst := netip.MustParseAddrPort(ln.Addr().String())
	flow := Flow{Dst: dst, Guest: "test"}
	if flow.Network() != "tcp" {
		t.Fatalf("zero Proto network = %q", flow.Network())
	}
	c, err := Passthrough{}.DialFlow(context.Background(), flow)
	if err != nil {
		t.Fatalf("Passthrough: %v", err)
	}
	defer c.Close()
	if got, _ := io.ReadAll(c); string(got) != "hi" {
		t.Fatalf("read %q", got)
	}
	if _, err := (DenyAll{}).DialFlow(context.Background(), flow); !errors.Is(err, ErrDenied) {
		t.Fatalf("DenyAll = %v, want ErrDenied", err)
	}
	if (Flow{Proto: vm.UDP}).Network() != "udp" {
		t.Fatal("UDP flow network")
	}
	// A known name is dialed by name: the address the guest used may be a
	// synthetic one that only the name resolves.
	port := strconv.Itoa(int(dst.Port()))
	named := Flow{Dst: netip.MustParseAddrPort("198.18.0.1:" + port), Host: "localhost"}
	if named.Target() != "localhost:"+port {
		t.Fatalf("Target = %q", named.Target())
	}
}
