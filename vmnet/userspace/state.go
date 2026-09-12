package userspace

import (
	"fmt"
	"net"
	"net/netip"
	"time"

	"github.com/miekg/dns"

	"github.com/shazow/virtle/vmnet"
)

// tokenState is an optional egress capability for preserving tokens while
// keeping the current policy's secret values and injection permissions.
type tokenState interface {
	SaveTokens() map[string]string
	RestoreTokens(tokens map[string]string, replace bool) error
}

// SaveNetworkState captures synthetic names and issued tokens for suspend.
// The caller must stop the guest before saving its host-side state.
func (n *Network) SaveNetworkState() vmnet.NetworkState {
	var state vmnet.NetworkState
	if t := n.fakeIPs; t != nil {
		t.mu.Lock()
		state.FakeIPRange = t.prefix
		for e := t.used.Front(); e != nil; e = e.Next() {
			binding := e.Value.(*fakeIPEntry)
			state.Bindings = append(state.Bindings, vmnet.DNSBinding{Name: binding.name, Addr: binding.addr})
		}
		t.mu.Unlock()
	}
	if policy, ok := n.egress.(tokenState); ok {
		state.Tokens = policy.SaveTokens()
	}
	return state
}

// RestoreNetworkState restores a checkpoint before attaching the saved NIC.
// Existing bindings may be shared with other guests, so conflicts fail rather
// than replacing their names or issued tokens.
func (n *Network) RestoreNetworkState(state vmnet.NetworkState) error {
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.closed {
		return net.ErrClosed
	}
	if n.fakeIPs == nil {
		if state.FakeIPRange.IsValid() || len(state.Bindings) != 0 {
			return fmt.Errorf("cannot restore synthetic DNS state on a forwarding network")
		}
		return n.restoreTokens(state.Tokens)
	}
	t := n.fakeIPs
	t.mu.Lock()
	defer t.mu.Unlock()
	if state.FakeIPRange != t.prefix {
		return fmt.Errorf("saved synthetic DNS range %s differs from %s", state.FakeIPRange, t.prefix)
	}
	names := make(map[string]bool, len(state.Bindings))
	addrs := make(map[netip.Addr]bool, len(state.Bindings))
	for _, b := range state.Bindings {
		_, validName := dns.IsDomainName(dns.Fqdn(b.Name))
		if b.Name == "" || !validName || fakeName(b.Name) != b.Name || !t.prefix.Contains(b.Addr) || b.Addr == t.prefix.Addr() || b.Addr == lastAddr(t.prefix) {
			return fmt.Errorf("invalid saved synthetic DNS binding %q at %s", b.Name, b.Addr)
		}
		if names[b.Name] || addrs[b.Addr] {
			return fmt.Errorf("duplicate saved synthetic DNS binding %q at %s", b.Name, b.Addr)
		}
		names[b.Name], addrs[b.Addr] = true, true
		if e := t.byName[b.Name]; e != nil && e.Value.(*fakeIPEntry).addr != b.Addr {
			return fmt.Errorf("saved DNS name %q has a different address on this network", b.Name)
		}
		if e := t.byAddr[b.Addr]; e != nil && e.Value.(*fakeIPEntry).name != b.Name {
			return fmt.Errorf("saved DNS address %s belongs to another name on this network", b.Addr)
		}
	}
	if err := n.restoreTokens(state.Tokens); err != nil {
		return err
	}
	// The guest's cache clock may have stopped while suspended. Grant a
	// full TTL from resume, regardless of how long the host was down.
	expires := t.now().Add(time.Duration(dnsTTL) * time.Second)
	for i := len(state.Bindings) - 1; i >= 0; i-- {
		b := state.Bindings[i]
		e := t.byName[b.Name]
		if e == nil {
			e = t.used.PushFront(&fakeIPEntry{name: b.Name, addr: b.Addr})
			t.byName[b.Name], t.byAddr[b.Addr] = e, e
		} else {
			t.used.MoveToFront(e)
		}
		e.Value.(*fakeIPEntry).expires = expires
		if b.Addr.Compare(t.next) >= 0 {
			t.next = b.Addr.Next()
		}
	}
	return nil
}

// restoreTokens is called with n.mu held after validating DNS state.
func (n *Network) restoreTokens(tokens map[string]string) error {
	if len(tokens) != 0 {
		policy, ok := n.egress.(tokenState)
		if !ok {
			return fmt.Errorf("the current egress cannot restore saved secret tokens")
		}
		// Check and merge under the policy's token lock so concurrent token
		// issuance cannot be overwritten on a network with attached guests.
		if err := policy.RestoreTokens(tokens, len(n.byAddr) == 0); err != nil {
			return err
		}
	}
	return nil
}

var _ vmnet.StatefulNetwork = (*Network)(nil)
