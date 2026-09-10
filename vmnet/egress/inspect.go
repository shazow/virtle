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
	"net/url"
	"path"
	"strings"
	"sync"
	"time"

	"github.com/shazow/virtle/vm"
	"github.com/shazow/virtle/vmnet"
)

// Placement is where in an HTTP request a secret's token is replaced.
type Placement string

const (
	InHeader Placement = "header" // any header value
	InQuery  Placement = "query"  // the URL query
	InPath   Placement = "path"   // the URL path
	InBody   Placement = "body"   // the request body
)

// Placements are every Placement, the default for a Secret without In.
var Placements = []Placement{InHeader, InQuery, InPath, InBody}

// Secret is a value a guest uses without holding: the guest gets Token,
// and an inspected request to a matching host has the token replaced by
// Value on its way out. Anywhere else the token is inert.
type Secret struct {
	// Name is the environment variable GuestEnv sets to the token, and the
	// name a vm.Egress.Secrets entry refers to.
	Name string
	// Token is the placeholder the guest holds; one is generated when it is
	// empty. It must be a string that survives HTTP unencoded.
	Token string
	// Value returns the real value when a request needs it, so it is never
	// held longer than the request and can come from a vault.
	Value func() (string, error)
	// Hosts are name patterns (as for Rule.Hosts) of the destinations the
	// secret may be sent to; it is never sent anywhere else.
	Hosts []string
	// Methods and Paths further limit the requests; empty means any. Paths
	// are path.Match patterns against the URL path.
	Methods []string
	Paths   []string
	// In limits where the token is replaced; empty means everywhere.
	In []Placement
}

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
// for each Secret it may hold, which is every Secret when e is nil and
// those e.Secrets names otherwise.
func (p *Policy) GuestEnv(e *vm.Egress) []string {
	env := make([]string, 0, len(p.Secrets))
	for _, s := range p.secretsFor(e) {
		env = append(env, s.Name+"="+p.tokenOf(s))
	}
	return env
}

// secretsFor lists the Secrets a guest with policy e may use.
func (p *Policy) secretsFor(e *vm.Egress) []*Secret {
	var secrets []*Secret
	for i := range p.Secrets {
		s := &p.Secrets[i]
		if e == nil || containsString(e.Secrets, s.Name) {
			secrets = append(secrets, s)
		}
	}
	return secrets
}

// tokenOf is the Secret's token, generating one on first use.
func (p *Policy) tokenOf(s *Secret) string {
	if s.Token != "" {
		return s.Token
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if token, ok := p.tokens[s.Name]; ok {
		return token
	}
	var random [16]byte
	if _, err := rand.Read(random[:]); err != nil {
		panic("egress: random source failed: " + err.Error())
	}
	token := "virtle_" + strings.ToUpper(s.Name) + "_" + hex.EncodeToString(random[:])
	if p.tokens == nil {
		p.tokens = make(map[string]string)
	}
	p.tokens[s.Name] = token
	return token
}

// Validate reports a Policy that cannot work: malformed patterns, a rule
// that inspects with no CA to mint certificates from, or a Secret without
// a name, a value, or hosts.
func (p *Policy) Validate() error {
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
	names := make(map[string]bool, len(p.Secrets))
	for i, s := range p.Secrets {
		switch {
		case s.Name == "":
			return fmt.Errorf("egress: secret %d has no name", i)
		case names[s.Name]:
			return fmt.Errorf("egress: secret %q is defined twice", s.Name)
		case s.Value == nil:
			return fmt.Errorf("egress: secret %q has no value", s.Name)
		case len(s.Hosts) == 0:
			return fmt.Errorf("egress: secret %q names no hosts it may be sent to", s.Name)
		}
		names[s.Name] = true
		for _, h := range s.Hosts {
			if err := ValidPattern(h); err != nil {
				return fmt.Errorf("egress: secret %q: %w", s.Name, err)
			}
		}
		for _, in := range s.In {
			if !containsPlacement(Placements, in) {
				return fmt.Errorf("egress: secret %q: unknown placement %q", s.Name, in)
			}
		}
	}
	return nil
}

// inspect serves an inspected flow: the guest gets one end of a pipe, and
// the other end is terminated (TLS with a minted certificate when the guest
// starts a handshake, plain HTTP otherwise) and reverse-proxied to the
// real destination with the guest's secret tokens replaced.
func (p *Policy) inspect(ctx context.Context, f vmnet.Flow, rule string) (net.Conn, error) {
	if len(p.CA.Certificate) == 0 {
		return nil, fmt.Errorf("egress: inspecting %s needs a CA: %w", f.Host, vmnet.ErrDenied)
	}
	guest, server := net.Pipe()
	go p.serveInspected(ctx, server, f, rule)
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
				host := hello.ServerName
				if host == "" {
					host = f.Host
				}
				return p.certFor(host)
			},
		})
	}
	target := &url.URL{Scheme: scheme, Host: f.Target()}
	proxy := &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.SetURL(target)
			pr.Out.Host = f.Host
			// The record keeps the path as the guest sent it: after
			// substitution it could carry a secret.
			info := &requestInfo{path: pr.In.URL.Path}
			info.secrets = p.substitute(pr.Out, f)
			pr.Out = pr.Out.WithContext(context.WithValue(pr.Out.Context(), requestInfoKey{}, info))
		},
		Transport: p.upstreamTransport(f),
		ModifyResponse: func(resp *http.Response) error {
			p.recordRequest(f, rule, resp.Request, resp.StatusCode, nil)
			return nil
		},
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			p.recordRequest(f, rule, r, http.StatusBadGateway, err)
			w.WriteHeader(http.StatusBadGateway)
		},
	}
	server := &http.Server{
		Handler:           proxy,
		ReadHeaderTimeout: 30 * time.Second,
		BaseContext:       func(net.Listener) context.Context { return ctx },
	}
	_ = server.Serve(&oneConnListener{conn: served, done: make(chan struct{})})
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
			conn, _, err := p.dial(ctx, f)
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
	secrets []string
}

// recordRequest records one inspected request as an Event.
func (p *Policy) recordRequest(f vmnet.Flow, rule string, r *http.Request, status int, err error) {
	ev := Event{
		Time: time.Now(), Guest: f.Guest, Proto: vm.TCP, Src: f.Src, Dst: f.Dst, Host: f.Host,
		Decision: Allowed, Rule: rule, Status: status, Err: err,
	}
	if r != nil {
		ev.Method, ev.Path = r.Method, r.URL.Path
		if info, ok := r.Context().Value(requestInfoKey{}).(*requestInfo); ok {
			ev.Path, ev.Secrets = info.path, info.secrets
		}
	}
	p.record(ev)
}

// substitute replaces the tokens of the secrets the request may carry and
// returns the names of those it replaced.
func (p *Policy) substitute(r *http.Request, f vmnet.Flow) []string {
	var used []string
	for _, s := range p.secretsFor(f.Egress) {
		if !secretApplies(s, f, r) {
			continue
		}
		value, err := s.Value()
		if err != nil {
			continue
		}
		token := p.tokenOf(s)
		if token == "" || value == "" {
			continue
		}
		if replaceRequest(r, token, value, s.In) {
			used = append(used, s.Name)
		}
	}
	return used
}

func secretApplies(s *Secret, f vmnet.Flow, r *http.Request) bool {
	matched := false
	for _, h := range s.Hosts {
		if matchPattern(h, nil, f) {
			matched = true
			break
		}
	}
	if !matched {
		return false
	}
	if len(s.Methods) != 0 && !containsFold(s.Methods, r.Method) {
		return false
	}
	if len(s.Paths) != 0 {
		matched = false
		for _, pattern := range s.Paths {
			if ok, err := path.Match(pattern, r.URL.Path); err == nil && ok {
				matched = true
				break
			}
		}
		if !matched {
			return false
		}
	}
	return true
}

// bufferedBodyLimit is the largest body rewritten in memory; larger bodies
// are rewritten as they stream, which sends them chunked.
const bufferedBodyLimit = 1 << 20

// replaceRequest replaces token with value in the chosen parts of the
// request and reports whether it found it anywhere.
func replaceRequest(r *http.Request, token, value string, in []Placement) bool {
	if len(in) == 0 {
		in = Placements
	}
	found := false
	for _, place := range in {
		switch place {
		case InHeader:
			for name, values := range r.Header {
				for i, v := range values {
					if strings.Contains(v, token) {
						r.Header[name][i] = strings.ReplaceAll(v, token, value)
						found = true
					}
				}
			}
		case InQuery:
			if strings.Contains(r.URL.RawQuery, token) {
				r.URL.RawQuery = strings.ReplaceAll(r.URL.RawQuery, token, value)
				found = true
			}
		case InPath:
			if strings.Contains(r.URL.Path, token) {
				r.URL.Path = strings.ReplaceAll(r.URL.Path, token, value)
				r.URL.RawPath = ""
				found = true
			}
		case InBody:
			if r.Body == nil || r.Body == http.NoBody {
				continue
			}
			if r.ContentLength >= 0 && r.ContentLength <= bufferedBodyLimit {
				data, err := io.ReadAll(io.LimitReader(r.Body, bufferedBodyLimit+1))
				_ = r.Body.Close()
				if err != nil {
					r.Body = io.NopCloser(bytes.NewReader(data))
					continue
				}
				if bytes.Contains(data, []byte(token)) {
					data = bytes.ReplaceAll(data, []byte(token), []byte(value))
					found = true
				}
				r.Body = io.NopCloser(bytes.NewReader(data))
				r.ContentLength = int64(len(data))
				r.Header.Set("Content-Length", fmt.Sprint(len(data)))
				continue
			}
			// Unknown or large length: rewrite as it streams. The length
			// may change, so the upstream gets it chunked.
			r.Body = &replacingReader{src: r.Body, token: []byte(token), value: []byte(value)}
			r.ContentLength = -1
			r.Header.Del("Content-Length")
			found = true // assumed: the body is not read here
		}
	}
	return found
}

// replacingReader replaces token with value in a stream, holding back the
// tail that might begin a token split across reads.
type replacingReader struct {
	src          io.ReadCloser
	token, value []byte
	buf          []byte
	eof          bool
}

func (r *replacingReader) Read(p []byte) (int, error) {
	for {
		// Emit what is safe: everything but a possible token prefix at the
		// end, unless the source is done.
		var safe int
		if !r.eof {
			safe = max(0, len(r.buf)-len(r.token)+1)
			if i := bytes.Index(r.buf, r.token); i >= 0 {
				r.buf = append(append(r.buf[:i:i], r.value...), r.buf[i+len(r.token):]...)
				continue
			}
		} else {
			r.buf = bytes.ReplaceAll(r.buf, r.token, r.value)
			safe = len(r.buf)
		}
		if safe > 0 {
			n := copy(p, r.buf[:safe])
			r.buf = r.buf[n:]
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

// oneConnListener hands one conn to an http.Server, then blocks until the
// server closes it.
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

func containsString(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

func containsFold(list []string, s string) bool {
	for _, v := range list {
		if strings.EqualFold(v, s) {
			return true
		}
	}
	return false
}

func containsPlacement(list []Placement, p Placement) bool {
	for _, v := range list {
		if v == p {
			return true
		}
	}
	return false
}
