package egress

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"time"
)

const (
	caCertFile = "ca.pem"
	caKeyFile  = "ca-key.pem"
	// caValidity is how long a generated CA lasts; the guests that trust it
	// are rebuilt far more often.
	caValidity = 10 * 365 * 24 * time.Hour
	// leafValidity is how long a minted certificate lasts; leaves are
	// re-minted when less than leafRenewal remains.
	leafValidity = 7 * 24 * time.Hour
	leafRenewal  = time.Hour
	// maxLeaves bounds the minted certificates kept; past it the cache
	// starts over, and a name costs one more key generation.
	maxLeaves = 1024
)

// LoadOrCreateCA returns the certificate authority in dir, creating one
// (ca.pem and ca-key.pem, readable by the owner only) on first use. Guests
// whose traffic a Policy inspects must trust ca.pem; see Policy.CAPEM.
func LoadOrCreateCA(dir string) (tls.Certificate, error) {
	certPath, keyPath := filepath.Join(dir, caCertFile), filepath.Join(dir, caKeyFile)
	cert, err := tls.LoadX509KeyPair(certPath, keyPath)
	if err == nil {
		if cert.Leaf, err = x509.ParseCertificate(cert.Certificate[0]); err != nil {
			return tls.Certificate{}, fmt.Errorf("egress: parse %s: %w", certPath, err)
		}
		return cert, nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return tls.Certificate{}, fmt.Errorf("egress: load CA from %s: %w", dir, err)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return tls.Certificate{}, fmt.Errorf("egress: create CA directory: %w", err)
	}
	key, serial, err := newKeyAndSerial()
	if err != nil {
		return tls.Certificate{}, err
	}
	now := time.Now()
	template := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: "virtle egress CA", Organization: []string{"virtle"}},
		NotBefore:             now.Add(-time.Hour),
		NotAfter:              now.Add(caValidity),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
		BasicConstraintsValid: true,
		IsCA:                  true,
		MaxPathLen:            0,
		MaxPathLenZero:        true,
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		return tls.Certificate{}, fmt.Errorf("egress: create CA certificate: %w", err)
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return tls.Certificate{}, err
	}
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})
	if err := os.WriteFile(keyPath, keyPEM, 0o600); err != nil {
		return tls.Certificate{}, fmt.Errorf("egress: write CA key: %w", err)
	}
	if err := os.WriteFile(certPath, certPEM, 0o644); err != nil {
		return tls.Certificate{}, fmt.Errorf("egress: write CA certificate: %w", err)
	}
	leaf, err := x509.ParseCertificate(der)
	if err != nil {
		return tls.Certificate{}, err
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key, Leaf: leaf}, nil
}

// newKeyAndSerial makes the key pair and the serial number of a certificate.
func newKeyAndSerial() (*ecdsa.PrivateKey, *big.Int, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, nil, err
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 127))
	if err != nil {
		return nil, nil, err
	}
	return key, serial, nil
}

// CAPEM is the CA certificate in PEM form, for guests to trust; nil
// without a CA.
func (p *Policy) CAPEM() []byte {
	if len(p.CA.Certificate) == 0 {
		return nil
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: p.CA.Certificate[0]})
}

// certFor mints (and caches) a certificate for a name or address, signed by
// the CA, to present to a guest whose flow is inspected.
func (p *Policy) certFor(host string) (*tls.Certificate, error) {
	host = normalizeName(host)
	p.mu.Lock()
	defer p.mu.Unlock()
	if cert, ok := p.leaves[host]; ok && time.Until(cert.Leaf.NotAfter) > leafRenewal {
		return cert, nil
	}
	caLeaf := p.CA.Leaf
	if caLeaf == nil {
		var err error
		if caLeaf, err = x509.ParseCertificate(p.CA.Certificate[0]); err != nil {
			return nil, err
		}
	}
	key, serial, err := newKeyAndSerial()
	if err != nil {
		return nil, err
	}
	now := time.Now()
	template := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: host},
		NotBefore:    now.Add(-time.Hour),
		NotAfter:     now.Add(leafValidity),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	if ip := net.ParseIP(host); ip != nil {
		template.IPAddresses = []net.IP{ip}
	} else {
		template.DNSNames = []string{host}
	}
	der, err := x509.CreateCertificate(rand.Reader, template, caLeaf, &key.PublicKey, p.CA.PrivateKey)
	if err != nil {
		return nil, fmt.Errorf("egress: mint certificate for %s: %w", host, err)
	}
	leaf, err := x509.ParseCertificate(der)
	if err != nil {
		return nil, err
	}
	cert := &tls.Certificate{Certificate: [][]byte{der, p.CA.Certificate[0]}, PrivateKey: key, Leaf: leaf}
	if p.leaves == nil || len(p.leaves) >= maxLeaves {
		p.leaves = make(map[string]*tls.Certificate)
	}
	p.leaves[host] = cert
	return cert, nil
}
