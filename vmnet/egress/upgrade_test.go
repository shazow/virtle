package egress

import (
	"bufio"
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"
)

func TestInspectedProtocolUpgradeLifetime(t *testing.T) {
	for _, secure := range []bool{false, true} {
		for _, ending := range []string{"guest closes", "upstream closes", "context canceled"} {
			t.Run(fmt.Sprintf("TLS=%t/%s", secure, ending), func(t *testing.T) {
				upstreamConn := make(chan net.Conn, 1)
				upstreamDone := make(chan struct{})
				authorization := make(chan string, 1)
				handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					defer close(upstreamDone)
					authorization <- r.Header.Get("Authorization")
					conn, rw, err := w.(http.Hijacker).Hijack()
					if err != nil {
						t.Error(err)
						return
					}
					defer conn.Close()
					upstreamConn <- conn
					_, _ = io.WriteString(rw, "HTTP/1.1 101 Switching Protocols\r\nConnection: Upgrade\r\nUpgrade: test-echo\r\n\r\n")
					if err := rw.Flush(); err != nil {
						t.Error(err)
						return
					}
					_, _ = io.Copy(conn, rw)
				})
				var upstream *httptest.Server
				if secure {
					upstream = httptest.NewTLSServer(handler)
				} else {
					upstream = httptest.NewServer(handler)
				}
				defer upstream.Close()
				rec := &events{}
				p := inspectingPolicy(t, upstream, rec, Injection{Name: "TOKEN", Token: "placeholder", Hosts: []string{"api.test"}, In: []Placement{InHeader}, Value: constValue("real-secret")})
				port, _ := strconv.Atoi(upstreamPort(upstream))
				ctx, cancel := context.WithCancel(t.Context())
				defer cancel()
				guest, server := net.Pipe()
				defer guest.Close()
				done := make(chan struct{})
				go func() { p.serveInspected(ctx, server, namedFlow("api.test", uint16(port)), "*.test"); close(done) }()
				var client net.Conn = guest
				if secure {
					roots := x509.NewCertPool()
					roots.AppendCertsFromPEM(p.CAPEM())
					client = tls.Client(client, &tls.Config{RootCAs: roots, ServerName: "api.test", NextProtos: []string{"http/1.1"}})
				}
				_ = client.SetDeadline(time.Now().Add(5 * time.Second))
				if _, err := fmt.Fprintf(client, "GET /stream HTTP/1.1\r\nHost: api.test:%d\r\nConnection: Upgrade\r\nUpgrade: test-echo\r\nAuthorization: Bearer placeholder\r\n\r\n", port); err != nil {
					t.Fatal(err)
				}
				reader := bufio.NewReader(client)
				resp, err := http.ReadResponse(reader, nil)
				if err != nil {
					t.Fatal(err)
				}
				if resp.StatusCode != http.StatusSwitchingProtocols {
					t.Fatalf("status %d", resp.StatusCode)
				}
				for _, message := range []string{"first frame", "second frame"} {
					if _, err := io.WriteString(client, message); err != nil {
						t.Fatal(err)
					}
					buf := make([]byte, len(message))
					if _, err := io.ReadFull(reader, buf); err != nil || string(buf) != message {
						t.Fatalf("upgraded echo = %q, %v", buf, err)
					}
				}
				if got := <-authorization; got != "Bearer real-secret" {
					t.Fatalf("upgrade authorization = %q", got)
				}
				if ev := rec.last(t); ev.Status != 101 || len(ev.Injections) != 1 || ev.Injections[0] != "TOKEN" {
					t.Fatalf("upgrade event = %+v", ev)
				}
				remote := <-upstreamConn
				switch ending {
				case "guest closes":
					_ = guest.Close()
				case "upstream closes":
					_ = remote.Close()
				case "context canceled":
					cancel()
				}
				if ending == "upstream closes" {
					// TLS half-close sends close_notify before closing the
					// relay. Consume it just as a stream client reads EOF.
					if _, err := io.Copy(io.Discard, reader); err != nil {
						t.Fatalf("read upgraded stream close: %v", err)
					}
					_ = guest.Close()
				}
				deadline, stop := context.WithTimeout(t.Context(), 5*time.Second)
				defer stop()
				for _, closed := range []<-chan struct{}{done, upstreamDone} {
					select {
					case <-closed:
					case <-deadline.Done():
						t.Fatal("upgraded stream did not finish")
					}
				}
			})
		}
	}
}
