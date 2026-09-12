package egress

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"crypto/tls"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httputil"
	"net/netip"
	"net/url"
	"path"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/shazow/virtle/vm"
	"github.com/shazow/virtle/vmnet"
)

// Placement is where in an HTTP request a token is replaced.
type Placement string

const (
	InHeader Placement = "header" // any header value
	InQuery  Placement = "query"  // the URL query
	InPath   Placement = "path"   // the URL path
	InBody   Placement = "body"   // the request body
)

// Placements are every Placement, the default for an Injection without In.
var Placements = []Placement{InHeader, InQuery, InPath, InBody}

// GuestCAPath is where GuestFiles places the CA certificate.
const GuestCAPath = "/etc/virtle/ca.pem"

// GuestFiles are the files a guest needs to have its flows inspected: the
// CA certificate at GuestCAPath, which the guest must add to its trust
// store (update-ca-certificates, SSL_CERT_FILE, NODE_EXTRA_CA_CERTS, ...).
// Nil without a CA.
func (p *Policy) GuestFiles() []vm.File {
	pemBytes := p.CAPEM()
	if pemBytes == nil {
		return nil
	}
	return []vm.File{{GuestPath: GuestCAPath, Content: bytes.NewReader(pemBytes), Mode: 0o644}}
}

// GuestEnv is the environment a guest with policy e receives: NAME=token
// for each named Injection it may hold, which is every named one when e is
// nil and those e.Secrets names otherwise.
func (p *Policy) GuestEnv(e *vm.Egress) []string {
	var env []string
	for _, inj := range p.injectionsFor(e) {
		if inj.Name != "" {
			env = append(env, inj.Name+"="+p.tokenOf(inj))
		}
	}
	return env
}

// injectionsFor lists the Injections a guest with policy e may use: every
// unnamed one, and the named ones e allows (all of them when e is nil).
func (p *Policy) injectionsFor(e *vm.Egress) []*Injection {
	var list []*Injection
	for i := range p.Injections {
		inj := &p.Injections[i]
		if inj.Name == "" || e == nil || slices.Contains(e.Secrets, inj.Name) {
			list = append(list, inj)
		}
	}
	return list
}

// tokenOf is the injection's token, generated from its name on first use
// when it has none.
func (p *Policy) tokenOf(inj *Injection) string {
	if inj.Token != "" || inj.Name == "" {
		return inj.Token
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if token, ok := p.tokens[inj.Name]; ok {
		return token
	}
	var random [16]byte
	if _, err := rand.Read(random[:]); err != nil {
		panic("egress: random source failed: " + err.Error())
	}
	token := "virtle_" + strings.ToUpper(inj.Name) + "_" + hex.EncodeToString(random[:])
	if p.tokens == nil {
		p.tokens = make(map[string]string)
	}
	p.tokens[inj.Name] = token
	return token
}

// Validate reports a Policy that cannot work: an unknown Reach, malformed
// patterns, a rule that inspects with no CA to mint certificates from, or an
// Injection with
// neither a name nor a token, without a value, named twice, or named but
// without the hosts its value may go to.
func (p *Policy) Validate() error {
	switch p.Reach {
	case "", ReachRules, ReachInternet, ReachAll:
	default:
		return fmt.Errorf("egress: unknown reach %q", p.Reach)
	}
	inspects := false
	for i, r := range p.Rules {
		if len(r.Hosts) == 0 {
			return fmt.Errorf("egress: rule %d has no hosts", i)
		}
		for _, h := range r.Hosts {
			if err := ValidPattern(h); err != nil {
				return fmt.Errorf("egress: rule %d: %w", i, err)
			}
		}
		inspects = inspects || r.Inspect
	}
	if inspects && len(p.CA.Certificate) == 0 {
		return errors.New("egress: a rule inspects but the policy has no CA (see LoadOrCreateCA)")
	}
	names := make(map[string]bool, len(p.Injections))
	for i := range p.Injections {
		inj := &p.Injections[i]
		switch {
		case inj.Name == "" && inj.Token == "":
			return fmt.Errorf("egress: injection %d has neither a name nor a token", i)
		case inj.Name != "" && names[inj.Name]:
			return fmt.Errorf("egress: injection %q is defined twice", inj.Name)
		case inj.Value == nil:
			return fmt.Errorf("egress: injection %q has no value", inj.label())
		case inj.Name != "" && len(inj.Hosts) == 0:
			return fmt.Errorf("egress: injection %q is issued to guests but names no hosts it may be sent to", inj.Name)
		}
		if inj.Name != "" {
			names[inj.Name] = true
		}
		for _, h := range inj.Hosts {
			if err := ValidPattern(h); err != nil {
				return fmt.Errorf("egress: injection %q: %w", inj.label(), err)
			}
		}
		for _, in := range inj.In {
			if !slices.Contains(Placements, in) {
				return fmt.Errorf("egress: injection %q: unknown placement %q", inj.label(), in)
			}
		}
	}
	return nil
}

// inspect serves an inspected flow: the guest gets one end of a pipe, and
// the other end is terminated (TLS with a minted certificate when the guest
// starts a handshake, plain HTTP otherwise) and reverse-proxied to the
// real destination, each request admitted and its tokens replaced.
func (p *Policy) inspect(ctx context.Context, f vmnet.Flow, rule string) (net.Conn, error) {
	if len(p.CA.Certificate) == 0 {
		return nil, fmt.Errorf("egress: inspecting %s needs a CA: %w", f.Host, vmnet.ErrDenied)
	}
	guest, server := net.Pipe()
	// The dial's context ends when DialFlow returns (a network bounds the
	// dial, not the flow); the connection serves for as long as the guest
	// keeps it, and each request's own context comes from the server.
	go p.serveInspected(context.WithoutCancel(ctx), server, f, rule)
	return guest, nil
}

func (p *Policy) serveInspected(ctx context.Context, conn net.Conn, f vmnet.Flow, rule string) {
	defer conn.Close()
	peek := bufio.NewReader(conn)
	first, err := peek.Peek(1)
	if err != nil {
		return
	}
	var served net.Conn = &peekedConn{Conn: conn, Reader: peek}
	scheme := "http"
	if first[0] == 0x16 { // a TLS handshake record
		scheme = "https"
		served = tls.Server(served, &tls.Config{
			NextProtos: []string{"h2", "http/1.1"},
			GetCertificate: func(hello *tls.ClientHelloInfo) (*tls.Certificate, error) {
				// The certificate names what the flow is for. The name the
				// guest resolved is authoritative: a guest that sends
				// another SNI is misdirecting its request, and answering
				// it would let a guest mint one certificate per name it
				// invents.
				host := f.Host
				if host == "" {
					host = hello.ServerName
				}
				if host == "" {
					host = f.Dst.Addr().String()
				}
				return p.certFor(host)
			},
		})
	}
	target := &url.URL{Scheme: scheme, Host: f.Target()}
	proxy := &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.SetURL(target)
			pr.Out.Host = pr.In.Host // the validated, normalized authority
		},
		Transport: p.upstreamTransport(f),
		ModifyResponse: func(resp *http.Response) error {
			p.recordRequest(f, rule, resp.Request, resp.StatusCode, nil)
			return nil
		},
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			// A token refused as the body streamed aborts the upstream
			// request; the record says it was refused, not that the
			// upstream failed.
			p.refuse(w, r, f, rule, err)
		},
	}
	listener := &oneConnListener{conn: served, done: make(chan struct{})}
	server := &http.Server{
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if err := validateAuthority(r, f, scheme); err != nil {
				p.refuse(w, r, f, rule, err)
				return
			}
			// The request as the guest sent it, kept for Admit, for the
			// values, and for the record: after substitution it could
			// carry a value.
			seen := Request{Flow: f, Method: r.Method, Host: r.Host, URL: cloneURL(r.URL), Header: r.Header.Clone()}
			info := &requestInfo{path: seen.URL.Path}
			r = r.WithContext(context.WithValue(r.Context(), requestInfoKey{}, info))
			if p.Admit != nil {
				if err := p.Admit(r.Context(), seen); err != nil {
					p.refuse(w, r, f, rule, err)
					return
				}
			}
			applied, err := p.substitute(r.Context(), r, seen, f)
			info.applied = applied
			if err != nil {
				p.refuse(w, r, f, rule, err)
				return
			}
			proxy.ServeHTTP(w, r)
		}),
		ReadHeaderTimeout: 30 * time.Second,
		BaseContext:       func(net.Listener) context.Context { return ctx },
		// The server serves this one connection: once it is closed the
		// listener ends, and with it Serve and this goroutine.
		ConnState: func(_ net.Conn, state http.ConnState) {
			if state == http.StateClosed || state == http.StateHijacked {
				_ = listener.Close()
			}
		},
	}
	_ = server.Serve(listener)
}

// validateAuthority binds an inspected request to the destination whose flow
// was authorized. TLS authenticates that destination, but a different HTTP
// authority could select another tenant at the same upstream address.
func validateAuthority(r *http.Request, f vmnet.Flow, scheme string) error {
	expected := normalizeName(f.Host)
	if expected == "" {
		expected = f.Dst.Addr().Unmap().String()
	}
	matches := func(authority string) bool {
		u, err := url.Parse("//" + authority)
		if err != nil || u.Host == "" || u.User != nil || u.Path != "" || u.RawQuery != "" || u.Fragment != "" || strings.HasSuffix(u.Host, ":") {
			return false
		}
		host := normalizeName(u.Hostname())
		if addr, err := netip.ParseAddr(host); err == nil {
			host = addr.Unmap().String()
		}
		port := uint64(80)
		if scheme == "https" {
			port = 443
		}
		if s := u.Port(); s != "" {
			port, err = strconv.ParseUint(s, 10, 16)
			if err != nil {
				return false
			}
		}
		return host == expected && port == uint64(f.Dst.Port())
	}
	if !matches(r.Host) || (r.URL.Host != "" && !matches(r.URL.Host)) || (r.URL.Scheme != "" && !strings.EqualFold(r.URL.Scheme, scheme)) {
		return fmt.Errorf("HTTP authority does not match the flow destination: %w", vmnet.ErrDenied)
	}
	// net/http uses the absolute URL's authority as r.Host when present.
	// Normalize both forms so admission and forwarding see one authority.
	r.Host = net.JoinHostPort(expected, strconv.Itoa(int(f.Dst.Port())))
	if r.URL.Host != "" {
		r.URL.Host = r.Host
	}
	return nil
}

// refuse answers a request the policy did not forward: 403 when it was
// denied, 502 when deciding on it or reaching the upstream failed, either
// way on record.
func (p *Policy) refuse(w http.ResponseWriter, r *http.Request, f vmnet.Flow, rule string, err error) {
	status := http.StatusBadGateway
	if _, denied := deniedReason(r, err); denied {
		status = http.StatusForbidden
	}
	p.recordRequest(f, rule, r, status, err)
	w.WriteHeader(status)
}

// deniedReason reports whether a request was refused, by err or by a token
// found as its body streamed, and why.
func deniedReason(r *http.Request, err error) (string, bool) {
	if errors.Is(err, vmnet.ErrDenied) {
		return err.Error(), true
	}
	if r != nil {
		if info, ok := r.Context().Value(requestInfoKey{}).(*requestInfo); ok {
			for _, a := range info.applied {
				if reason := a.denied.Load(); reason != nil {
					return *reason, true
				}
			}
		}
	}
	return "", false
}

// upstreamTransport dials the real destination the way dial does, so
// resolution and the deny ranges apply to inspected flows too.
func (p *Policy) upstreamTransport(f vmnet.Flow) http.RoundTripper {
	tlsConfig := &tls.Config{ServerName: normalizeName(f.Host)}
	if p.UpstreamTLS != nil {
		tlsConfig = p.UpstreamTLS.Clone()
		if tlsConfig.ServerName == "" {
			tlsConfig.ServerName = normalizeName(f.Host)
		}
	}
	return &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			conn, _, err := p.dial(ctx, f, false) // inspected flows come from Rules
			return conn, err
		},
		TLSClientConfig:   tlsConfig,
		ForceAttemptHTTP2: true,
		DisableKeepAlives: true,
	}
}

type requestInfoKey struct{}

// requestInfo is what an inspected request records besides its outcome.
type requestInfo struct {
	path    string
	applied []*applied
}

// applied is one token's substitution in one request. used is set when the
// token was found and replaced anywhere, including in a body that streamed
// out after the headers; denied carries the reason when finding the token
// refused the request instead.
type applied struct {
	name   string
	used   atomic.Bool
	denied atomic.Pointer[string]
}

func usedNames(list []*applied) []string {
	var names []string
	for _, a := range list {
		if a.used.Load() {
			names = append(names, a.name)
		}
	}
	return names
}

// recordRequest records one inspected request as an Event.
func (p *Policy) recordRequest(f vmnet.Flow, rule string, r *http.Request, status int, err error) {
	ev := Event{
		Time: time.Now(), Guest: f.Guest, Proto: vm.TCP, Src: f.Src, Dst: f.Dst, Host: f.Host,
		Decision: Allowed, Rule: rule, Status: status, Err: err,
	}
	if reason, denied := deniedReason(r, err); denied {
		ev.Decision, ev.Reason, ev.Err = Denied, reason, nil
	}
	if r != nil {
		ev.Method, ev.Path = r.Method, r.URL.Path
		if info, ok := r.Context().Value(requestInfoKey{}).(*requestInfo); ok {
			ev.Path, ev.Injections = info.path, usedNames(info.applied)
		}
	}
	p.record(ev)
}

// substitute replaces, in r, the tokens of the injections that apply to
// it; seen is the request as the guest sent it. A value is read only once
// its token is found. An injection that refuses the request ends the pass
// with an error wrapping vmnet.ErrDenied.
func (p *Policy) substitute(ctx context.Context, r *http.Request, seen Request, f vmnet.Flow) ([]*applied, error) {
	var list []*applied
	for _, inj := range p.injectionsFor(f.Egress) {
		if inj.Value == nil || !scopeApplies(inj.Hosts, inj.Methods, inj.Paths, inj.Name == "", f, seen.Method, seen.URL.Path) {
			continue
		}
		token := p.tokenOf(inj)
		if token == "" {
			continue
		}
		a := &applied{name: inj.label()}
		list = append(list, a)
		value := sync.OnceValues(func() (string, error) { return inj.Value(ctx, seen) })
		if err := replaceRequest(r, token, value, inj.In, a); err != nil {
			return list, err
		}
	}
	return list, nil
}

func matchesPath(patterns []string, p string) bool {
	for _, pattern := range patterns {
		if ok, err := path.Match(pattern, p); err == nil && ok {
			return true
		}
	}
	return false
}

func cloneURL(u *url.URL) *url.URL {
	c := *u
	return &c
}

// bufferedBodyLimit is the largest body rewritten in memory; larger bodies
// are rewritten as they stream, which sends them chunked.
const bufferedBodyLimit = 1 << 20

// replaceRequest replaces token in the chosen parts of the request with
// the value resolve returns, reading it only once a token is found. An
// error wrapping vmnet.ErrDenied from resolve refuses the request and is
// returned; any other error, or an empty value, leaves every occurrence as
// it is. a is marked used when anything was replaced, including later, as
// a streamed body passes, and denied when a token there refused it.
func replaceRequest(r *http.Request, token string, resolve func() (string, error), in []Placement, a *applied) error {
	if len(in) == 0 {
		in = Placements
	}
	// value resolves once: the replacement and whether to use it, or the
	// refusal.
	value := func() (string, bool, error) {
		v, err := resolve()
		switch {
		case errors.Is(err, vmnet.ErrDenied):
			return "", false, fmt.Errorf("%s: %w", a.name, err)
		case err != nil || v == "":
			return "", false, nil
		}
		return v, true, nil
	}
	for _, place := range in {
		switch place {
		case InHeader:
			for name, values := range r.Header {
				for i, v := range values {
					if !strings.Contains(v, token) {
						continue
					}
					val, ok, err := value()
					if err != nil || !ok {
						return err
					}
					r.Header[name][i] = strings.ReplaceAll(v, token, val)
					a.used.Store(true)
				}
			}
		case InQuery:
			if strings.Contains(r.URL.RawQuery, token) {
				val, ok, err := value()
				if err != nil || !ok {
					return err
				}
				// The token stands in a query value; the value takes its
				// place encoded as one.
				r.URL.RawQuery = strings.ReplaceAll(r.URL.RawQuery, token, url.QueryEscape(val))
				a.used.Store(true)
			}
		case InPath:
			if strings.Contains(r.URL.Path, token) {
				val, ok, err := value()
				if err != nil || !ok {
					return err
				}
				r.URL.Path = strings.ReplaceAll(r.URL.Path, token, val)
				r.URL.RawPath = ""
				a.used.Store(true)
			}
		case InBody:
			if r.Body == nil || r.Body == http.NoBody {
				continue
			}
			if r.ContentLength >= 0 && r.ContentLength <= bufferedBodyLimit {
				data, err := io.ReadAll(io.LimitReader(r.Body, bufferedBodyLimit+1))
				_ = r.Body.Close()
				r.Body = io.NopCloser(bytes.NewReader(data))
				if err != nil || !bytes.Contains(data, []byte(token)) {
					continue
				}
				val, ok, err := value()
				if err != nil || !ok {
					return err
				}
				data = bytes.ReplaceAll(data, []byte(token), []byte(val))
				r.Body = io.NopCloser(bytes.NewReader(data))
				r.ContentLength = int64(len(data))
				r.Header.Set("Content-Length", fmt.Sprint(len(data)))
				a.used.Store(true)
				continue
			}
			// Unknown or large length: rewrite as it streams. The length
			// may change, so the upstream gets it chunked.
			r.Body = &replacingReader{src: r.Body, token: []byte(token), value: value, applied: a}
			r.ContentLength = -1
			r.Header.Del("Content-Length")
		}
	}
	return nil
}

// replacingReader replaces token in a stream with the value resolved on
// the first match, holding back the tail that might begin a token split
// across reads. When the value cannot be resolved the stream passes
// unchanged from then on; when the token refuses the request the read
// fails, which aborts it.
type replacingReader struct {
	src     io.ReadCloser
	token   []byte
	value   func() (string, bool, error)
	applied *applied

	val   []byte
	state int // 0: unresolved, 1: replacing, 2: passing through
	err   error
	buf   []byte
	skip  int // leading bytes of buf already scanned or written, not to scan again
	eof   bool
}

func (r *replacingReader) replacement() ([]byte, bool) {
	if r.state == 0 {
		switch v, ok, err := r.value(); {
		case err != nil:
			reason := err.Error()
			r.applied.denied.Store(&reason)
			r.err, r.state = err, 2
		case ok:
			r.val, r.state = []byte(v), 1
		default:
			r.state = 2
		}
	}
	return r.val, r.state == 1
}

func (r *replacingReader) Read(p []byte) (int, error) {
	for {
		if r.err != nil {
			return 0, r.err
		}
		// Emit what is safe: everything but a possible token prefix at the
		// end, unless the source is done or nothing is replaced anymore.
		safe := len(r.buf)
		if r.state != 2 && !r.eof {
			safe = max(r.skip, len(r.buf)-len(r.token)+1)
			if i := bytes.Index(r.buf[r.skip:], r.token); i >= 0 {
				if val, ok := r.replacement(); ok {
					j := r.skip + i
					r.buf = append(append(r.buf[:j:j], val...), r.buf[j+len(r.token):]...)
					r.applied.used.Store(true)
					r.skip = j + len(val)
				}
				continue
			}
		} else if r.state != 2 && bytes.Contains(r.buf[r.skip:], r.token) {
			if val, ok := r.replacement(); ok {
				r.buf = append(r.buf[:r.skip:r.skip], bytes.ReplaceAll(r.buf[r.skip:], r.token, val)...)
				r.applied.used.Store(true)
			}
			r.skip = len(r.buf) // everything is written; a value holding the token is not scanned again
			continue
		}
		if safe > 0 {
			n := copy(p, r.buf[:safe])
			r.buf = r.buf[n:]
			r.skip = max(0, r.skip-n)
			return n, nil
		}
		if r.eof {
			return 0, io.EOF
		}
		chunk := make([]byte, 32*1024)
		n, err := r.src.Read(chunk)
		r.buf = append(r.buf, chunk[:n]...)
		if err == io.EOF {
			r.eof = true
		} else if err != nil {
			return 0, err
		}
	}
}

func (r *replacingReader) Close() error { return r.src.Close() }

// peekedConn is a conn whose first bytes were peeked.
type peekedConn struct {
	net.Conn
	Reader *bufio.Reader
}

func (c *peekedConn) Read(p []byte) (int, error) { return c.Reader.Read(p) }

// oneConnListener hands one conn to an http.Server, then blocks until it is
// closed, which the server's ConnState hook does once the conn is done.
type oneConnListener struct {
	conn net.Conn
	once sync.Once
	done chan struct{}
}

func (l *oneConnListener) Accept() (net.Conn, error) {
	var conn net.Conn
	l.once.Do(func() { conn = l.conn })
	if conn != nil {
		return conn, nil
	}
	<-l.done
	return nil, net.ErrClosed
}

func (l *oneConnListener) Close() error {
	select {
	case <-l.done:
	default:
		close(l.done)
	}
	return nil
}

func (l *oneConnListener) Addr() net.Addr { return l.conn.LocalAddr() }
