package egress

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"testing/iotest"

	"github.com/shazow/virtle/vm"
	"github.com/shazow/virtle/vmnet"
)

// seen is what an upstream handler received.
type seen struct {
	mu            sync.Mutex
	auth, query   string
	path, body    string
	contentLength int64
	proto         string
}

func (s *seen) handler(t *testing.T) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("upstream read: %v", err)
		}
		s.mu.Lock()
		s.auth, s.query, s.path, s.body = r.Header.Get("Authorization"), r.URL.Query().Get("key"), r.URL.Path, string(body)
		s.contentLength, s.proto = r.ContentLength, r.Proto
		s.mu.Unlock()
		_, _ = io.WriteString(w, "ok")
	}
}

// inspectingPolicy inspects every named host and resolves them all to the
// upstream; secrets are the caller's.
func inspectingPolicy(t *testing.T, upstream *httptest.Server, rec Recorder, secrets ...Secret) *Policy {
	t.Helper()
	ca, err := LoadOrCreateCA(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	upstreamTLS := &tls.Config{}
	if upstream.TLS != nil {
		// httptest's certificate names example.com, not the hosts the
		// policy resolves to it.
		pool := x509.NewCertPool()
		pool.AddCert(upstream.Certificate())
		upstreamTLS.RootCAs, upstreamTLS.ServerName = pool, "example.com"
	}
	loopback := netip.MustParseAddr("127.0.0.1")
	return &Policy{
		Rules:        []Rule{{Hosts: []string{"*.test"}, Inspect: true}},
		DenyPrefixes: []netip.Prefix{},
		Resolver:     hosts{"api.test": {loopback}, "other.test": {loopback}, "plain.test": {loopback}},
		Recorder:     rec,
		CA:           ca,
		UpstreamTLS:  upstreamTLS,
		Secrets:      secrets,
	}
}

// guestClient is an HTTP client whose connections are the policy's flows,
// as a guest's would be, trusting the policy's CA.
func guestClient(p *Policy, guest *vm.Egress, h2 bool) *http.Client {
	pool := x509.NewCertPool()
	pool.AppendCertsFromPEM(p.CAPEM())
	return &http.Client{Transport: &http.Transport{
		DialContext: func(ctx context.Context, _, addr string) (net.Conn, error) {
			host, portStr, err := net.SplitHostPort(addr)
			if err != nil {
				return nil, err
			}
			port, _ := strconv.Atoi(portStr)
			f := namedFlow(host, uint16(port))
			f.Egress = guest
			return p.DialFlow(ctx, f)
		},
		TLSClientConfig:   &tls.Config{RootCAs: pool},
		ForceAttemptHTTP2: h2,
		DisableKeepAlives: true,
	}}
}

func upstreamPort(s *httptest.Server) string {
	_, port, _ := net.SplitHostPort(s.Listener.Addr().String())
	return port
}

func TestInspectReplacesSecretTokens(t *testing.T) {
	got := &seen{}
	upstream := httptest.NewTLSServer(got.handler(t))
	defer upstream.Close()
	rec := &events{}
	p := inspectingPolicy(t, upstream, rec, Secret{
		Name:  "TOKEN",
		Value: func() (string, error) { return "real-secret", nil },
		Hosts: []string{"api.test"},
	})
	if err := p.Validate(); err != nil {
		t.Fatal(err)
	}
	env := p.GuestEnv(nil)
	if len(env) != 1 || !strings.HasPrefix(env[0], "TOKEN=virtle_TOKEN_") {
		t.Fatalf("GuestEnv = %v", env)
	}
	token := strings.TrimPrefix(env[0], "TOKEN=")
	port := upstreamPort(upstream)

	client := guestClient(p, nil, false)
	req, _ := http.NewRequest("POST", "https://api.test:"+port+"/v1/"+token+"/items?key="+token, strings.NewReader("data="+token+"&x=1"))
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("request through the policy: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 || string(body) != "ok" {
		t.Fatalf("response %d %q", resp.StatusCode, body)
	}
	got.mu.Lock()
	if got.auth != "Bearer real-secret" || got.query != "real-secret" || got.path != "/v1/real-secret/items" || got.body != "data=real-secret&x=1" {
		t.Errorf("upstream saw auth %q query %q path %q body %q", got.auth, got.query, got.path, got.body)
	}
	if got.contentLength != int64(len("data=real-secret&x=1")) {
		t.Errorf("upstream content length = %d", got.contentLength)
	}
	got.mu.Unlock()

	rec.mu.Lock()
	if len(rec.list) != 2 {
		t.Fatalf("events = %+v, want the flow and the request", rec.list)
	}
	flow, request := rec.list[0], rec.list[1]
	rec.mu.Unlock()
	if flow.Decision != Allowed || flow.Rule != "*.test" || flow.Method != "" {
		t.Errorf("flow event = %+v", flow)
	}
	if request.Method != "POST" || request.Path != "/v1/"+token+"/items" || request.Status != 200 || len(request.Secrets) != 1 || request.Secrets[0] != "TOKEN" {
		t.Errorf("request event = %+v; the recorded path must carry the token, never the secret", request)
	}

	// The token is inert at a host the secret does not name, even though
	// that host is inspected too.
	req, _ = http.NewRequest("GET", "https://other.test:"+port+"/?key="+token, nil)
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err = client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	got.mu.Lock()
	if got.auth != "Bearer "+token || got.query != token {
		t.Errorf("secret sent to other.test: auth %q query %q", got.auth, got.query)
	}
	got.mu.Unlock()
	if ev := rec.last(t); len(ev.Secrets) != 0 {
		t.Errorf("event claims secrets were used: %+v", ev)
	}

	// A guest whose policy names no secrets keeps the token as is.
	req, _ = http.NewRequest("GET", "https://api.test:"+port+"/", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err = guestClient(p, &vm.Egress{Allow: []vm.Reach{{Host: "api.test"}}}, false).Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	got.mu.Lock()
	if got.auth != "Bearer "+token {
		t.Errorf("a guest without the secret got it: %q", got.auth)
	}
	got.mu.Unlock()

	// HTTP/2 end to end.
	req, _ = http.NewRequest("GET", "https://api.test:"+port+"/h2", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err = guestClient(p, nil, true).Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.ProtoMajor != 2 {
		t.Errorf("guest side negotiated %s, want HTTP/2", resp.Proto)
	}
	got.mu.Lock()
	if got.auth != "Bearer real-secret" {
		t.Errorf("over h2 the upstream saw %q", got.auth)
	}
	got.mu.Unlock()
}

func TestInspectPlainHTTPAndStreamedBodies(t *testing.T) {
	got := &seen{}
	upstream := httptest.NewServer(got.handler(t))
	defer upstream.Close()
	p := inspectingPolicy(t, upstream, &events{}, Secret{
		Name:  "KEY",
		Token: "fixed-token",
		Value: func() (string, error) { return "the-value", nil },
		Hosts: []string{"plain.test"},
		In:    []Placement{InBody},
	})
	port := upstreamPort(upstream)
	client := guestClient(p, nil, false)

	// A body over the buffering limit is rewritten as it streams and
	// arrives chunked; tokens straddling chunk boundaries are still found.
	filler := bytes.Repeat([]byte("x"), 2<<20)
	body := append(append(append([]byte("head fixed-token "), filler...), []byte("fixed-token tail")...), 0)
	req, _ := http.NewRequest("PUT", "http://plain.test:"+port+"/upload", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer fixed-token") // not In the placements: untouched
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	got.mu.Lock()
	defer got.mu.Unlock()
	if got.contentLength != -1 {
		t.Errorf("large body arrived with length %d, want chunked", got.contentLength)
	}
	if strings.Count(got.body, "the-value") != 2 || strings.Contains(got.body, "fixed-token") || len(got.body) != len(body)-2*len("fixed-token")+2*len("the-value") {
		t.Errorf("streamed body: %d bytes, %d replacements", len(got.body), strings.Count(got.body, "the-value"))
	}
	if got.auth != "Bearer fixed-token" {
		t.Errorf("header rewritten despite In = body: %q", got.auth)
	}
}

func TestReplacingReaderFindsSplitTokens(t *testing.T) {
	input := strings.Repeat("abc TOKEN def TOKENTOKEN ghi TOKE", 50) + "TOKEN"
	want := strings.ReplaceAll(input, "TOKEN", "v")
	for name, src := range map[string]io.Reader{
		"one byte at a time": iotest.OneByteReader(strings.NewReader(input)),
		"whole":              strings.NewReader(input),
		"half":               iotest.HalfReader(strings.NewReader(input)),
	} {
		r := &replacingReader{src: io.NopCloser(src), token: []byte("TOKEN"), value: []byte("v")}
		got, err := io.ReadAll(iotest.OneByteReader(r))
		if err != nil || string(got) != want {
			t.Errorf("%s: %v\n got %q\nwant %q", name, err, got, want)
		}
	}
}

func TestGuestEnvNarrowsAndTokensAreStable(t *testing.T) {
	p := &Policy{Secrets: []Secret{
		{Name: "A", Value: func() (string, error) { return "a", nil }, Hosts: []string{"a.test"}},
		{Name: "B", Token: "b-token", Value: func() (string, error) { return "b", nil }, Hosts: []string{"b.test"}},
	}}
	all := p.GuestEnv(nil)
	if len(all) != 2 || !strings.HasPrefix(all[0], "A=virtle_A_") || all[1] != "B=b-token" {
		t.Fatalf("GuestEnv(nil) = %v", all)
	}
	if again := p.GuestEnv(nil); again[0] != all[0] {
		t.Fatalf("token changed between calls: %v then %v", all, again)
	}
	if only := p.GuestEnv(&vm.Egress{Secrets: []string{"B"}}); len(only) != 1 || only[0] != "B=b-token" {
		t.Fatalf("narrowed GuestEnv = %v", only)
	}
	if none := p.GuestEnv(&vm.Egress{}); len(none) != 0 {
		t.Fatalf("a guest policy without secrets got %v", none)
	}
	if p.GuestFiles() != nil {
		t.Fatal("GuestFiles without a CA")
	}
}

func TestValidate(t *testing.T) {
	ca, err := LoadOrCreateCA(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	value := func() (string, error) { return "v", nil }
	for name, tc := range map[string]struct {
		policy *Policy
		want   string
	}{
		"inspect without CA":   {&Policy{Rules: []Rule{{Hosts: []string{"a.test"}, Inspect: true}}}, "no CA"},
		"rule without hosts":   {&Policy{Rules: []Rule{{}}}, "no hosts"},
		"bad pattern":          {&Policy{Rules: []Rule{{Hosts: []string{"["}}}}, "pattern"},
		"secret without name":  {&Policy{Secrets: []Secret{{Value: value, Hosts: []string{"a"}}}}, "no name"},
		"secret without value": {&Policy{Secrets: []Secret{{Name: "A", Hosts: []string{"a"}}}}, "no value"},
		"secret without hosts": {&Policy{Secrets: []Secret{{Name: "A", Value: value}}}, "no hosts"},
		"duplicate secret":     {&Policy{Secrets: []Secret{{Name: "A", Value: value, Hosts: []string{"a"}}, {Name: "A", Value: value, Hosts: []string{"a"}}}}, "twice"},
		"bad placement":        {&Policy{Secrets: []Secret{{Name: "A", Value: value, Hosts: []string{"a"}, In: []Placement{"cookie"}}}}, "placement"},
	} {
		t.Run(name, func(t *testing.T) {
			if err := tc.policy.Validate(); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("Validate = %v, want %q", err, tc.want)
			}
		})
	}
	ok := Policy{Rules: []Rule{{Hosts: []string{"*.test"}, Inspect: true}}, CA: ca, Secrets: []Secret{{Name: "A", Value: value, Hosts: []string{"a.test"}, In: []Placement{InHeader}}}}
	if err := ok.Validate(); err != nil {
		t.Fatal(err)
	}
	if files := ok.GuestFiles(); len(files) != 1 || files[0].GuestPath != GuestCAPath {
		t.Fatalf("GuestFiles = %+v", files)
	}
}

func TestLoadOrCreateCAIsStable(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "ca")
	first, err := LoadOrCreateCA(dir)
	if err != nil {
		t.Fatal(err)
	}
	second, err := LoadOrCreateCA(dir)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(first.Certificate[0], second.Certificate[0]) || second.Leaf == nil || !second.Leaf.IsCA {
		t.Fatal("the CA was regenerated or not parsed")
	}
	info, err := os.Stat(filepath.Join(dir, caKeyFile))
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("key file mode %v, %v", info.Mode(), err)
	}
	p := &Policy{CA: first}
	leaf, err := p.certFor("api.test")
	if err != nil {
		t.Fatal(err)
	}
	if again, _ := p.certFor("API.test."); again != leaf {
		t.Fatal("leaf certificates are not cached per name")
	}
	pool := x509.NewCertPool()
	pool.AppendCertsFromPEM(p.CAPEM())
	if _, err := leaf.Leaf.Verify(x509.VerifyOptions{DNSName: "api.test", Roots: pool}); err != nil {
		t.Fatalf("minted certificate does not verify: %v", err)
	}
	if _, err := (&Policy{}).inspect(context.Background(), namedFlow("api.test", 443), "*"); err == nil {
		t.Fatal("inspect without a CA succeeded")
	}
}

var _ = vmnet.ErrDenied
