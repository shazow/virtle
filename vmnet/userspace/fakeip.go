package userspace

import (
	"container/list"
	"net/netip"
	"strings"
	"sync"
)

// DefaultFakeIPRange is where synthetic DNS answers come from when
// Config.FakeIPRange is zero: the benchmarking range, which no real
// destination uses.
var DefaultFakeIPRange = netip.MustParsePrefix("198.18.0.0/15")

// fakeIPTable hands out one synthetic address per name a guest resolves,
// so a flow to that address carries the name the guest meant and the
// Egress resolves it when it dials. Names never leave the process at
// resolution time. A name keeps its address until the range is spent; then
// the name that has gone longest without a lookup or a flow gives its
// address up, so a guest resolving names without end cannot exhaust the
// range for the others.
type fakeIPTable struct {
	prefix netip.Prefix

	mu     sync.Mutex
	byName map[string]*list.Element
	byAddr map[netip.Addr]*list.Element
	used   *list.List // *fakeIPEntry, most recently used first
	next   netip.Addr // the next address never handed out
}

type fakeIPEntry struct {
	name string
	addr netip.Addr
}

func newFakeIPTable(prefix netip.Prefix) *fakeIPTable {
	return &fakeIPTable{
		prefix: prefix,
		byName: make(map[string]*list.Element),
		byAddr: make(map[netip.Addr]*list.Element),
		used:   list.New(),
		next:   prefix.Masked().Addr().Next(),
	}
}

// fakeName is the key form of a name: lowercase, no trailing dot.
func fakeName(name string) string {
	return strings.ToLower(strings.TrimSuffix(name, "."))
}

// addr returns the name's address, allocating one on first use; ok is false
// only when the range holds no address at all.
func (t *fakeIPTable) addr(name string) (netip.Addr, bool) {
	name = fakeName(name)
	t.mu.Lock()
	defer t.mu.Unlock()
	if e, ok := t.byName[name]; ok {
		t.used.MoveToFront(e)
		return e.Value.(*fakeIPEntry).addr, true
	}
	a := t.next
	if t.prefix.Contains(a) && a != lastAddr(t.prefix) {
		t.next = a.Next()
	} else {
		oldest := t.used.Back()
		if oldest == nil {
			return netip.Addr{}, false
		}
		old := t.used.Remove(oldest).(*fakeIPEntry)
		delete(t.byName, old.name)
		delete(t.byAddr, old.addr)
		a = old.addr
	}
	e := t.used.PushFront(&fakeIPEntry{name: name, addr: a})
	t.byName[name] = e
	t.byAddr[a] = e
	return a, true
}

// name returns the name an address was handed out for, and counts the use.
func (t *fakeIPTable) name(a netip.Addr) (string, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	e, ok := t.byAddr[a]
	if !ok {
		return "", false
	}
	t.used.MoveToFront(e)
	return e.Value.(*fakeIPEntry).name, true
}

// contains reports whether an address is in the synthetic range, whether
// or not it has been handed out.
func (t *fakeIPTable) contains(a netip.Addr) bool {
	return t.prefix.Contains(a)
}
