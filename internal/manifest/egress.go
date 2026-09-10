package manifest

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/shazow/virtle/vmnet/egress"
)

// Egress is the resolved [egress] section: what the loader builds a
// vmnet/egress policy from, and what the guest's vm.Egress says.
type Egress struct {
	Allow   []EgressRule   `json:"allow,omitempty"`
	Deny    []EgressRule   `json:"deny,omitempty"`
	Secrets []EgressSecret `json:"secrets,omitempty"`
	// CADir holds the CA that signs inspected connections, resolved against
	// the working directory; it is created on first use.
	CADir string `json:"caDir"`
	// Inspects reports whether any allow entry inspects, which is what
	// needs the CA.
	Inspects bool `json:"inspects,omitempty"`
}

// EgressRule is one resolved allow or deny entry.
type EgressRule struct {
	Host    string `json:"host"`
	Ports   []int  `json:"ports,omitempty"`
	Inspect bool   `json:"inspect,omitempty"`
}

// EgressSecret is one resolved secret; From is "env:NAME" or "file:PATH"
// with the path already resolved.
type EgressSecret struct {
	Name    string   `json:"name"`
	From    string   `json:"from"`
	Hosts   []string `json:"hosts"`
	Methods []string `json:"methods,omitempty"`
	Paths   []string `json:"paths,omitempty"`
	In      []string `json:"in,omitempty"`
}

// ValueFunc reads the secret's value from its source when called, so the
// value is held only while a request needs it.
func (s EgressSecret) ValueFunc() func() (string, error) {
	scheme, ref, _ := strings.Cut(s.From, ":")
	switch scheme {
	case "env":
		return func() (string, error) {
			value, ok := os.LookupEnv(ref)
			if !ok {
				return "", fmt.Errorf("secret %s: environment variable %s is not set", s.Name, ref)
			}
			return value, nil
		}
	case "file":
		return func() (string, error) {
			data, err := os.ReadFile(ref)
			if err != nil {
				return "", fmt.Errorf("secret %s: %w", s.Name, err)
			}
			return strings.TrimRight(string(data), "\r\n"), nil
		}
	}
	return func() (string, error) { return "", fmt.Errorf("secret %s: unknown source %q", s.Name, s.From) }
}

var envNamePattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// egressPlacements are the accepted values of a secret's in list.
var egressPlacements = []string{"header", "query", "path", "body"}

// resolveEgress validates the [egress] section against a document whose
// defaults are applied and resolves its paths. Values are never read here.
func (m *Manifest) resolveEgress(d Document) (*Egress, error) {
	if d.Egress == nil {
		return nil, nil
	}
	if !declaresNetworkType(d.Networks, NetworkTypeVirtle) {
		return nil, fmt.Errorf("manifest.egress needs a network of type %s", NetworkTypeVirtle)
	}
	in := d.Egress
	e := &Egress{CADir: in.CADir}
	for i, r := range in.Allow {
		rule, err := resolveEgressRule(r.Host, r.Ports, fmt.Sprintf("manifest.egress.allow[%d]", i))
		if err != nil {
			return nil, err
		}
		rule.Inspect = r.Inspect
		e.Inspects = e.Inspects || r.Inspect
		e.Allow = append(e.Allow, rule)
	}
	for i, r := range in.Deny {
		rule, err := resolveEgressRule(r.Host, r.Ports, fmt.Sprintf("manifest.egress.deny[%d]", i))
		if err != nil {
			return nil, err
		}
		e.Deny = append(e.Deny, rule)
	}
	names := make(map[string]bool, len(in.Secrets))
	for i, s := range in.Secrets {
		field := fmt.Sprintf("manifest.egress.secrets[%d]", i)
		switch {
		case !envNamePattern.MatchString(s.Name):
			return nil, fmt.Errorf("%s.name %q must be an environment variable name", field, s.Name)
		case names[s.Name]:
			return nil, fmt.Errorf("%s.name %q is defined twice", field, s.Name)
		case len(s.Hosts) == 0:
			return nil, fmt.Errorf("%s.hosts is required: the hosts that may receive the value", field)
		case !e.Inspects:
			return nil, fmt.Errorf("%s: secrets are injected into inspected requests, and no allow entry sets inspect = true", field)
		}
		names[s.Name] = true
		scheme, ref, ok := strings.Cut(s.From, ":")
		switch {
		case !ok || ref == "":
			return nil, fmt.Errorf("%s.from must be env:NAME or file:PATH", field)
		case scheme == "env":
			if !envNamePattern.MatchString(ref) {
				return nil, fmt.Errorf("%s.from: %q is not an environment variable name", field, ref)
			}
		case scheme == "file":
			ref = m.resolvePath(ref)
		default:
			return nil, fmt.Errorf("%s.from: unknown source %q; use env:NAME or file:PATH", field, scheme)
		}
		for _, h := range s.Hosts {
			if err := egress.ValidPattern(h); err != nil {
				return nil, fmt.Errorf("%s.hosts: %w", field, err)
			}
		}
		for _, p := range s.In {
			if !containsString(egressPlacements, p) {
				return nil, fmt.Errorf("%s.in %q must be one of %s", field, p, strings.Join(egressPlacements, ", "))
			}
		}
		e.Secrets = append(e.Secrets, EgressSecret{
			Name: s.Name, From: scheme + ":" + ref, Hosts: s.Hosts, Methods: s.Methods, Paths: s.Paths, In: s.In,
		})
	}
	if e.CADir == "" {
		e.CADir = filepath.Join(m.ResolvedPersistenceStateDir(), "egress-ca")
	} else {
		e.CADir = m.resolvePath(e.CADir)
	}
	return e, nil
}

func resolveEgressRule(host string, ports []int, field string) (EgressRule, error) {
	if err := egress.ValidPattern(host); err != nil {
		return EgressRule{}, fmt.Errorf("%s.host: %w", field, err)
	}
	for _, p := range ports {
		if p <= 0 || p > 65535 {
			return EgressRule{}, fmt.Errorf("%s.ports: %d is not a port", field, p)
		}
	}
	return EgressRule{Host: host, Ports: ports}, nil
}

// declaresNetworkType reports whether any network entry has the type.
func declaresNetworkType(networks []NetworkInput, netType string) bool {
	for _, network := range networks {
		if network.Type == netType {
			return true
		}
	}
	return false
}

func containsString(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}
