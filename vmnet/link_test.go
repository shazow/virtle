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
	"time"

	"github.com/shazow/virtle/vm"
)

func TestFramedLinksRoundTrip(t *testing.T) {
	for _, tc := range []struct {
		name string
		wrap func(net.Conn, int) Link
	}{
		{"qemu stream", QEMUStream},
		{"tunnel", Tunnel},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a, b := net.Pipe()
			la, lb := tc.wrap(a, 1500), tc.wrap(b, 1500)
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
		})
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
}

func TestDeferredLinkWaitsForItsPeer(t *testing.T) {
	d := NewDeferred(1500)
	if err := d.WriteFrame([]byte{1}); err != nil {
		t.Fatalf("write before bind = %v, want a dropped frame", err)
	}
	read := make(chan error, 1)
	go func() {
		buf := make([]byte, 1514)
		n, err := d.ReadFrame(buf)
		if err == nil && (n != 3 || buf[0] != 9) {
			err = errors.New("wrong frame")
		}
		read <- err
	}()
	select {
	case err := <-read:
		t.Fatalf("ReadFrame returned before Bind: %v", err)
	case <-time.After(50 * time.Millisecond):
	}
	a, b := net.Pipe()
	if err := d.Bind(Tunnel(a, 1500)); err != nil {
		t.Fatal(err)
	}
	if err := d.Bind(Tunnel(a, 1500)); err == nil {
		t.Fatal("second Bind succeeded")
	}
	go func() { _ = Tunnel(b, 1500).WriteFrame([]byte{9, 9, 9}) }()
	if err := <-read; err != nil {
		t.Fatalf("ReadFrame after Bind: %v", err)
	}
	if !d.Bound() {
		t.Fatal("Bound = false after Bind")
	}
	if err := d.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := d.ReadFrame(make([]byte, 16)); !errors.Is(err, net.ErrClosed) {
		t.Fatalf("ReadFrame after Close = %v, want net.ErrClosed", err)
	}
	if err := d.Bind(Tunnel(b, 1500)); !errors.Is(err, net.ErrClosed) {
		t.Fatalf("Bind after Close = %v, want net.ErrClosed", err)
	}
}

func TestDeferredCloseUnblocksReaders(t *testing.T) {
	d := NewDeferred(1500)
	done := make(chan error, 1)
	go func() {
		_, err := d.ReadFrame(make([]byte, 16))
		done <- err
	}()
	time.Sleep(20 * time.Millisecond)
	_ = d.Close()
	select {
	case err := <-done:
		if !errors.Is(err, net.ErrClosed) {
			t.Fatalf("blocked read ended with %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Close did not unblock ReadFrame")
	}
	if err := d.Bind(NewDeferred(100)); err == nil {
		t.Fatal("bound a link with a smaller MTU")
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
