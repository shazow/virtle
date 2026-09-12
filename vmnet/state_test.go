package vmnet_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/netip"
	"reflect"
	"testing"

	"github.com/shazow/virtle/vmnet"
	"github.com/shazow/virtle/vmnet/egress"
	"github.com/shazow/virtle/vmnet/userspace"
)

// checkpointNetwork shows that a consumer's network adapter can implement
// checkpoint methods using only public types while delegating its NICs.
type checkpointNetwork struct {
	*userspace.Network
}

func (n *checkpointNetwork) SaveNetworkState() vmnet.NetworkState {
	return n.Network.SaveNetworkState()
}

func (n *checkpointNetwork) RestoreNetworkState(state vmnet.NetworkState) error {
	return n.Network.RestoreNetworkState(state)
}

var _ vmnet.StatefulNetwork = (*checkpointNetwork)(nil)

func TestNetworkStateRoundTrip(t *testing.T) {
	n, err := userspace.New(userspace.Config{
		DNSUpstream: "127.0.0.1:53",
		Egress: &egress.Policy{Injections: []egress.Injection{
			{
				Name: "TOKEN", Hosts: []string{"api.test"},
				Value: func(context.Context, egress.Request) (string, error) { return "host-secret", nil },
			},
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer n.Close()
	const checkpoint = `{"fakeIPRange":"198.18.0.0/15","bindings":[{"name":"api.test","addr":"198.18.0.1"}],"tokens":{"TOKEN":"issued-token"}}`
	var saved vmnet.NetworkState
	if err := json.NewDecoder(bytes.NewBufferString(checkpoint)).Decode(&saved); err != nil {
		t.Fatal(err)
	}
	adapter := &checkpointNetwork{Network: n}
	var network vmnet.StatefulNetwork = adapter
	if err := network.RestoreNetworkState(saved); err != nil {
		t.Fatal(err)
	}
	want := vmnet.NetworkState{
		FakeIPRange: netip.MustParsePrefix("198.18.0.0/15"),
		Bindings: []vmnet.DNSBinding{
			{Name: "api.test", Addr: netip.MustParseAddr("198.18.0.1")},
		},
		Tokens: map[string]string{"TOKEN": "issued-token"},
	}
	if got := network.SaveNetworkState(); !reflect.DeepEqual(got, want) {
		t.Fatalf("saved state = %+v, want %+v", got, want)
	}
	encoded, err := json.Marshal(network.SaveNetworkState())
	if err != nil {
		t.Fatal(err)
	}
	if string(encoded) != checkpoint {
		t.Fatalf("encoded checkpoint = %s, want %s", encoded, checkpoint)
	}
}
