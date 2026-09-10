package userspace

import (
	"net/netip"
	"strings"
	"sync"
)

// DefaultFakeIPRange is where DNSFakeIP answers come from when
// Config.FakeIPRange is zero: the benchmarking range, which no real
// destination uses.
var DefaultFakeIPRange = netip.MustParsePrefix("198.18.0.0/15")

// fakeIPTable hands out one synthetic address per name a guest resolves,
// so a flow to that address carries the name the guest meant and the
// Egress resolves it when it dials. Names never leave the process at
// resolution time, and a name keeps its address for the network's life.
type fakeIPTable struct {
	prefix netip.Prefix

	mu     sync.Mutex
	byName map[string]netip.Addr
	byAddr map[netip.Addr]string
	next   netip.Addr
}

func newFakeIPTable(prefix netip.Prefix) *fakeIPTable {
	return &fakeIPTable{
		prefix: prefix,
		byName: make(map[string]netip.Addr),
		byAddr: make(map[netip.Addr]string),
		next:   prefix.Masked().Addr().Next(),
	}
}

// fakeName is the key form of a name: lowercase, no trailing dot.
func fakeName(name string) string {
	return strings.ToLower(strings.TrimSuffix(name, "."))
}

// addr returns the name's address, allocating one on first use; ok is false
// once the range is exhausted.
func (t *fakeIPTable) addr(name string) (netip.Addr, bool) {
	name = fakeName(name)
	t.mu.Lock()
	defer t.mu.Unlock()
	if a, ok := t.byName[name]; ok {
		return a, true
	}
	a := t.next
	if !t.prefix.Contains(a) || a == lastAddr(t.prefix) {
		return netip.Addr{}, false
	}
	t.next = a.Next()
	t.byName[name] = a
	t.byAddr[a] = name
	return a, true
}

// name returns the name an address was handed out for.
func (t *fakeIPTable) name(a netip.Addr) (string, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	name, ok := t.byAddr[a]
	return name, ok
}

// contains reports whether an address is in the synthetic range, whether
// or not it has been handed out.
func (t *fakeIPTable) contains(a netip.Addr) bool {
	return t.prefix.Contains(a)
}
