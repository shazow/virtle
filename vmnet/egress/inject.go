package egress

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"net/http"
	"net/url"

	"github.com/shazow/virtle/vmnet"
)

// Injection replaces a token in inspected requests with a value computed as
// the request passes. It is the general form of a Secret: anything the
// guest should not hold, cannot know at boot, or must not be able to
// predict (a credential, a nonce, an identity, a value fetched from the
// host when the request is made) is written as the token and filled in on
// the way out.
type Injection struct {
	// Token is the literal the guest writes, such as "$VIRTLE_RANDOM$". It
	// must be a string the guest's client sends unencoded.
	Token string
	// Value computes the replacement. It is called once per request the
	// token appears in, only then, and every occurrence in that request
	// gets the same value. An error or an empty value leaves the token as
	// the guest sent it.
	Value func(ctx context.Context, req Request) (string, error)
	// Hosts are name patterns (as for Rule.Hosts) of the destinations the
	// injection applies to; empty means every inspected destination.
	Hosts []string
	// Methods and Paths further limit the requests; empty means any. Paths
	// are path.Match patterns against the URL path.
	Methods []string
	Paths   []string
	// In limits where the token is replaced; empty means everywhere.
	In []Placement
}

// Request is the inspected request an Injection computes a value for, as
// the guest sent it and before any token is replaced. URL and Header are
// the request's own and must not be modified.
type Request struct {
	Flow   vmnet.Flow
	Method string
	URL    *url.URL
	Header http.Header
}

// Random returns an Injection.Value of n random bytes as hex, drawn for
// each request: Injection{Token: "$VIRTLE_RANDOM$", Value: Random(16)} gives
// a guest a nonce it can neither predict nor reuse.
func Random(n int) func(context.Context, Request) (string, error) {
	return func(context.Context, Request) (string, error) {
		b := make([]byte, n)
		if _, err := rand.Read(b); err != nil {
			return "", err
		}
		return hex.EncodeToString(b), nil
	}
}

// scopeApplies reports whether a flow and request fall within a host,
// method, and path scope. Empty hosts match every host when anyHost is
// set and none otherwise.
func scopeApplies(hosts, methods, paths []string, anyHost bool, f vmnet.Flow, r *http.Request) bool {
	if len(hosts) == 0 {
		if !anyHost {
			return false
		}
	} else if !matchesAny(hosts, f) {
		return false
	}
	if len(methods) != 0 && !containsFold(methods, r.Method) {
		return false
	}
	if len(paths) != 0 && !matchesPath(paths, r.URL.Path) {
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
