package userspace

import (
	"fmt"
	"net"
	"testing"
	"testing/synctest"
	"time"

	"gvisor.dev/gvisor/pkg/tcpip"
	"gvisor.dev/gvisor/pkg/tcpip/adapters/gonet"
	"gvisor.dev/gvisor/pkg/tcpip/header"
	"gvisor.dev/gvisor/pkg/tcpip/link/loopback"
	"gvisor.dev/gvisor/pkg/tcpip/network/ipv4"
	"gvisor.dev/gvisor/pkg/tcpip/stack"
	"gvisor.dev/gvisor/pkg/tcpip/transport/udp"
)

const udpActivityInterval = udpIdleTimeout / 3

// A netstack loopback keeps real UDP datagrams entirely in memory, so
// synctest can advance idle deadlines without wall-clock sleeps.
func udpLoopback(t *testing.T) *stack.Stack {
	t.Helper()
	s := stack.New(stack.Options{
		NetworkProtocols:   []stack.NetworkProtocolFactory{ipv4.NewProtocol},
		TransportProtocols: []stack.TransportProtocolFactory{udp.NewProtocol},
	})
	t.Cleanup(s.Destroy)
	if err := s.CreateNIC(nicID, loopback.New()); err != nil {
		t.Fatal(err)
	}
	if err := s.AddProtocolAddress(nicID, tcpip.ProtocolAddress{
		Protocol: ipv4.ProtocolNumber,
		AddressWithPrefix: tcpip.AddressWithPrefix{
			Address: tcpip.AddrFrom4([4]byte{127, 0, 0, 1}), PrefixLen: 8,
		},
	}, stack.AddressProperties{}); err != nil {
		t.Fatal(err)
	}
	s.SetRouteTable([]tcpip.Route{{Destination: header.IPv4EmptySubnet, NIC: nicID}})
	return s
}

func udpListen(t *testing.T, s *stack.Stack) *gonet.UDPConn {
	t.Helper()
	c, err := gonet.DialUDP(s, &tcpip.FullAddress{
		NIC: nicID, Addr: tcpip.AddrFrom4([4]byte{127, 0, 0, 1}),
	}, nil, ipv4.ProtocolNumber)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}

func udpDial(t *testing.T, s *stack.Stack, addr net.Addr) *gonet.UDPConn {
	t.Helper()
	c, err := gonet.DialUDP(s, nil, &tcpip.FullAddress{
		NIC: nicID, Addr: tcpip.AddrFromSlice(addr.(*net.UDPAddr).IP), Port: uint16(addr.(*net.UDPAddr).Port),
	}, ipv4.ProtocolNumber)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}

func readDatagram(t *testing.T, c net.PacketConn, want string) net.Addr {
	t.Helper()
	_ = c.SetReadDeadline(time.Now().Add(testTimeout))
	buf := make([]byte, maxMTU)
	n, from, err := c.ReadFrom(buf)
	if err != nil {
		t.Fatal(err)
	}
	if string(buf[:n]) != want {
		t.Fatalf("datagram = %q, want %q", buf[:n], want)
	}
	return from
}

func TestUDPProxyActivity(t *testing.T) {
	for _, direction := range []string{"host-to-guest", "guest-to-host"} {
		t.Run(direction, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				s := udpLoopback(t)
				pc, guest := udpListen(t, s), udpListen(t, s)
				client := udpDial(t, s, pc.LocalAddr())
				dials := 0
				u := newUDPProxy(pc, func() (net.Conn, error) {
					dials++
					return udpDial(t, s, guest.LocalAddr()), nil
				})
				done := make(chan struct{})
				go func() { defer close(done); u.run() }()
				defer func() { _ = u.Close(); <-done }()
				if _, err := client.Write([]byte("open")); err != nil {
					t.Fatal(err)
				}
				peer := readDatagram(t, guest, "open")
				for i := range 6 {
					// Fake time advances past the original timeout while
					// only one side sends traffic.
					synctest.Wait()
					time.Sleep(udpActivityInterval)
					payload := fmt.Sprintf("packet %d", i)
					if direction == "host-to-guest" {
						if _, err := client.Write([]byte(payload)); err != nil {
							t.Fatal(err)
						}
						if from := readDatagram(t, guest, payload); from.String() != peer.String() {
							t.Fatalf("active peer changed from %s to %s", peer, from)
						}
					} else {
						if _, err := guest.WriteTo([]byte(payload), peer); err != nil {
							t.Fatal(err)
						}
						readDatagram(t, client, payload)
					}
				}
				synctest.Wait()
				if dials != 1 {
					t.Fatalf("active peer dialed %d times, want 1", dials)
				}
				time.Sleep(udpIdleTimeout)
				synctest.Wait()
				if _, err := client.Write([]byte("new flow")); err != nil {
					t.Fatal(err)
				}
				readDatagram(t, guest, "new flow")
				synctest.Wait()
				if dials != 2 {
					t.Fatalf("peer after idle dialed %d times, want 2", dials)
				}
			})
		})
	}
}

func TestUDPProxyPeersExpireIndependently(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s := udpLoopback(t)
		pc, guest := udpListen(t, s), udpListen(t, s)
		active, idle := udpDial(t, s, pc.LocalAddr()), udpDial(t, s, pc.LocalAddr())
		dials := 0
		u := newUDPProxy(pc, func() (net.Conn, error) {
			dials++
			return udpDial(t, s, guest.LocalAddr()), nil
		})
		done := make(chan struct{})
		go func() { defer close(done); u.run() }()
		defer func() { _ = u.Close(); <-done }()
		send := func(c net.Conn, payload string) net.Addr {
			t.Helper()
			if _, err := c.Write([]byte(payload)); err != nil {
				t.Fatal(err)
			}
			return readDatagram(t, guest, payload)
		}
		peer := send(active, "active")
		send(idle, "idle")
		for range 6 {
			synctest.Wait()
			time.Sleep(udpActivityInterval)
			if from := send(active, "active"); from.String() != peer.String() {
				t.Fatalf("active peer changed from %s to %s", peer, from)
			}
		}
		send(idle, "new idle flow")
		synctest.Wait()
		if dials != 3 {
			t.Fatalf("dials = %d, want two initial peers and one renewed idle peer", dials)
		}
		if from := send(active, "still active"); from.String() != peer.String() {
			t.Fatalf("active peer changed from %s to %s", peer, from)
		}
	})
}
