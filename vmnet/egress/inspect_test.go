package egress

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
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
	"sync/atomic"
	"testing"
	"testing/iotest"
	"time"

	"github.com/shazow/virtle/vm"
	"github.com/shazow/virtle/vmnet"
)

// seen is what an upstream handler received.
type seen struct {
	mu            sync.Mutex
	auth, query   string
	path, body    string
	host          string
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
		s.contentLength, s.proto, s.host = r.ContentLength, r.Proto, r.Host
		s.mu.Unlock()
		_, _ = io.WriteString(w, "ok")
	}
}

// constValue is an Injection.Value that always returns v.
func constValue(v string) func(context.Context, Request) (string, error) {
	return func(context.Context, Request) (string, error) { return v, nil }
}

// inspectingPolicy inspects every named host and resolves them all to the
// upstream; injections are the caller's.
func inspectingPolicy(t *testing.T, upstream *httptest.Server, rec Recorder, injections ...Injection) *Policy {
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
		Injections:   injections,
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
	p := inspectingPolicy(t, upstream, rec, Injection{
		Name:  "TOKEN",
		Value: constValue("real-secret"),
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
	if request.Method != "POST" || request.Path != "/v1/"+token+"/items" || request.Status != 200 || len(request.Injections) != 1 || request.Injections[0] != "TOKEN" {
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
	if ev := rec.last(t); len(ev.Injections) != 0 {
		t.Errorf("event claims the secret was used: %+v", ev)
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
	p := inspectingPolicy(t, upstream, &events{}, Injection{
		Name:  "KEY",
		Token: "fixed-token",
		Value: constValue("the-value"),
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
		a := &applied{}
		r := &replacingReader{src: io.NopCloser(src), token: []byte("TOKEN"), value: func() (string, bool, error) { return "v", true, nil }, applied: a}
		got, err := io.ReadAll(iotest.OneByteReader(r))
		if err != nil || string(got) != want || !a.used.Load() {
			t.Errorf("%s: %v\n got %q\nwant %q", name, err, got, want)
		}
	}
}

func TestGuestEnvNarrowsAndTokensAreStable(t *testing.T) {
	p := &Policy{Injections: []Injection{
		{Name: "A", Value: constValue("a"), Hosts: []string{"a.test"}},
		{Name: "B", Token: "b-token", Value: constValue("b"), Hosts: []string{"b.test"}},
		{Token: "$NONCE$", Value: constValue("n")}, // unnamed: every guest knows it, nothing to issue
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
	value := constValue("v")
	for name, tc := range map[string]struct {
		policy *Policy
		want   string
	}{
		"inspect without CA":              {&Policy{Rules: []Rule{{Hosts: []string{"a.test"}, Inspect: true}}}, "no CA"},
		"rule without hosts":              {&Policy{Rules: []Rule{{}}}, "no hosts"},
		"bad pattern":                     {&Policy{Rules: []Rule{{Hosts: []string{"["}}}}, "pattern"},
		"injection without name or token": {&Policy{Injections: []Injection{{Value: value, Hosts: []string{"a"}}}}, "neither a name nor a token"},
		"injection without value":         {&Policy{Injections: []Injection{{Name: "A", Hosts: []string{"a"}}}}, "no value"},
		"named injection without hosts":   {&Policy{Injections: []Injection{{Name: "A", Value: value}}}, "no hosts"},
		"duplicate name":                  {&Policy{Injections: []Injection{{Name: "A", Value: value, Hosts: []string{"a"}}, {Name: "A", Value: value, Hosts: []string{"a"}}}}, "twice"},
		"bad placement":                   {&Policy{Injections: []Injection{{Name: "A", Value: value, Hosts: []string{"a"}, In: []Placement{"cookie"}}}}, "placement"},
		"unnamed without value":           {&Policy{Injections: []Injection{{Token: "$X$"}}}, "no value"},
		"injection bad pattern":           {&Policy{Injections: []Injection{{Token: "$X$", Value: value, Hosts: []string{"["}}}}, "pattern"},
	} {
		t.Run(name, func(t *testing.T) {
			if err := tc.policy.Validate(); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("Validate = %v, want %q", err, tc.want)
			}
		})
	}
	ok := Policy{Rules: []Rule{{Hosts: []string{"*.test"}, Inspect: true}}, CA: ca, Injections: []Injection{{Name: "A", Value: value, Hosts: []string{"a.test"}, In: []Placement{InHeader}}}}
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

func TestInspectInjectsValues(t *testing.T) {
	got := &seen{}
	upstream := httptest.NewTLSServer(got.handler(t))
	defer upstream.Close()
	rec := &events{}
	p := inspectingPolicy(t, upstream, rec)
	var calls atomic.Int32
	const token = "$VIRTLE_RANDOM$"
	p.Injections = []Injection{
		{Token: token, Value: func(_ context.Context, r Request) (string, error) {
			if r.Flow.Host != "api.test" || r.Method != "POST" || !strings.Contains(r.URL.Path, token) || r.Header.Get("Authorization") != "Bearer "+token {
				t.Errorf("Value saw %s %s %v: not the request as the guest sent it", r.Flow.Host, r.Method, r.URL)
			}
			return fmt.Sprintf("v%d", calls.Add(1)), nil
		}},
		{Token: "$ELSEWHERE$", Hosts: []string{"other.test"}, Value: func(context.Context, Request) (string, error) { return "never", nil }},
		{Token: "$FAILS$", Value: func(context.Context, Request) (string, error) { return "", errors.New("unavailable") }},
	}
	if err := p.Validate(); err != nil {
		t.Fatal(err)
	}
	port := upstreamPort(upstream)
	client := guestClient(p, nil, false)
	send := func() {
		t.Helper()
		req, _ := http.NewRequest("POST", "https://api.test:"+port+"/v1/"+token+"/items?key="+token, strings.NewReader("data="+token+"&e=$ELSEWHERE$&f=$FAILS$"))
		req.Header.Set("Authorization", "Bearer "+token)
		resp, err := client.Do(req)
		if err != nil {
			t.Fatalf("request through the policy: %v", err)
		}
		resp.Body.Close()
		if resp.StatusCode != 200 {
			t.Fatalf("status %d", resp.StatusCode)
		}
	}

	send()
	got.mu.Lock()
	if got.auth != "Bearer v1" || got.query != "v1" || got.path != "/v1/v1/items" || got.body != "data=v1&e=$ELSEWHERE$&f=$FAILS$" {
		t.Errorf("upstream saw auth %q query %q path %q body %q", got.auth, got.query, got.path, got.body)
	}
	got.mu.Unlock()
	if calls.Load() != 1 {
		t.Errorf("Value called %d times for one request, want once", calls.Load())
	}
	rec.mu.Lock()
	request := rec.list[len(rec.list)-1]
	rec.mu.Unlock()
	if request.Method != "POST" || request.Path != "/v1/"+token+"/items" || len(request.Injections) != 1 || request.Injections[0] != token {
		t.Errorf("request event = %+v; want the guest's path and the one token that was replaced", request)
	}

	// Every request gets its own value.
	send()
	got.mu.Lock()
	if got.auth != "Bearer v2" || got.body != "data=v2&e=$ELSEWHERE$&f=$FAILS$" {
		t.Errorf("second request saw auth %q body %q", got.auth, got.body)
	}
	got.mu.Unlock()

	// A request without the token costs no Value call and records nothing.
	resp, err := client.Get("https://api.test:" + port + "/plain")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if calls.Load() != 2 {
		t.Errorf("Value called for a request without the token")
	}
	rec.mu.Lock()
	request = rec.list[len(rec.list)-1]
	rec.mu.Unlock()
	if request.Path != "/plain" || len(request.Injections) != 0 {
		t.Errorf("event for a request without tokens = %+v", request)
	}
}

func TestValuesAreReadOnlyWhenTheTokenIsPresent(t *testing.T) {
	got := &seen{}
	upstream := httptest.NewServer(got.handler(t))
	defer upstream.Close()
	rec := &events{}
	var secretReads, injectionReads atomic.Int32
	p := inspectingPolicy(t, upstream, rec, Injection{
		Name:  "KEY",
		Token: "fixed-token",
		Value: func(context.Context, Request) (string, error) { secretReads.Add(1); return "the-value", nil },
		Hosts: []string{"plain.test"},
	})
	p.Injections = append(p.Injections, Injection{Token: "$NONCE$", Value: func(context.Context, Request) (string, error) {
		injectionReads.Add(1)
		return "n", nil
	}})
	port := upstreamPort(upstream)
	client := guestClient(p, nil, false)

	resp, err := client.Get("http://plain.test:" + port + "/nothing")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if secretReads.Load() != 0 || injectionReads.Load() != 0 {
		t.Fatalf("values read %d and %d times for a request without tokens", secretReads.Load(), injectionReads.Load())
	}

	// Tokens deep in a streamed body are found and resolved as they pass,
	// once each, and the request's event names them.
	filler := bytes.Repeat([]byte("x"), 2<<20)
	body := append(append(append([]byte("head $NONCE$ "), filler...), []byte(" fixed-token $NONCE$ tail")...), 0)
	req, _ := http.NewRequest("PUT", "http://plain.test:"+port+"/upload", bytes.NewReader(body))
	resp, err = client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if secretReads.Load() != 1 || injectionReads.Load() != 1 {
		t.Errorf("values read %d and %d times, want once each", secretReads.Load(), injectionReads.Load())
	}
	got.mu.Lock()
	if !strings.HasPrefix(got.body, "head n ") || !strings.HasSuffix(got.body, " the-value n tail\x00") || len(got.body) != len(body)-2*len("$NONCE$")+2-len("fixed-token")+len("the-value") {
		t.Errorf("streamed body: %d bytes, head %q, tail %q", len(got.body), got.body[:8], got.body[len(got.body)-24:])
	}
	got.mu.Unlock()
	rec.mu.Lock()
	request := rec.list[len(rec.list)-1]
	rec.mu.Unlock()
	if len(request.Injections) != 2 || request.Injections[0] != "KEY" || request.Injections[1] != "$NONCE$" {
		t.Errorf("request event = %+v", request)
	}
}

func TestReplacingReaderValueContainingToken(t *testing.T) {
	// A value that contains the token must not be replaced again.
	a := &applied{}
	r := &replacingReader{
		src:     io.NopCloser(strings.NewReader("a$T$b$T$c")),
		token:   []byte("$T$"),
		value:   func() (string, bool, error) { return "<$T$>", true, nil },
		applied: a,
	}
	out, err := io.ReadAll(r)
	if err != nil || string(out) != "a<$T$>b<$T$>c" || !a.used.Load() {
		t.Fatalf("got %q, %v, used=%v", out, err, a.used.Load())
	}
	// The same when the token is only seen once the source is done.
	a = &applied{}
	r = &replacingReader{
		src:     io.NopCloser(iotest.DataErrReader(strings.NewReader("ab$T$"))),
		token:   []byte("$T$"),
		value:   func() (string, bool, error) { return "<$T$>", true, nil },
		applied: a,
	}
	if out, err := io.ReadAll(r); err != nil || string(out) != "ab<$T$>" {
		t.Fatalf("at end of stream: got %q, %v", out, err)
	}
	// An unresolvable value passes the stream through untouched.
	a = &applied{}
	r = &replacingReader{
		src:     io.NopCloser(strings.NewReader("a$T$b")),
		token:   []byte("$T$"),
		value:   func() (string, bool, error) { return "", false, nil },
		applied: a,
	}
	if out, err := io.ReadAll(r); err != nil || string(out) != "a$T$b" || a.used.Load() {
		t.Fatalf("got %q, %v, used=%v", out, err, a.used.Load())
	}
}

func TestInspectOutlivesTheDialContext(t *testing.T) {
	got := &seen{}
	upstream := httptest.NewServer(got.handler(t))
	defer upstream.Close()
	p := inspectingPolicy(t, upstream, &events{})
	port := upstreamPort(upstream)
	portNum, _ := strconv.Atoi(port)
	// A network cancels the dial's context as soon as DialFlow returns; the
	// inspected connection must keep serving for as long as the guest holds it.
	client := &http.Client{Transport: &http.Transport{
		DialContext: func(context.Context, string, string) (net.Conn, error) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			return p.DialFlow(ctx, namedFlow("plain.test", uint16(portNum)))
		},
		DisableKeepAlives: true,
	}}
	resp, err := client.Get("http://plain.test:" + port + "/after-dial")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 || string(body) != "ok" {
		t.Fatalf("response %d %q; the proxy must not inherit the dial's cancellation", resp.StatusCode, body)
	}
}

func TestInjectionRefusesTheRequest(t *testing.T) {
	// The upstream drains what it gets: a request refused as its body
	// streams is aborted under it, which is the point.
	var mu sync.Mutex
	var reached []string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		mu.Lock()
		reached = append(reached, r.URL.Path)
		mu.Unlock()
		_, _ = io.WriteString(w, "ok")
	}))
	defer upstream.Close()
	rec := &events{}
	p := inspectingPolicy(t, upstream, rec, Injection{
		Token: "$VIRTLE_REJECT$",
		Value: func(context.Context, Request) (string, error) { return "", vmnet.ErrDenied },
	})
	port := upstreamPort(upstream)
	client := guestClient(p, nil, false)

	// In a header: refused before anything reaches the upstream.
	req, _ := http.NewRequest("GET", "http://plain.test:"+port+"/tripwire", nil)
	req.Header.Set("X-Token", "$VIRTLE_REJECT$")
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("status %d, want 403", resp.StatusCode)
	}
	mu.Lock()
	if len(reached) != 0 {
		t.Errorf("the refused request reached the upstream: %v", reached)
	}
	mu.Unlock()
	ev := rec.last(t)
	if ev.Decision != Denied || ev.Status != 403 || ev.Path != "/tripwire" || !strings.Contains(ev.Reason, "$VIRTLE_REJECT$") || len(ev.Injections) != 0 {
		t.Errorf("event = %+v", ev)
	}

	// Deep in a streamed body: the request is aborted and still on record
	// as refused.
	body := append(append(bytes.Repeat([]byte("x"), 2<<20), []byte("$VIRTLE_REJECT$")...), 0)
	req, _ = http.NewRequest("PUT", "http://plain.test:"+port+"/upload", bytes.NewReader(body))
	resp, err = client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("streamed refusal: status %d, want 403", resp.StatusCode)
	}
	if ev := rec.last(t); ev.Decision != Denied || ev.Status != 403 || ev.Path != "/upload" || !strings.Contains(ev.Reason, "$VIRTLE_REJECT$") {
		t.Errorf("streamed refusal event = %+v", ev)
	}

	// Without the token the same requests pass.
	resp, err = client.Get("http://plain.test:" + port + "/clear")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 || rec.last(t).Decision != Allowed {
		t.Errorf("clear request: status %d, event %+v", resp.StatusCode, rec.last(t))
	}
}

func TestAdmitDecidesPerRequest(t *testing.T) {
	got := &seen{}
	upstream := httptest.NewTLSServer(got.handler(t))
	defer upstream.Close()
	rec := &events{}
	p := inspectingPolicy(t, upstream, rec)
	var admitted []string
	p.Admit = func(_ context.Context, r Request) error {
		admitted = append(admitted, r.Method+" "+r.URL.Path+" "+r.Header.Get("X-Reason"))
		if r.URL.Path == "/forbidden" {
			return fmt.Errorf("the policy forbids %s: %w", r.URL.Path, vmnet.ErrDenied)
		}
		if r.Header.Get("X-Reason") == "broken" {
			return errors.New("decider unavailable")
		}
		return nil
	}
	port := upstreamPort(upstream)
	client := guestClient(p, nil, false)
	for _, tc := range []struct {
		path, reason string
		status       int
		decision     Decision
	}{
		{"/ok", "", 200, Allowed},
		{"/forbidden", "", 403, Denied},
		{"/ok", "broken", 502, Allowed},
	} {
		req, _ := http.NewRequest("GET", "https://api.test:"+port+tc.path, nil)
		req.Header.Set("X-Reason", tc.reason)
		resp, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		ev := rec.last(t)
		if resp.StatusCode != tc.status || ev.Decision != tc.decision || ev.Status != tc.status || ev.Path != tc.path {
			t.Errorf("%s %q: status %d, event %+v; want %d %s", tc.path, tc.reason, resp.StatusCode, ev, tc.status, tc.decision)
		}
		if tc.status == 502 && ev.Err == nil {
			t.Errorf("a failed decision must record its error: %+v", ev)
		}
	}
	if len(admitted) != 3 || admitted[1] != "GET /forbidden " {
		t.Errorf("Admit saw %q", admitted)
	}
	got.mu.Lock()
	if got.path == "/forbidden" {
		t.Error("the forbidden request reached the upstream")
	}
	got.mu.Unlock()
}

func TestInspectedFlowsEndWithTheConnection(t *testing.T) {
	got := &seen{}
	plain := httptest.NewServer(got.handler(t))
	defer plain.Close()
	secure := httptest.NewTLSServer(got.handler(t))
	defer secure.Close()
	for name, tc := range map[string]struct {
		upstream *httptest.Server
		url      string
		h2       bool
	}{
		"http/1.1": {plain, "http://plain.test:" + upstreamPort(plain) + "/", false},
		"h2":       {secure, "https://api.test:" + upstreamPort(secure) + "/", true},
	} {
		t.Run(name, func(t *testing.T) {
			p := inspectingPolicy(t, tc.upstream, &events{})
			client := guestClient(p, nil, tc.h2)
			finished := make(chan struct{}, 1)
			transport := client.Transport.(*http.Transport)
			t.Cleanup(transport.CloseIdleConnections)
			transport.DialContext = func(ctx context.Context, _, addr string) (net.Conn, error) {
				host, portStr, err := net.SplitHostPort(addr)
				if err != nil {
					return nil, err
				}
				port, _ := strconv.Atoi(portStr)
				guest, server := net.Pipe()
				t.Cleanup(func() { _ = guest.Close() })
				go func() {
					p.serveInspected(context.WithoutCancel(ctx), server, namedFlow(host, uint16(port)), "*.test")
					finished <- struct{}{}
				}()
				return guest, nil
			}
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			for range 4 {
				req, err := http.NewRequestWithContext(ctx, http.MethodGet, tc.url, nil)
				if err != nil {
					t.Fatal(err)
				}
				resp, err := client.Do(req)
				if err != nil {
					t.Fatal(err)
				}
				_, err = io.Copy(io.Discard, resp.Body)
				resp.Body.Close()
				if err != nil {
					t.Fatal(err)
				}
				if resp.StatusCode != 200 {
					t.Fatalf("status %d", resp.StatusCode)
				}
				// Wait for this connection's serving goroutine itself to end,
				// independently of unrelated goroutines in the test process.
				select {
				case <-finished:
				case <-ctx.Done():
					t.Fatal("inspection server did not end with its connection")
				}
			}
		})
	}
}

func TestInjectedQueryValuesAreEncoded(t *testing.T) {
	got := &seen{}
	upstream := httptest.NewServer(got.handler(t))
	defer upstream.Close()
	const value = "a b&c=d/e?#"
	p := inspectingPolicy(t, upstream, &events{}, Injection{Token: "$V$", Value: constValue(value)})
	port := upstreamPort(upstream)
	resp, err := guestClient(p, nil, false).Get("http://plain.test:" + port + "/q?key=$V$&other=1")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	got.mu.Lock()
	defer got.mu.Unlock()
	if got.query != value {
		t.Errorf("upstream decoded key=%q, want %q: the value must be query-encoded in place of the token", got.query, value)
	}
	// The request reaches the upstream with the Host the guest sent.
	if got.host != "plain.test:"+port {
		t.Errorf("upstream saw Host %q, want %q", got.host, "plain.test:"+port)
	}
}

func TestInspectMintsForTheResolvedName(t *testing.T) {
	upstream := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	defer upstream.Close()
	p := inspectingPolicy(t, upstream, &events{})
	p.Rules = append(p.Rules, Rule{Hosts: []string{"127.0.0.0/8"}, Inspect: true})
	port := netip.MustParseAddrPort(upstream.Listener.Addr().String()).Port()
	pool := x509.NewCertPool()
	pool.AppendCertsFromPEM(p.CAPEM())
	handshake := func(f vmnet.Flow, cfg *tls.Config) (*x509.Certificate, error) {
		t.Helper()
		raw, err := p.DialFlow(context.Background(), f)
		if err != nil {
			t.Fatal(err)
		}
		defer raw.Close()
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		c := tls.Client(raw, cfg)
		if err := c.HandshakeContext(ctx); err != nil {
			return nil, err
		}
		return c.ConnectionState().PeerCertificates[0], nil
	}

	// The name the guest resolved names the certificate.
	cert, err := handshake(namedFlow("api.test", port), &tls.Config{ServerName: "api.test", RootCAs: pool})
	if err != nil {
		t.Fatalf("handshake for the resolved name: %v", err)
	}
	if len(cert.DNSNames) != 1 || cert.DNSNames[0] != "api.test" {
		t.Errorf("certificate names %v, want api.test", cert.DNSNames)
	}
	// An SNI for another name gets the same certificate, and no new one is
	// minted for it: the guest connected to api.test's address.
	if _, err := handshake(namedFlow("api.test", port), &tls.Config{ServerName: "other.test", RootCAs: pool}); err == nil {
		t.Error("a certificate verified for an SNI other than the resolved name")
	}
	p.mu.Lock()
	leaves := len(p.leaves)
	p.mu.Unlock()
	if leaves != 1 {
		t.Errorf("%d certificates minted, want 1: a guest's SNI must not mint", leaves)
	}
	// A flow by address without an SNI gets a certificate for the address.
	cert, err = handshake(addrFlow(netip.AddrPortFrom(netip.MustParseAddr("127.0.0.1"), port)), &tls.Config{InsecureSkipVerify: true})
	if err != nil {
		t.Fatalf("handshake for an address flow: %v", err)
	}
	if len(cert.IPAddresses) != 1 || !cert.IPAddresses[0].Equal(net.ParseIP("127.0.0.1")) || len(cert.DNSNames) != 0 {
		t.Errorf("address flow certificate names %v / %v, want the address", cert.DNSNames, cert.IPAddresses)
	}
}
