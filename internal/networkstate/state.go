// Package networkstate carries the host-side state needed to resume a guest.
// It is an internal checkpoint contract, not a network configuration API.
package networkstate

import "net/netip"

type Binding struct {
	Name string     `json:"name"`
	Addr netip.Addr `json:"addr"`
}

// State contains names and issued placeholders, never the secret values.
type State struct {
	FakeIPRange netip.Prefix      `json:"fakeIPRange"`
	Bindings    []Binding         `json:"bindings,omitempty"`
	Tokens      map[string]string `json:"tokens,omitempty"`
}

type Network interface {
	SaveNetworkState() State
	RestoreNetworkState(State) error
}

type Egress interface {
	SaveTokens() map[string]string
	RestoreTokens(tokens map[string]string, replace bool) error
}
