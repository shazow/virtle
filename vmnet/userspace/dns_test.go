package userspace

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net"
	"net/netip"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/miekg/dns"

	"github.com/shazow/virtle/vm"
	"github.com/shazow/virtle/vmnet"
	"github.com/shazow/virtle/vmnet/egress"
)

func dnsUpstream(t *testing.T, handler dns.HandlerFunc) string {
	t.Helper()
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ready, done := make(chan struct{}), make(chan error, 1)
	server := &dns.Server{PacketConn: pc, Handler: handler, NotifyStartedFunc: func() { close(ready) }}
	go func() { done <- server.ActivateAndServe() }()
	select {
	case <-ready:
	case err := <-done:
		t.Fatalf("start DNS upstream: %v", err)
	}
	t.Cleanup(func() {
		_ = server.Shutdown()
		if err := <-done; err != nil {
			t.Errorf("DNS upstream: %v", err)
		}
	})
	return pc.LocalAddr().String()
}

func addressDNS(w dns.ResponseWriter, r *dns.Msg) {
	m := new(dns.Msg)
	m.SetReply(r)
	if q := r.Question[0]; q.Qtype == dns.TypeA {
		m.Answer = []dns.RR{&dns.A{Hdr: dns.RR_Header{Name: q.Name, Rrtype: dns.TypeA, Class: dns.ClassINET, Ttl: 60}, A: net.IPv4(127, 0, 0, 1)}}
	}
	_ = w.WriteMsg(m)
}

func queryDNS(t *testing.T, g *guest, network, name string, qtype uint16) *dns.Msg {
	t.Helper()
	return exchangeDNS(t, g, network, new(dns.Msg).SetQuestion(dns.Fqdn(name), qtype))
}

func exchangeDNS(t *testing.T, g *guest, network string, m *dns.Msg) *dns.Msg {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), testTimeout)
	defer cancel()
	addr := netip.AddrPortFrom(g.n.gateway, dnsPort)
	var c net.Conn
	var err error
	if network == "tcp" {
		c, err = g.dialTCP(ctx, addr)
	} else {
		c, err = g.dialUDP(addr)
	}
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	r, _, err := (&dns.Client{Timeout: testTimeout}).ExchangeWithConnContext(ctx, m, &dns.Conn{Conn: c})
	if err != nil {
		t.Fatalf("query %v over %s: %v", m.Question, network, err)
	}
	return r
}

func TestDNSResponseUsesGuestEDNS(t *testing.T) {
	upstream := dnsUpstream(t, func(w dns.ResponseWriter, r *dns.Msg) {
		m := new(dns.Msg).SetReply(r)
		m.SetEdns0(4096, false)
		m.IsEdns0().Option = []dns.EDNS0{&dns.EDNS0_LOCAL{Code: 65001, Data: []byte("upstream-only")}}
		if r.Question[0].Name == "extended.test." {
			m.Rcode = dns.RcodeBadVers
		} else {
			m.Answer = []dns.RR{&dns.TXT{Hdr: dns.RR_Header{Name: r.Question[0].Name, Rrtype: dns.TypeTXT, Class: dns.ClassINET}, Txt: []string{"answer"}}}
			m.Extra = append(m.Extra, &dns.A{Hdr: dns.RR_Header{Name: "extra.test.", Rrtype: dns.TypeA, Class: dns.ClassINET}, A: net.IPv4(192, 0, 2, 1)})
		}
		_ = w.WriteMsg(m)
	})
	n := newTestNetwork(t, Config{DNSUpstream: upstream})
	g := attachGuest(t, n, "guest", vmnet.AttachOptions{})
	for _, edns := range []bool{false, true} {
		for _, name := range []string{"records.test.", "extended.test."} {
			q := new(dns.Msg).SetQuestion(name, dns.TypeTXT)
			if edns {
				q.SetEdns0(4096, false)
			}
			r := exchangeDNS(t, g, "udp", q)
			opts, addresses := 0, 0
			for _, rr := range r.Extra {
				switch rr := rr.(type) {
				case *dns.OPT:
					opts++
					if len(rr.Option) != 0 || int(rr.UDPSize()) != n.MTU()-28 {
						t.Fatalf("gateway OPT = %v", rr)
					}
				case *dns.A:
					addresses++
				}
			}
			wantOpts := 0
			if edns {
				wantOpts = 1
			}
			if opts != wantOpts {
				t.Fatalf("EDNS=%t: OPT count = %d, want %d", edns, opts, wantOpts)
			}
			if name == "records.test." {
				if r.Rcode != dns.RcodeSuccess || len(r.Answer) != 1 || addresses != 1 {
					t.Fatalf("ordinary records were not preserved: %v", r)
				}
			} else {
				want := dns.RcodeServerFailure
				if edns {
					want = dns.RcodeBadVers
				}
				if r.Rcode != want {
					t.Fatalf("EDNS=%t: extended RCODE = %d, want %d", edns, r.Rcode, want)
				}
			}
		}
	}
}

func TestDNSLargeRepliesUseTCP(t *testing.T) {
	const records = 8
	upstream := dnsUpstream(t, func(w dns.ResponseWriter, r *dns.Msg) {
		m := new(dns.Msg).SetReply(r)
		for range records {
			m.Answer = append(m.Answer, &dns.TXT{Hdr: dns.RR_Header{Name: r.Question[0].Name, Rrtype: dns.TypeTXT, Class: dns.ClassINET}, Txt: []string{strings.Repeat("a", 100)}})
		}
		_ = w.WriteMsg(m)
	})
	n := newTestNetwork(t, Config{DNSUpstream: upstream, MTU: minMTU})
	g := attachGuest(t, n, "guest", vmnet.AttachOptions{})
	q := new(dns.Msg).SetQuestion("large.test.", dns.TypeTXT).SetEdns0(4096, false)
	r := exchangeDNS(t, g, "udp", q)
	if !r.Truncated || r.Len() > n.MTU()-28 {
		t.Fatalf("UDP reply must fit one frame and request TCP fallback: %v", r)
	}
	r = exchangeDNS(t, g, "tcp", q)
	if r.Truncated || len(r.Answer) != records {
		t.Fatalf("TCP reply has %d records, truncated=%t", len(r.Answer), r.Truncated)
	}
}

type dnsLog struct {
	mu sync.Mutex
	bytes.Buffer
}

func (l *dnsLog) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.Buffer.Write(p)
}

func (l *dnsLog) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.Buffer.String()
}

func TestDNSProxyPreservesNamesAndRecords(t *testing.T) {
	records := map[uint16]string{
		dns.TypeTXT: "example.test. 123 IN TXT \"one\" \"two\"",
		dns.TypeMX:  "example.test. 123 IN MX 10 mail.test.",
		dns.TypeNS:  "example.test. 123 IN NS ns.test.",
		dns.TypeSRV: "example.test. 123 IN SRV 1 2 443 service.test.",
		dns.TypePTR: "7.2.0.192.in-addr.arpa. 123 IN PTR example.test.",
	}
	var requests atomic.Int64
	upstream := dnsUpstream(t, func(w dns.ResponseWriter, r *dns.Msg) {
		requests.Add(1)
		m := new(dns.Msg).SetReply(r)
		q := r.Question[0]
		switch q.Name {
		case ".":
			m.Answer = []dns.RR{&dns.NS{Hdr: dns.RR_Header{Name: ".", Rrtype: dns.TypeNS, Class: dns.ClassINET}, Ns: "ns.test."}}
		case "alias.test.":
			m.Answer = []dns.RR{&dns.CNAME{Hdr: dns.RR_Header{Name: q.Name, Rrtype: dns.TypeCNAME, Class: dns.ClassINET}, Target: "example.test."}}
		case "missing.test.":
			m.Rcode = dns.RcodeNameError
		case "empty.test.":
		default:
			if q.Qtype == dns.TypeA {
				addressDNS(w, r)
				return
			}
			if text, ok := records[q.Qtype]; ok {
				rr, err := dns.NewRR(text)
				if err != nil {
					t.Error(err)
					return
				}
				m.Answer = []dns.RR{rr}
			}
		}
		_ = w.WriteMsg(m)
	})
	for _, mode := range []DNSMode{"", DNSForward, DNSFakeIP} {
		t.Run(string(mode), func(t *testing.T) {
			log := new(dnsLog)
			n := newTestNetwork(t, Config{DNS: mode, DNSUpstream: upstream, Egress: &egress.Policy{Reach: egress.ReachAll}, Logger: slog.New(slog.NewJSONHandler(log, nil))})
			wantMode := mode
			if wantMode == "" {
				wantMode = DNSForward
			}
			if n.DNS() != wantMode {
				t.Fatalf("DNS = %s, want %s", n.DNS(), wantMode)
			}
			g := attachGuest(t, n, "guest", vmnet.AttachOptions{})
			for _, network := range []string{"udp", "tcp"} {
				t.Run(network, func(t *testing.T) {
					r := queryDNS(t, g, network, "example.test", dns.TypeA)
					if r.Rcode != dns.RcodeSuccess || len(r.Answer) != 1 {
						t.Fatalf("A reply = %v", r)
					}
					a := r.Answer[0].(*dns.A).A.String()
					if mode == DNSFakeIP {
						if !n.fakeIPs.contains(netip.MustParseAddr(a)) || r.AuthenticatedData {
							t.Fatalf("A reply = %v; want an unsigned synthetic address", r)
						}
						reverse, _ := dns.ReverseAddr(a)
						ptr := queryDNS(t, g, network, reverse, dns.TypePTR)
						if len(ptr.Answer) != 1 || ptr.Answer[0].(*dns.PTR).Ptr != "example.test." {
							t.Fatalf("PTR reply = %v", ptr)
						}
					} else if a != "127.0.0.1" {
						t.Fatalf("forwarded A = %s, want upstream address", a)
					}
					before := requests.Load()
					if r := queryDNS(t, g, network, "example.test", dns.TypeAAAA); r.Rcode != dns.RcodeSuccess || len(r.Answer) != 0 {
						t.Fatalf("AAAA reply = %v", r)
					}
					for _, local := range []netip.Addr{n.Gateway(), g.addr} {
						reverse, _ := dns.ReverseAddr(local.String())
						if r := queryDNS(t, g, network, reverse, dns.TypePTR); r.Rcode != dns.RcodeNameError {
							t.Fatalf("local PTR %s = %v", local, r)
						}
					}
					if requests.Load() != before {
						t.Error("local AAAA or PTR answer contacted upstream")
					}
					for typ, text := range records {
						want, _ := dns.NewRR(text)
						r := queryDNS(t, g, network, want.Header().Name, typ)
						if r.Rcode != dns.RcodeSuccess || len(r.Answer) != 1 || r.Answer[0].String() != want.String() {
							t.Fatalf("%s reply = %v, want %s", dns.Type(typ), r, want)
						}
					}
					if r := queryDNS(t, g, network, "missing.test", dns.TypeA); r.Rcode != dns.RcodeNameError {
						t.Fatalf("missing name = %v", r)
					}
					if r := queryDNS(t, g, network, "empty.test", dns.TypeA); r.Rcode != dns.RcodeSuccess || len(r.Answer) != 0 {
						t.Fatalf("NODATA = %v", r)
					}
					if r := queryDNS(t, g, network, ".", dns.TypeNS); len(r.Answer) != 1 || r.Answer[0].(*dns.NS).Ns != "ns.test." {
						t.Fatalf("root NS reply = %v", r)
					}
					r = queryDNS(t, g, network, "alias.test", dns.TypeCNAME)
					if len(r.Answer) != 1 || r.Answer[0].(*dns.CNAME).Target != "example.test." {
						t.Fatalf("CNAME reply = %v", r)
					}
					r = queryDNS(t, g, network, "alias.test", dns.TypeA)
					if len(r.Answer) != 1 {
						t.Fatalf("alias A reply = %v", r)
					}
					if mode == DNSFakeIP {
						alias := netip.MustParseAddr(r.Answer[0].(*dns.A).A.String())
						if name, ok := n.fakeIPs.name(alias); !ok || name != "alias.test" {
							t.Fatalf("alias address %s maps to %q, want original name", alias, name)
						}
					} else if alias, ok := r.Answer[0].(*dns.CNAME); !ok || alias.Target != "example.test." {
						t.Fatalf("forwarded alias = %v", r)
					}
				})
			}
			found := false
			for _, line := range strings.Split(strings.TrimSpace(log.String()), "\n") {
				var entry map[string]any
				if err := json.Unmarshal([]byte(line), &entry); err != nil {
					t.Fatal(err)
				}
				if entry["msg"] == "dns query" && entry["type"] == "TXT" {
					found = true
					if entry["guest"] != "guest" || entry["name"] != "example.test" || entry["decision"] != "allow" || entry["upstream"] != upstream || entry["rcode"] != "NOERROR" {
						t.Errorf("query log = %v", entry)
					}
				}
			}
			if !found {
				t.Fatal("no TXT query recorded")
			}
		})
	}
}

func TestDNSAuthorizationPrecedesUpstream(t *testing.T) {
	var requests atomic.Int64
	upstream := dnsUpstream(t, func(w dns.ResponseWriter, r *dns.Msg) {
		requests.Add(1)
		addressDNS(w, r)
	})
	for name, tc := range map[string]struct {
		egress vmnet.Egress
		guest  *vm.Egress
	}{
		"deny all":           {egress: &egress.Policy{}},
		"hostname denied":    {egress: &egress.Policy{Rules: []egress.Rule{{Hosts: []string{"allowed.test"}}}}},
		"custom without DNS": {egress: &recordingEgress{}},
		"guest denied":       {egress: &egress.Policy{Reach: egress.ReachInternet}, guest: &vm.Egress{}},
	} {
		for _, mode := range []DNSMode{DNSForward, DNSFakeIP} {
			t.Run(name+"/"+string(mode), func(t *testing.T) {
				n := newTestNetwork(t, Config{DNS: mode, DNSUpstream: upstream, Egress: tc.egress})
				g := attachGuest(t, n, "guest", vmnet.AttachOptions{Egress: tc.guest})
				for _, network := range []string{"udp", "tcp"} {
					for _, qtype := range []uint16{dns.TypeA, dns.TypeTXT, dns.TypeMX, dns.TypeSRV} {
						if r := queryDNS(t, g, network, "blocked.test", qtype); r.Rcode != dns.RcodeRefused {
							t.Fatalf("%s reply = %v", dns.Type(qtype), r)
						}
					}
				}
			})
		}
	}
	if requests.Load() != 0 {
		t.Fatalf("denied queries reached upstream %d times", requests.Load())
	}
}

func TestNetworkCloseCancelsDNS(t *testing.T) {
	started, release := make(chan struct{}), make(chan struct{})
	upstream := dnsUpstream(t, func(w dns.ResponseWriter, r *dns.Msg) {
		close(started)
		<-release
		addressDNS(w, r)
	})
	defer close(release)
	n := newTestNetwork(t, Config{DNSUpstream: upstream})
	g := attachGuest(t, n, "guest", vmnet.AttachOptions{})
	u, err := g.dialUDP(netip.AddrPortFrom(n.gateway, dnsPort))
	if err != nil {
		t.Fatal(err)
	}
	defer u.Close()
	m := new(dns.Msg)
	m.SetQuestion("example.test.", dns.TypeA)
	if err := (&dns.Conn{Conn: u}).WriteMsg(m); err != nil {
		t.Fatal(err)
	}
	select {
	case <-started:
	case <-time.After(testTimeout):
		t.Fatal("upstream did not receive query")
	}
	done := make(chan error, 1)
	go func() { done <- n.Close() }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(testTimeout):
		t.Fatal("network close waited for the upstream reply")
	}
}
