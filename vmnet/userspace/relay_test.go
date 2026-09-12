package userspace

import (
	"fmt"
	"testing"
	"testing/synctest"
	"time"
)

func TestRelayDatagramsActivity(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s := udpLoopback(t)
		client, server := udpListen(t, s), udpListen(t, s)
		a, b := udpDial(t, s, client.LocalAddr()), udpDial(t, s, server.LocalAddr())
		done := make(chan struct{})
		go func() { defer close(done); relayDatagrams(a, b) }()
		defer func() { _ = a.Close(); _ = b.Close(); <-done }()
		for _, reverse := range []bool{false, true} {
			src, dst, relay := client, server, a
			if reverse {
				src, dst, relay = server, client, b
			}
			for i := range 6 {
				synctest.Wait()
				time.Sleep(udpActivityInterval)
				payload := fmt.Sprintf("packet %d, reverse %v", i, reverse)
				if _, err := src.WriteTo([]byte(payload), relay.LocalAddr()); err != nil {
					t.Fatal(err)
				}
				readDatagram(t, dst, payload)
			}
		}
		synctest.Wait()
		time.Sleep(udpIdleTimeout)
		<-done
	})
}
