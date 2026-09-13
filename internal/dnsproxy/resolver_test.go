package dnsproxy

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/netip"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"testing/iotest"
	"time"

	"github.com/miekg/dns"
)

const (
	testTimeout      = 5 * time.Second
	testShortTimeout = 100 * time.Millisecond
)

// serve starts real UDP and TCP DNS servers on the same loopback port.
func serve(t *testing.T, handler dns.HandlerFunc) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	pc, err := net.ListenPacket("udp", ln.Addr().String())
	if err != nil {
		_ = ln.Close()
		t.Fatal(err)
	}
	for _, server := range []*dns.Server{{Listener: ln, Handler: handler}, {PacketConn: pc, Handler: handler}} {
		started := make(chan struct{})
		server.NotifyStartedFunc = func() { close(started) }
		done := make(chan error, 1)
		go func() { done <- server.ActivateAndServe() }()
		select {
		case <-started:
		case err := <-done:
			t.Fatalf("start DNS server: %v", err)
		case <-time.After(testTimeout):
			t.Fatal("DNS server did not start")
		}
		t.Cleanup(func() {
			if err := server.Shutdown(); err != nil {
				t.Error(err)
			}
			if err := <-done; err != nil {
				t.Error(err)
			}
		})
	}
	return ln.Addr().String()
}

func resolverAt(t *testing.T, server string) *Resolver {
	t.Helper()
	r, err := New(server)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func record(text string) dns.RR {
	rr, err := dns.NewRR(text)
	if err != nil {
		panic(err)
	}
	return rr
}

func TestReadHost(t *testing.T) {
	r, err := readHost(strings.NewReader("# local configuration\nsearch example.test\nnameserver 127.0.0.53\nnameserver 2001:db8::53 # secondary\nnameserver 127.0.0.53\noptions rotate\n"))
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"127.0.0.53:53", "[2001:db8::53]:53"}; !reflect.DeepEqual(r.servers, want) {
		t.Fatalf("servers = %v, want %v", r.servers, want)
	}
	for _, input := range []string{"", "nameserver", "nameserver resolver.example"} {
		if _, err := readHost(strings.NewReader(input)); err == nil {
			t.Errorf("accepted host configuration %q", input)
		}
	}
	if _, err := readHost(iotest.ErrReader(io.ErrUnexpectedEOF)); !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("reader error = %v", err)
	}
}

func TestExplicitUpstream(t *testing.T) {
	for _, input := range []string{"127.0.0.1:5353", "[2001:0db8::1]:53", "[fe80::1%eth0]:53"} {
		r, err := New(input)
		if err != nil {
			t.Fatal(err)
		}
		want := netip.MustParseAddrPort(input).String()
		if len(r.servers) != 1 || r.servers[0] != want {
			t.Fatalf("explicit resolver = %v, want only %s", r.servers, want)
		}
	}
	for _, input := range []string{"localhost:53", "127.0.0.1", "127.0.0.1:0", "127.0.0.1:65536", "https://127.0.0.1/dns-query"} {
		if _, err := New(input); err == nil {
			t.Errorf("accepted upstream %q", input)
		}
	}
}

func TestQueryNormalizesQuestionAndPreservesReply(t *testing.T) {
	received := make(chan *dns.Msg, 1)
	server := serve(t, func(w dns.ResponseWriter, q *dns.Msg) {
		received <- q.Copy()
		m := new(dns.Msg).SetReply(q)
		m.Authoritative = true
		m.Answer = []dns.RR{record("example.test. 42 IN TXT \"answer\"")}
		m.Ns = []dns.RR{record("example.test. 42 IN NS ns.example.test.")}
		m.Extra = []dns.RR{record("ns.example.test. 42 IN A 192.0.2.53")}
		_ = w.WriteMsg(m)
	})
	response, err := resolverAt(t, server).Query(t.Context(), "EXAMPLE.Test.", dns.TypeTXT)
	if err != nil {
		t.Fatal(err)
	}
	q := <-received
	if q.Question[0] != (dns.Question{Name: "example.test.", Qtype: dns.TypeTXT, Qclass: dns.ClassINET}) || !q.RecursionDesired || q.CheckingDisabled || q.AuthenticatedData || len(q.Answer) != 0 || len(q.Ns) != 0 {
		t.Fatalf("upstream recursive IN query = %s", q)
	}
	if opt := q.IsEdns0(); len(q.Extra) != 1 || opt == nil || opt.UDPSize() != udpSize || opt.Do() || len(opt.Option) != 0 {
		t.Fatalf("upstream options = %v", q.Extra)
	}
	if response.Id != q.Id || !reflect.DeepEqual(response.Question, q.Question) || !response.Authoritative || len(response.Answer) != 1 || len(response.Ns) != 1 || len(response.Extra) != 1 || response.Answer[0].Header().Ttl != 42 {
		t.Fatalf("reply changed: %s", response)
	}
}

func TestQueryKinds(t *testing.T) {
	var calls atomic.Int32
	server := serve(t, func(w dns.ResponseWriter, q *dns.Msg) {
		calls.Add(1)
		_ = w.WriteMsg(new(dns.Msg).SetReply(q))
	})
	r := resolverAt(t, server)
	for _, typ := range []uint16{dns.TypeA, dns.TypeAAAA, dns.TypeCNAME, dns.TypeMX, dns.TypeTXT, dns.TypeSRV, dns.TypeHTTPS, dns.TypeSOA} {
		m, err := r.Query(t.Context(), "Example.Test", typ)
		if err != nil {
			t.Fatalf("type %d: %v", typ, err)
		}
		if want := (dns.Question{Name: "example.test.", Qtype: typ, Qclass: dns.ClassINET}); m.Question[0] != want {
			t.Fatalf("question = %+v, want %+v", m.Question[0], want)
		}
	}
	before := calls.Load()
	for _, typ := range []uint16{0, dns.TypeAXFR, dns.TypeIXFR, dns.TypeANY, dns.TypeOPT, dns.TypeTSIG, dns.TypeTKEY, dns.TypeMAILA, dns.TypeMAILB} {
		if _, err := r.Query(t.Context(), "example.test.", typ); err == nil {
			t.Errorf("accepted meta/transfer type %d", typ)
		}
	}
	for _, name := range []string{"", "bad..test."} {
		if _, err := r.Query(t.Context(), name, dns.TypeA); err == nil {
			t.Errorf("accepted invalid name %q", name)
		}
		if _, err := r.LookupNetIP(t.Context(), "ip", name); err == nil {
			t.Errorf("looked up invalid name %q", name)
		}
	}
	if got := calls.Load() - before; got != 0 {
		t.Errorf("invalid questions reached upstream %d times", got)
	}
}

func TestQueryRetriesTCPTruncation(t *testing.T) {
	transports := make(chan string, 2)
	server := serve(t, func(w dns.ResponseWriter, q *dns.Msg) {
		transports <- w.RemoteAddr().Network()
		m := new(dns.Msg).SetReply(q)
		if w.RemoteAddr().Network() == "udp" {
			m.Truncated = true
		} else {
			m.Answer = []dns.RR{record("example.test. 60 IN A 192.0.2.1")}
		}
		_ = w.WriteMsg(m)
	})
	m, err := resolverAt(t, server).Query(t.Context(), "example.test.", dns.TypeA)
	if err != nil || m.Truncated || len(m.Answer) != 1 {
		t.Fatalf("TCP fallback = %v, %v", m, err)
	}
	if first, second := <-transports, <-transports; first != "udp" || second != "tcp" {
		t.Fatalf("transports = %s, %s", first, second)
	}
}

func TestHostServerFailover(t *testing.T) {
	for _, rcode := range []int{dns.RcodeServerFailure, dns.RcodeNameError, dns.RcodeRefused} {
		t.Run(dns.RcodeToString[rcode], func(t *testing.T) {
			first := serve(t, func(w dns.ResponseWriter, q *dns.Msg) {
				m := new(dns.Msg).SetRcode(q, rcode)
				m.Ns = []dns.RR{record("example.test. 60 IN SOA ns.example.test. admin.example.test. 1 2 3 4 5")}
				_ = w.WriteMsg(m)
			})
			var calls atomic.Int32
			second := serve(t, func(w dns.ResponseWriter, q *dns.Msg) {
				calls.Add(1)
				_ = w.WriteMsg(new(dns.Msg).SetReply(q))
			})
			r := &Resolver{servers: []string{first, second}}
			m, err := r.Query(t.Context(), "example.test.", dns.TypeA)
			if err != nil {
				t.Fatal(err)
			}
			if rcode == dns.RcodeServerFailure {
				if calls.Load() != 1 || m.Rcode != dns.RcodeSuccess {
					t.Fatalf("SERVFAIL did not fail over: %s, calls=%d", m, calls.Load())
				}
			} else if calls.Load() != 0 || m.Rcode != rcode || len(m.Ns) != 1 {
				t.Fatalf("terminal reply changed: %s, calls=%d", m, calls.Load())
			}
		})
	}
}

func TestQueryRejectsMismatchedQuestion(t *testing.T) {
	server := serve(t, func(w dns.ResponseWriter, q *dns.Msg) {
		m := new(dns.Msg).SetReply(q)
		m.Question[0].Name = "other.test."
		_ = w.WriteMsg(m)
	})
	if _, err := resolverAt(t, server).Query(t.Context(), "example.test.", dns.TypeA); err == nil || !strings.Contains(err.Error(), "does not match") {
		t.Fatalf("mismatched reply = %v", err)
	}
}

func TestQueryMatchesID(t *testing.T) {
	for _, network := range []string{"udp", "tcp"} {
		t.Run(network, func(t *testing.T) {
			server := serve(t, func(w dns.ResponseWriter, q *dns.Msg) {
				m := new(dns.Msg).SetReply(q)
				if network == "tcp" && w.RemoteAddr().Network() == "udp" {
					m.Truncated = true
					_ = w.WriteMsg(m)
					return
				}
				m.Id++
				m.Answer = []dns.RR{record("example.test. 60 IN TXT \"wrong-id\"")}
				_ = w.WriteMsg(m)
				if network == "udp" {
					// Unrelated datagrams are ignored until the matching reply.
					m.Id = q.Id
					m.Answer = []dns.RR{record("example.test. 60 IN TXT \"matching-id\"")}
					_ = w.WriteMsg(m)
				}
			})
			m, err := resolverAt(t, server).Query(t.Context(), "example.test.", dns.TypeTXT)
			if network == "tcp" {
				if !errors.Is(err, dns.ErrId) {
					t.Fatalf("mismatched TCP reply = %v, %v; want dns.ErrId", m, err)
				}
			} else if err != nil || len(m.Answer) != 1 || m.Answer[0].(*dns.TXT).Txt[0] != "matching-id" {
				t.Fatalf("reply ID matching = %v, %v", m, err)
			}
		})
	}
}

func TestQueryTransportFailure(t *testing.T) {
	first := serve(t, func(w dns.ResponseWriter, q *dns.Msg) {
		if w.RemoteAddr().Network() == "tcp" {
			_ = w.Close() // terminate the connection before sending a response
			return
		}
		m := new(dns.Msg).SetReply(q)
		m.Truncated = true
		_ = w.WriteMsg(m)
	})
	second := serve(t, func(w dns.ResponseWriter, q *dns.Msg) { _ = w.WriteMsg(new(dns.Msg).SetReply(q)) })
	if _, err := (&Resolver{servers: []string{first, second}}).Query(t.Context(), "example.test.", dns.TypeA); err != nil {
		t.Fatalf("transport failure did not try the next host server: %v", err)
	}
	if _, err := resolverAt(t, first).Query(t.Context(), "example.test.", dns.TypeA); err == nil {
		t.Fatal("explicit upstream failure was hidden")
	}
}

func TestQueryCancellationClosesTCP(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	pc, err := net.ListenPacket("udp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	started := make(chan struct{})
	udp := &dns.Server{PacketConn: pc, NotifyStartedFunc: func() { close(started) }, Handler: dns.HandlerFunc(func(w dns.ResponseWriter, q *dns.Msg) {
		m := new(dns.Msg).SetReply(q)
		m.Truncated = true
		_ = w.WriteMsg(m)
	})}
	served := make(chan error, 1)
	go func() { served <- udp.ActivateAndServe() }()
	<-started
	t.Cleanup(func() { _ = udp.Shutdown(); <-served })
	received, peerClosed := make(chan error, 1), make(chan error, 1)
	go func() {
		c, err := ln.Accept()
		if err != nil {
			received <- err
			return
		}
		defer c.Close()
		_, err = (&dns.Conn{Conn: c}).ReadMsg()
		received <- err
		_, err = c.Read(make([]byte, 1))
		peerClosed <- err
	}()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan error, 1)
	r := resolverAt(t, ln.Addr().String())
	go func() {
		_, err := r.Query(ctx, "example.test.", dns.TypeA)
		done <- err
	}()
	select {
	case err := <-received:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(testTimeout):
		t.Fatal("TCP query never arrived")
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("canceled exchange = %v", err)
		}
	case <-time.After(testTimeout):
		t.Fatal("cancellation did not end exchange")
	}
	select {
	case err := <-peerClosed:
		if !errors.Is(err, io.EOF) {
			t.Fatalf("upstream socket after cancellation = %v", err)
		}
	case <-time.After(testTimeout):
		t.Fatal("cancellation left the upstream socket open")
	}
}

func TestLookupFollowsCNAMEsAndAddressFamilies(t *testing.T) {
	server := serve(t, func(w dns.ResponseWriter, q *dns.Msg) {
		m := new(dns.Msg).SetReply(q)
		switch q.Question[0].Name {
		case "example.test.":
			m.Answer = []dns.RR{record("example.test. 60 IN CNAME middle.test."), record("middle.test. 60 IN CNAME terminal.test."), record("unrelated.test. 60 IN A 192.0.2.99")}
		case "terminal.test.":
			if q.Question[0].Qtype == dns.TypeA {
				m.Answer = []dns.RR{record("terminal.test. 60 IN A 192.0.2.1")}
			} else {
				m.Answer = []dns.RR{record("terminal.test. 60 IN AAAA 2001:db8::1")}
			}
		default:
			m.Rcode = dns.RcodeNameError
		}
		_ = w.WriteMsg(m)
	})
	r := resolverAt(t, server)
	addrs, err := r.LookupNetIP(t.Context(), "ip", "EXAMPLE.test")
	if want := []netip.Addr{netip.MustParseAddr("192.0.2.1"), netip.MustParseAddr("2001:db8::1")}; err != nil || !reflect.DeepEqual(addrs, want) {
		t.Fatalf("LookupNetIP = %v, %v; want %v", addrs, err, want)
	}
	if _, err := r.LookupNetIP(t.Context(), "ip4", "missing.test"); err == nil {
		t.Fatal("NXDOMAIN accepted")
	} else if dnsErr := new(net.DNSError); !errors.As(err, &dnsErr) || !dnsErr.IsNotFound {
		t.Fatalf("NXDOMAIN error = %v", err)
	}
}

func TestLookupBoundsCNAMEChains(t *testing.T) {
	for _, loop := range []bool{true, false} {
		t.Run(fmt.Sprint(loop), func(t *testing.T) {
			var calls atomic.Int32
			server := serve(t, func(w dns.ResponseWriter, q *dns.Msg) {
				i := calls.Add(1)
				target := "start.test."
				if !loop {
					target = fmt.Sprintf("alias%d.test.", i)
				}
				m := new(dns.Msg).SetReply(q)
				m.Answer = []dns.RR{record(q.Question[0].Name + " 60 IN CNAME " + target)}
				_ = w.WriteMsg(m)
			})
			if _, err := resolverAt(t, server).LookupNetIP(t.Context(), "ip4", "start.test"); err == nil || !strings.Contains(err.Error(), "CNAME chain") {
				t.Fatalf("unbounded aliases = %v", err)
			}
			if calls.Load() > maxAliases+1 {
				t.Fatalf("made %d alias queries", calls.Load())
			}
		})
	}
}

func TestLookupKeepsSuccessfulAddressFamily(t *testing.T) {
	server := serve(t, func(w dns.ResponseWriter, q *dns.Msg) {
		if q.Question[0].Qtype == dns.TypeAAAA {
			return // this upstream never answers IPv6 questions
		}
		m := new(dns.Msg).SetReply(q)
		m.Answer = []dns.RR{record("example.test. 60 IN A 192.0.2.1")}
		_ = w.WriteMsg(m)
	})
	ctx, cancel := context.WithTimeout(t.Context(), testShortTimeout)
	defer cancel()
	addrs, err := resolverAt(t, server).LookupNetIP(ctx, "ip", "example.test")
	if err != nil || len(addrs) != 1 || addrs[0].String() != "192.0.2.1" {
		t.Fatalf("partial address-family success = %v, %v", addrs, err)
	}
}

func TestLookupInlineAliasAndNoData(t *testing.T) {
	server := serve(t, func(w dns.ResponseWriter, q *dns.Msg) {
		m := new(dns.Msg).SetReply(q)
		if q.Question[0].Name == "alias.test." {
			m.Answer = []dns.RR{record("alias.test. 60 IN CNAME target.test."), record("target.test. 60 IN A 192.0.2.1")}
		}
		_ = w.WriteMsg(m)
	})
	r := resolverAt(t, server)
	addrs, err := r.LookupNetIP(t.Context(), "ip4", "alias.test")
	if err != nil || len(addrs) != 1 || addrs[0].String() != "192.0.2.1" {
		t.Fatalf("inline alias lookup = %v, %v", addrs, err)
	}
	_, err = r.LookupNetIP(t.Context(), "ip4", "empty.test")
	var dnsErr *net.DNSError
	if !errors.As(err, &dnsErr) || !dnsErr.IsNotFound {
		t.Fatalf("NODATA lookup = %v", err)
	}
}

func TestContextResolverAndLiteralLookup(t *testing.T) {
	r := resolverAt(t, "192.0.2.53:53")
	ctx := WithResolver(t.Context(), r)
	if FromContext(ctx) != r || FromContext(t.Context()) != nil {
		t.Fatal("context lost resolver selection")
	}
	addrs, err := r.LookupNetIP(ctx, "ip4", "192.0.2.1")
	if err != nil || len(addrs) != 1 || addrs[0].String() != "192.0.2.1" {
		t.Fatalf("literal lookup = %v, %v", addrs, err)
	}
	if _, err := r.LookupNetIP(ctx, "udp", "example.test"); err == nil {
		t.Fatal("invalid network accepted")
	}
}

func TestQueryLogsSelectedUpstream(t *testing.T) {
	first := serve(t, func(w dns.ResponseWriter, q *dns.Msg) {
		_ = w.WriteMsg(new(dns.Msg).SetRcode(q, dns.RcodeServerFailure))
	})
	second := serve(t, func(w dns.ResponseWriter, q *dns.Msg) { _ = w.WriteMsg(new(dns.Msg).SetRcode(q, dns.RcodeNameError)) })
	base := &Resolver{servers: []string{first, second}}
	var output bytes.Buffer
	r := base.WithLogger(slog.New(slog.NewJSONHandler(&output, nil)).With("guest", "test-vm"))
	if base.logger != nil {
		t.Fatal("WithLogger changed the original resolver")
	}
	if _, err := r.Query(t.Context(), "Example.TEST.", dns.TypeA); err != nil {
		t.Fatal(err)
	}
	var entry map[string]any
	decoder := json.NewDecoder(&output)
	if err := decoder.Decode(&entry); err != nil {
		t.Fatal(err)
	}
	if entry["msg"] != "dns exchange" || entry["name"] != "example.test." || entry["upstream"] != second || entry["rcode"] != "NXDOMAIN" || entry["guest"] != "test-vm" || entry["duration"] == nil {
		t.Fatalf("exchange log = %v", entry)
	}
	if err := decoder.Decode(&entry); !errors.Is(err, io.EOF) {
		t.Fatalf("more than one log entry: %v", err)
	}
}
