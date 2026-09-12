package egress

import (
	"context"
	"errors"
	"net"
	"net/netip"
	"sync/atomic"
	"testing"
	"time"

	"github.com/miekg/dns"
	"github.com/shazow/virtle/internal/dnsproxy"
	"github.com/shazow/virtle/vm"
	"github.com/shazow/virtle/vmnet"
)

func TestDNSAuthorization(t *testing.T) {
	nameRule := []Rule{{Hosts: []string{"*.example.test"}, Ports: []int{443}, Inspect: true}}
	guestAllow := []vm.Reach{{Host: "api.example.test", Ports: []int{443}}}
	for _, tc := range []struct {
		name, host string
		rules      []Rule
		reach      Reach
		guest      *vm.Egress
		allowed    bool
	}{
		{name: "zero policy", host: "api.example.test"},
		{name: "hostname rule ignores service port and inspection", host: "api.example.test", rules: nameRule, allowed: true},
		{name: "normalized hostname", host: "API.EXAMPLE.TEST.", rules: nameRule, allowed: true},
		{name: "unmatched hostname", host: "other.test", rules: nameRule},
		{name: "internet reach", host: "other.test", reach: ReachInternet, allowed: true},
		{name: "all reach", host: "other.test", reach: ReachAll, allowed: true},
		{name: "explicit rules reach", host: "api.example.test", rules: nameRule, reach: ReachRules, allowed: true},
		{name: "address-only rule", host: "192.0.2.1", rules: []Rule{{Hosts: []string{"192.0.2.1"}}}},
		{name: "CIDR-only rule", host: "api.example.test", rules: []Rule{{Hosts: []string{"0.0.0.0/0"}}}},
		{name: "internet fallback with address rules", host: "api.example.test", rules: []Rule{{Hosts: []string{"0.0.0.0/0"}}}, reach: ReachInternet, allowed: true},
		{name: "empty guest policy", host: "api.example.test", rules: nameRule, guest: &vm.Egress{}},
		{name: "guest allow ignores service port", host: "api.example.test", rules: nameRule, guest: &vm.Egress{Allow: guestAllow}, allowed: true},
		{name: "guest does not widen network", host: "api.example.test", guest: &vm.Egress{Allow: guestAllow}},
		{name: "guest restricts internet", host: "other.test", reach: ReachInternet, guest: &vm.Egress{Allow: guestAllow}},
		{name: "guest unrestricted deny wins", host: "api.example.test", rules: nameRule, guest: &vm.Egress{Allow: guestAllow, Deny: []vm.Reach{{Host: "*.example.test"}}}},
		{name: "guest service deny waits for flow", host: "api.example.test", rules: nameRule, guest: &vm.Egress{Allow: guestAllow, Deny: []vm.Reach{{Host: "api.example.test", Ports: []int{443}}}}, allowed: true},
		{name: "guest port 53 deny is not a DNS rule", host: "api.example.test", rules: nameRule, guest: &vm.Egress{Allow: guestAllow, Deny: []vm.Reach{{Host: "api.example.test", Ports: []int{53}}}}, allowed: true},
		{name: "guest address deny waits for resolution", host: "api.example.test", rules: nameRule, guest: &vm.Egress{Allow: guestAllow, Deny: []vm.Reach{{Host: "0.0.0.0/0"}}}, allowed: true},
		{name: "guest address-only allow", host: "api.example.test", reach: ReachAll, guest: &vm.Egress{Allow: []vm.Reach{{Host: "0.0.0.0/0"}}}},
		{name: "missing question name", reach: ReachAll},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := &Policy{Rules: tc.rules, Reach: tc.reach}
			f := vmnet.Flow{Host: tc.host, Guest: "vm1", Src: netip.MustParseAddrPort("192.168.127.2:40000"), Dst: netip.MustParseAddrPort("192.168.127.1:53"), Egress: tc.guest}
			for _, qtype := range []uint16{dns.TypeA, dns.TypeAAAA, dns.TypeCNAME, dns.TypeNS, dns.TypeTXT, dns.TypeMX, dns.TypeSRV} {
				err := p.AuthorizeDNS(context.Background(), f, qtype)
				if tc.allowed {
					if err != nil {
						t.Fatalf("type %d: %v", qtype, err)
					}
				} else if !errors.Is(err, vmnet.ErrDenied) {
					t.Fatalf("type %d: error %v, want ErrDenied", qtype, err)
				}
			}
		})
	}
}

func TestPassthroughAuthorizesDNS(t *testing.T) {
	f := namedFlow("api.example.test", 53)
	f.Egress = &vm.Egress{}
	if err := (vmnet.Passthrough{}).AuthorizeDNS(context.Background(), f, dns.TypeTXT); err != nil {
		t.Fatalf("passthrough DNS: %v", err)
	}
}

func TestFlowResolutionUsesConfiguredDNS(t *testing.T) {
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	var queries atomic.Int32
	started := make(chan struct{})
	server := &dns.Server{
		PacketConn:        pc,
		NotifyStartedFunc: func() { close(started) },
		Handler: dns.HandlerFunc(func(w dns.ResponseWriter, r *dns.Msg) {
			queries.Add(1)
			m := new(dns.Msg)
			m.SetReply(r)
			q := r.Question[0]
			if q.Name != "api.example.test." {
				// In particular localhost must fail here, even if the host's
				// own resolver could resolve it without consulting DNS.
				m.Rcode = dns.RcodeNameError
			} else if q.Qtype == dns.TypeA {
				m.Answer = []dns.RR{&dns.A{Hdr: dns.RR_Header{Name: q.Name, Rrtype: dns.TypeA, Class: dns.ClassINET, Ttl: 60}, A: net.IPv4(127, 0, 0, 1)}}
			}
			_ = w.WriteMsg(m)
		}),
	}
	go func() { _ = server.ActivateAndServe() }()
	<-started
	t.Cleanup(func() { _ = server.Shutdown() })
	resolver, err := dnsproxy.New(pc.LocalAddr().String())
	if err != nil {
		t.Fatal(err)
	}
	ctx := dnsproxy.WithResolver(t.Context(), resolver)
	port := echoServer(t)
	loopback := netip.MustParseAddr("127.0.0.1")
	for _, tc := range []struct {
		name           string
		egress         vmnet.Egress
		customResolver bool
	}{
		{"passthrough", vmnet.Passthrough{}, false},
		{"policy", &Policy{Rules: []Rule{{Hosts: []string{"api.example.test", "localhost"}}}, DenyPrefixes: []netip.Prefix{}}, false},
		{"explicit policy resolver", &Policy{Rules: []Rule{{Hosts: []string{"api.example.test"}}}, DenyPrefixes: []netip.Prefix{}, Resolver: hosts{"api.example.test": {loopback}}}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			before := queries.Load()
			c, err := tc.egress.DialFlow(ctx, namedFlow("api.example.test", port))
			if err != nil {
				t.Fatal(err)
			}
			expectEcho(t, c)
			if consulted := queries.Load() > before; consulted == tc.customResolver {
				t.Fatalf("configured DNS consulted = %t, explicit policy resolver = %t", consulted, tc.customResolver)
			}
			if tc.customResolver {
				return
			}
			c, err = tc.egress.DialFlow(ctx, namedFlow("localhost", port))
			if c != nil {
				c.Close()
			}
			if err == nil {
				t.Fatal("configured DNS refused localhost, but the flow used another resolver")
			}
		})
	}
	t.Run("explicit passthrough resolver", func(t *testing.T) {
		var customDials atomic.Int32
		custom := &net.Resolver{PreferGo: true, Dial: func(ctx context.Context, network, _ string) (net.Conn, error) {
			customDials.Add(1)
			return (&net.Dialer{}).DialContext(ctx, network, pc.LocalAddr().String())
		}}
		p := vmnet.Passthrough{Dialer: &net.Dialer{Resolver: custom}}
		c, err := p.DialFlow(ctx, namedFlow("api.example.test", port))
		if err != nil {
			t.Fatal(err)
		}
		expectEcho(t, c)
		if customDials.Load() == 0 {
			t.Fatal("flow ignored the explicitly configured passthrough resolver")
		}
	})
	before := queries.Load()
	c, err := (vmnet.Passthrough{}).DialFlow(ctx, addrFlow(netip.AddrPortFrom(loopback, port)))
	if err != nil {
		t.Fatal(err)
	}
	expectEcho(t, c)
	if queries.Load() != before {
		t.Fatal("a direct-address flow consulted DNS")
	}
	p := &Policy{Rules: []Rule{{Hosts: []string{"api.example.test"}}}}
	if c, err := p.DialFlow(ctx, namedFlow("api.example.test", port)); !errors.Is(err, vmnet.ErrDenied) {
		if c != nil {
			c.Close()
		}
		t.Fatalf("configured DNS resolved to loopback: %v, want ErrDenied", err)
	}
}

func TestPassthroughBoundsConfiguredDNSLookup(t *testing.T) {
	// A bound but silent UDP server leaves lookup pending until the dialer's
	// deadline. No external resolver or simulated connection is involved.
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer pc.Close()
	resolver, err := dnsproxy.New(pc.LocalAddr().String())
	if err != nil {
		t.Fatal(err)
	}
	const timeout = 100 * time.Millisecond
	for _, tc := range []struct {
		name              string
		timeout, deadline time.Duration
		parentExpired     bool
	}{
		{name: "timeout", timeout: timeout},
		{name: "negative timeout", timeout: -timeout},
		{name: "deadline", deadline: timeout},
		{name: "earlier timeout", timeout: timeout, deadline: time.Second},
		{name: "earlier deadline", timeout: time.Second, deadline: timeout},
		{name: "earlier context", timeout: time.Second, parentExpired: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := &net.Dialer{Timeout: tc.timeout}
			if tc.deadline != 0 {
				d.Deadline = time.Now().Add(tc.deadline)
			}
			parentTimeout := 2 * time.Second
			if tc.parentExpired {
				parentTimeout = -timeout
			}
			ctx, cancel := context.WithTimeout(t.Context(), parentTimeout)
			defer cancel()
			_, err := (vmnet.Passthrough{Dialer: d}).DialFlow(dnsproxy.WithResolver(ctx, resolver), namedFlow("api.example.test", 443))
			var timeoutErr net.Error
			if !errors.As(err, &timeoutErr) || !timeoutErr.Timeout() {
				t.Fatalf("lookup = %v, want a timeout", err)
			}
			if !tc.parentExpired && ctx.Err() != nil {
				t.Fatal("lookup outlived the dialer's limit and reached the parent deadline")
			}
		})
	}
}
