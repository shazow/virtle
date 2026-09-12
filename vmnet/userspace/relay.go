package userspace

import (
	"io"
	"net"
	"sync"
	"time"
)

// splice copies in both directions until both ends are done. A finished
// direction half-closes its destination when it can, so a peer that sends
// EOF and then reads still gets its answer.
func splice(a, b net.Conn) {
	var wg sync.WaitGroup
	closeBoth := sync.OnceFunc(func() {
		_ = a.Close()
		_ = b.Close()
	})
	copy := func(dst, src net.Conn) {
		defer wg.Done()
		_, err := io.Copy(dst, src)
		if err == nil {
			if cw, ok := dst.(interface{ CloseWrite() error }); ok {
				_ = cw.CloseWrite()
				return
			}
		}
		closeBoth()
	}
	wg.Add(2)
	go copy(a, b)
	go copy(b, a)
	wg.Wait()
	closeBoth()
}

// relayDatagrams copies datagrams in both directions until either side
// fails or neither side sends a datagram for udpIdleTimeout.
func relayDatagrams(a, b net.Conn) {
	var wg sync.WaitGroup
	activity := newUDPActivity(a, b)
	closeBoth := sync.OnceFunc(func() {
		_ = a.Close()
		_ = b.Close()
	})
	copy := func(dst, src net.Conn) {
		defer wg.Done()
		defer closeBoth()
		buf := make([]byte, maxMTU)
		for {
			k, err := src.Read(buf)
			if err != nil {
				return
			}
			activity.touch()
			if _, err := dst.Write(buf[:k]); err != nil {
				return
			}
		}
	}
	wg.Add(2)
	go copy(a, b)
	go copy(b, a)
	wg.Wait()
}

// udpActivity gives all readers of a UDP flow the same idle deadline.
// A datagram in either direction keeps the whole flow alive. Serializing
// updates prevents a delayed refresh from replacing a newer deadline.
type udpActivity struct {
	mu    sync.Mutex
	conns []net.Conn
}

func newUDPActivity(conns ...net.Conn) *udpActivity {
	a := &udpActivity{conns: conns}
	a.touch()
	return a
}

func (a *udpActivity) touch() {
	a.mu.Lock()
	defer a.mu.Unlock()
	deadline := time.Now().Add(udpIdleTimeout)
	for _, c := range a.conns {
		_ = c.SetReadDeadline(deadline)
	}
}
