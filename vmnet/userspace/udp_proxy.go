package userspace

import (
	"net"
	"sync"
)

// udpProxy relays datagrams from a host packet conn to per-peer guest
// conns, tracking peers by source address as a NAT would.
type udpProxy struct {
	pc   net.PacketConn
	dial func() (net.Conn, error)

	mu     sync.Mutex
	closed bool
	peers  map[string]*udpPeer
	wg     sync.WaitGroup
}

type udpPeer struct {
	net.Conn
	activity *udpActivity
}

func newUDPProxy(pc net.PacketConn, dial func() (net.Conn, error)) *udpProxy {
	return &udpProxy{pc: pc, dial: dial, peers: make(map[string]*udpPeer)}
}

func (u *udpProxy) run() {
	buf := make([]byte, maxMTU)
	for {
		k, peer, err := u.pc.ReadFrom(buf)
		if err != nil {
			return
		}
		c := u.peer(peer)
		if c == nil {
			continue
		}
		c.activity.touch()
		if _, err := c.Write(buf[:k]); err != nil {
			u.drop(peer.String(), c)
		}
	}
}

// peer returns the guest conn for a host peer, dialing one on first use.
func (u *udpProxy) peer(peer net.Addr) *udpPeer {
	key := peer.String()
	u.mu.Lock()
	defer u.mu.Unlock()
	if u.closed {
		return nil
	}
	if c, ok := u.peers[key]; ok {
		return c
	}
	c, err := u.dial()
	if err != nil {
		return nil
	}
	p := &udpPeer{Conn: c, activity: newUDPActivity(c)}
	u.peers[key] = p
	u.wg.Add(1)
	go u.reply(peer, p)
	return p
}

// reply copies the guest's answers back to the peer until the flow idles.
func (u *udpProxy) reply(peer net.Addr, c *udpPeer) {
	defer u.wg.Done()
	defer u.drop(peer.String(), c)
	buf := make([]byte, maxMTU)
	for {
		k, err := c.Read(buf)
		if err != nil {
			return
		}
		c.activity.touch()
		if _, err := u.pc.WriteTo(buf[:k], peer); err != nil {
			return
		}
	}
}

func (u *udpProxy) drop(key string, c *udpPeer) {
	u.mu.Lock()
	if u.peers[key] == c {
		delete(u.peers, key)
	}
	u.mu.Unlock()
	_ = c.Close()
}

func (u *udpProxy) Close() error {
	u.mu.Lock()
	u.closed = true
	peers := make([]*udpPeer, 0, len(u.peers))
	for _, c := range u.peers {
		peers = append(peers, c)
	}
	u.peers = map[string]*udpPeer{}
	u.mu.Unlock()
	err := u.pc.Close()
	for _, c := range peers {
		_ = c.Close()
	}
	u.wg.Wait()
	return err
}
