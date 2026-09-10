package manifest

import (
	"reflect"
	"strings"
	"testing"
)

func TestResolveNetworkTypes(t *testing.T) {
	host := HostInput{System: "x86_64-linux"}
	forward := []ForwardPort{{Host: "127.0.0.1:8080", Guest: ":80"}}
	resolve := func(t *testing.T, inputs ...NetworkInput) []QEMUNetDevice {
		t.Helper()
		devices, err := resolveNetwork("", inputs, nil, host, "mmio", CPUCount{})
		if err != nil {
			t.Fatalf("resolveNetwork: %v", err)
		}
		return devices
	}

	t.Run("user keeps slirp forwards", func(t *testing.T) {
		dev := resolve(t, NetworkInput{Forward: forward})[0]
		if dev.Backend != "user" || dev.Managed || dev.MacAddress != defaultNetworkMAC {
			t.Fatalf("device = %+v", dev)
		}
		if want := []string{"hostfwd=tcp:127.0.0.1:8080-:80"}; !reflect.DeepEqual(dev.NetdevOptions, want) {
			t.Fatalf("netdev options = %v, want %v", dev.NetdevOptions, want)
		}
	})

	t.Run("virtle is managed with forwards on the port", func(t *testing.T) {
		dev := resolve(t, NetworkInput{Type: NetworkTypeVirtle, Forward: forward})[0]
		if dev.Backend != "stream" || !dev.Managed || dev.ID != defaultNetworkID || len(dev.NetdevOptions) != 0 {
			t.Fatalf("device = %+v", dev)
		}
		if dev.MacAddress != "" {
			t.Fatalf("MAC = %q, want the network to allocate one", dev.MacAddress)
		}
		if want := []HotplugForward{{Proto: "tcp", Host: "127.0.0.1:8080", Guest: ":80"}}; !reflect.DeepEqual(dev.Forward, want) {
			t.Fatalf("forwards = %+v, want %+v", dev.Forward, want)
		}
		if dev := resolve(t, NetworkInput{Type: NetworkTypeVirtle, MAC: defaultNetworkMAC})[0]; dev.MacAddress != "" {
			t.Fatalf("the default MAC was requested: %q", dev.MacAddress)
		}
		if dev := resolve(t, NetworkInput{Type: NetworkTypeVirtle, MAC: "02:11:22:33:44:55"})[0]; dev.MacAddress != "02:11:22:33:44:55" {
			t.Fatalf("a chosen MAC was dropped: %q", dev.MacAddress)
		}
	})

	t.Run("tap names the host device", func(t *testing.T) {
		dev := resolve(t, NetworkInput{Type: NetworkTypeTAP, Tap: "tap0"})[0]
		if dev.Backend != "tap" || dev.Managed {
			t.Fatalf("device = %+v", dev)
		}
		if want := []string{"ifname=tap0", "script=no", "downscript=no"}; !reflect.DeepEqual(dev.NetdevOptions, want) {
			t.Fatalf("netdev options = %v, want %v", dev.NetdevOptions, want)
		}
	})

	for name, tc := range map[string]struct {
		inputs []NetworkInput
		want   string
	}{
		"unknown type":       {[]NetworkInput{{Type: "bridge"}}, "type must be one of user, virtle, or tap"},
		"tap without name":   {[]NetworkInput{{Type: NetworkTypeTAP}}, "tap is required"},
		"tap with forwards":  {[]NetworkInput{{Type: NetworkTypeTAP, Tap: "tap0", Forward: forward}}, "forward is not supported on a tap network"},
		"tap name with meta": {[]NetworkInput{{Type: NetworkTypeTAP, Tap: "tap0,script=/x"}}, "not an interface name"},
		"tap on user type":   {[]NetworkInput{{Tap: "tap0"}}, "tap applies to type tap only"},
		"two virtle":         {[]NetworkInput{{Type: NetworkTypeVirtle}, {ID: "b", Type: NetworkTypeVirtle}}, "one virtle network"},
		"virtle guest forward": {[]NetworkInput{{Type: NetworkTypeVirtle, Forward: []ForwardPort{{From: "guest", Host: "127.0.0.1:53", Guest: ":53"}}}},
			"from guest is not supported on a virtle network"},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := resolveNetwork("", tc.inputs, nil, host, "mmio", CPUCount{})
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %v, want %q", err, tc.want)
			}
		})
	}
}
