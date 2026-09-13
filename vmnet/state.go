package vmnet

import "net/netip"

// DNSBinding associates a resolved name with the synthetic address a guest
// may still hold in its DNS cache when suspended.
type DNSBinding struct {
	Name string     `json:"name"`
	Addr netip.Addr `json:"addr"`
}

// NetworkState contains the host-side state needed to resume a guest's
// existing network identity. It preserves synthetic DNS names and
// issued secret tokens; secret values and their permissions remain in the
// current egress policy. Established transport connections are not saved.
type NetworkState struct {
	FakeIPRange netip.Prefix      `json:"fakeIPRange"`
	Bindings    []DNSBinding      `json:"bindings,omitempty"`
	Tokens      map[string]string `json:"tokens,omitempty"`
}

// StatefulNetwork is an optional Network capability for suspend and resume.
// The backend stops the guest before saving and restores its state before
// attaching the saved NIC. RestoreNetworkState must reject conflicting state
// without changing bindings or tokens already used by attached guests.
type StatefulNetwork interface {
	Network
	SaveNetworkState() NetworkState
	RestoreNetworkState(NetworkState) error
}
