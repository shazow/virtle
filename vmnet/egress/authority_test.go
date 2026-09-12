package egress

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/shazow/virtle/vmnet"
)

func TestRequestAuthority(t *testing.T) {
	for _, tc := range []struct {
		name, authority, scheme, flowHost, dst, want string
	}{
		{"named host", "api.test:8443", "https", "api.test", "198.18.0.5:8443", "api.test:8443"},
		{"normalized host", "API.TEST.:08443", "https", "api.test", "198.18.0.5:8443", "api.test:8443"},
		{"default HTTP port", "api.test", "http", "api.test", "198.18.0.5:80", "api.test:80"},
		{"default HTTPS port", "api.test", "https", "api.test", "198.18.0.5:443", "api.test:443"},
		{"IPv4 destination", "192.0.2.1:443", "https", "", "192.0.2.1:443", "192.0.2.1:443"},
		{"IPv6 destination", "[2001:0db8::1]:443", "https", "", "[2001:db8::1]:443", "[2001:db8::1]:443"},
		{"other virtual host", "other.test:443", "https", "api.test", "198.18.0.5:443", ""},
		{"other port", "api.test:8443", "https", "api.test", "198.18.0.5:443", ""},
		{"missing nondefault port", "api.test", "https", "api.test", "198.18.0.5:8443", ""},
		{"other default port", "api.test", "http", "api.test", "198.18.0.5:443", ""},
		{"name on address flow", "api.test:443", "https", "", "192.0.2.1:443", ""},
		{"missing host", "", "https", "api.test", "198.18.0.5:443", ""},
		{"userinfo", "someone@api.test:443", "https", "api.test", "198.18.0.5:443", ""},
		{"empty explicit port", "api.test:", "https", "api.test", "198.18.0.5:443", ""},
		{"invalid port", "api.test:invalid", "https", "api.test", "198.18.0.5:443", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r, err := http.NewRequest(http.MethodGet, "/items", nil)
			if err != nil {
				t.Fatal(err)
			}
			r.Host = tc.authority
			f := vmnet.Flow{Host: tc.flowHost, Dst: netip.MustParseAddrPort(tc.dst)}
			err = validateAuthority(r, f, tc.scheme)
			if tc.want == "" {
				if !errors.Is(err, vmnet.ErrDenied) {
					t.Fatalf("validation = %v, want ErrDenied", err)
				}
				return
			}
			if err != nil || r.Host != tc.want {
				t.Fatalf("authority = %q, %v; want %q", r.Host, err, tc.want)
			}
		})
	}
}

func TestAbsoluteRequestAuthority(t *testing.T) {
	for _, tc := range []struct {
		url, host string
		allowed   bool
	}{
		{"https://API.TEST.:443/items", "API.TEST.:443", true},
		{"https://other.test:443/items", "other.test:443", false},
		{"https://other.test:443/items", "api.test:443", false},
		{"https://api.test:443/items", "other.test:443", false},
		{"http://api.test:443/items", "api.test:443", false},
	} {
		t.Run(tc.url+"_"+tc.host, func(t *testing.T) {
			r, err := http.NewRequest(http.MethodGet, tc.url, nil)
			if err != nil {
				t.Fatal(err)
			}
			r.Host = tc.host
			err = validateAuthority(r, namedFlow("api.test", 443), "https")
			if !tc.allowed {
				if !errors.Is(err, vmnet.ErrDenied) {
					t.Fatalf("validation = %v, want ErrDenied", err)
				}
				return
			}
			if err != nil || r.Host != "api.test:443" || r.URL.Host != r.Host {
				t.Fatalf("authority = %q, URL %v, error %v", r.Host, r.URL, err)
			}
		})
	}
}

func TestInspectedAuthorityControlsAdmissionAndSecrets(t *testing.T) {
	for _, tc := range []struct {
		name, scheme string
		h2           bool
	}{
		{"HTTP/1", "http", false},
		{"HTTPS/1", "https", false},
		{"HTTPS/2", "https", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := &seen{}
			upstream := httptest.NewUnstartedServer(got.handler(t))
			var admitted, injected atomic.Int32
			rec := &events{}
			p := inspectingPolicy(t, upstream, rec, Injection{
				Name: "TOKEN", Token: "placeholder", Hosts: []string{"api.test"}, In: []Placement{InHeader},
				Value: func(context.Context, Request) (string, error) { injected.Add(1); return "real-secret", nil },
			})
			if tc.scheme == "https" {
				cert, err := p.certFor("api.test")
				if err != nil {
					t.Fatal(err)
				}
				upstream.TLS = &tls.Config{Certificates: []tls.Certificate{*cert}}
				upstream.StartTLS()
				roots := x509.NewCertPool()
				roots.AppendCertsFromPEM(p.CAPEM())
				p.UpstreamTLS = &tls.Config{RootCAs: roots}
			} else {
				upstream.Start()
			}
			defer upstream.Close()
			port := upstreamPort(upstream)
			p.Admit = func(_ context.Context, r Request) error {
				admitted.Add(1)
				if r.Host != "api.test:"+port {
					t.Errorf("admitted authority = %q", r.Host)
				}
				return nil
			}
			client := guestClient(p, nil, tc.h2)
			for _, authority := range []string{"attacker.test:" + port, "api.test:1", "API.TEST.:" + port} {
				req, err := http.NewRequest(http.MethodGet, tc.scheme+"://api.test:"+port+"/items", nil)
				if err != nil {
					t.Fatal(err)
				}
				req.Host = authority
				req.Header.Set("Authorization", "Bearer placeholder")
				resp, err := client.Do(req)
				if err != nil {
					t.Fatal(err)
				}
				_, err = io.Copy(io.Discard, resp.Body)
				resp.Body.Close()
				if err != nil {
					t.Fatal(err)
				}
				wantStatus := http.StatusForbidden
				if strings.HasPrefix(authority, "API.TEST.") {
					wantStatus = http.StatusOK
				}
				if resp.StatusCode != wantStatus {
					t.Fatalf("authority %s: status %d, want %d", authority, resp.StatusCode, wantStatus)
				}
				if wantStatus == http.StatusForbidden {
					if admitted.Load() != 0 || injected.Load() != 0 {
						t.Fatal("refused authority reached admission or secret injection")
					}
					if ev := rec.last(t); ev.Decision != Denied || ev.Status != http.StatusForbidden {
						t.Fatalf("refused authority event = %+v", ev)
					}
				}
			}
			if admitted.Load() != 1 || injected.Load() != 1 {
				t.Fatalf("admitted %d, injected %d; want one authorized request", admitted.Load(), injected.Load())
			}
			got.mu.Lock()
			defer got.mu.Unlock()
			if got.host != "api.test:"+port || got.auth != "Bearer real-secret" {
				t.Fatalf("authorized upstream request: host %q, auth %q", got.host, got.auth)
			}
		})
	}
}
