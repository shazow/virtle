package egress

import (
	"context"
	"net/http"
	"net/url"
	"slices"
	"strings"

	"github.com/shazow/virtle/vmnet"
)

// Injection replaces a token in inspected requests with a value computed
// as the request passes, or refuses the request that carries it. Anything
// the guest should not hold, cannot know at boot, or must not be able to
// predict (a credential, a nonce, an identity, a value the host looks up
// when the request is made) is written as the token and filled in on the
// way out.
//
// A named Injection is one issued to guests, which is what a secret is:
// its token is generated when none is given and reaches the guest through
// GuestEnv, a guest's vm.Egress.Secrets names the ones it may use, and it
// must name the hosts its value may be sent to. An unnamed Injection has a
// fixed token every guest may write, such as "$VIRTLE_RANDOM$".
type Injection struct {
	// Name identifies the injection to guests (GuestEnv, vm.Egress.Secrets)
	// and in Events. Empty for a token every guest knows.
	Name string
	// Token is the literal the guest writes. Empty means one generated from
	// Name, once per Policy. It must be a string the guest's client sends
	// unencoded.
	Token string
	// Value computes the replacement. It is called once per request the
	// token appears in, only then, and every occurrence in that request
	// gets the same value. An error wrapping vmnet.ErrDenied refuses the
	// request (the guest gets 403); any other error, or an empty value,
	// leaves the token as the guest sent it.
	Value func(ctx context.Context, req Request) (string, error)
	// Hosts are name patterns (as for Rule.Hosts) of the destinations the
	// injection applies to. Required for a named Injection, whose value is
	// never sent anywhere else; empty on an unnamed one means every
	// inspected destination.
	Hosts []string
	// Methods and Paths further limit the requests; empty means any. Paths
	// are path.Match patterns against the URL path.
	Methods []string
	Paths   []string
	// In limits where the token is replaced; empty means everywhere.
	In []Placement
}

// label is how Events and errors refer to the injection.
func (inj *Injection) label() string {
	if inj.Name != "" {
		return inj.Name
	}
	return inj.Token
}

// Request is an inspected request as the guest sent it, before any token
// is replaced: what Policy.Admit decides on and what an Injection.Value
// computes from. URL and Header are copies.
type Request struct {
	Flow   vmnet.Flow
	Method string
	// Host is the validated HTTP authority, normalized to the flow's host
	// and port before admission or injection.
	Host   string
	URL    *url.URL
	Header http.Header
}

// scopeApplies reports whether a flow and request fall within a host,
// method, and path scope. Empty hosts match every host when anyHost is
// set and none otherwise.
func scopeApplies(hosts, methods, paths []string, anyHost bool, f vmnet.Flow, method, urlPath string) bool {
	if len(hosts) == 0 {
		if !anyHost {
			return false
		}
	} else if !matchesAny(hosts, f) {
		return false
	}
	if len(methods) != 0 && !slices.ContainsFunc(methods, func(m string) bool { return strings.EqualFold(m, method) }) {
		return false
	}
	if len(paths) != 0 && !matchesPath(paths, urlPath) {
		return false
	}
	return true
}

func matchesAny(patterns []string, f vmnet.Flow) bool {
	for _, h := range patterns {
		if matchPattern(h, nil, f) {
			return true
		}
	}
	return false
}
