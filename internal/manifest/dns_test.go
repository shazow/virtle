package manifest

import (
	"strings"
	"testing"
)

func TestNetworkDNSUpstream(t *testing.T) {
	for _, tc := range []struct {
		name string
		dns  *DNSInput
		want string
	}{
		{"omitted", nil, "host"},
		{"empty", &DNSInput{}, "host"},
		{"host", &DNSInput{Upstream: "host"}, "host"},
		{"host whitespace", &DNSInput{Upstream: " host "}, "host"},
		{"address whitespace", &DNSInput{Upstream: " 127.0.0.1:53 "}, "127.0.0.1:53"},
		{"IPv4", &DNSInput{Upstream: "127.0.0.1:5353"}, "127.0.0.1:5353"},
		{"IPv6", &DNSInput{Upstream: "[2001:0db8:0:0::1]:53"}, "[2001:db8::1]:53"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			devices, err := resolveNetwork("", []NetworkInput{{Type: NetworkTypeVirtle, DNS: tc.dns}}, nil, HostInput{System: "x86_64-linux"}, "mmio", CPUCount{})
			if err != nil {
				t.Fatal(err)
			}
			if got := devices[0].DNSUpstream; got != tc.want {
				t.Errorf("DNSUpstream = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestNetworkDNSDecodes(t *testing.T) {
	for name, input := range map[string]string{
		"manifest.toml": "[kernel]\npath = 'kernel'\n[[networks]]\ntype = 'virtle'\n[networks.dns]\nupstream = '[2001:0db8::1]:5353'\n",
		"manifest.json": `{"kernel":{"path":"kernel"},"networks":[{"type":"virtle","dns":{"upstream":"[2001:0db8::1]:5353"}}]}`,
	} {
		t.Run(name, func(t *testing.T) {
			doc, err := DecodeDocumentBytes([]byte(input), name)
			if err != nil {
				t.Fatal(err)
			}
			mf, err := doc.Manifest()
			if err != nil {
				t.Fatal(err)
			}
			if got := mf.QEMU.Devices.Network[0].DNSUpstream; got != "[2001:db8::1]:5353" {
				t.Errorf("DNSUpstream = %q", got)
			}
		})
	}
}

func TestNetworkDNSRejectsInvalidUpstream(t *testing.T) {
	for _, upstream := range []string{
		"resolver.example:53", "https://resolver.example/dns-query", "127.0.0.1", "127.0.0.1:",
		"127.0.0.1:0", "127.0.0.1:-1", "127.0.0.1:65536", "127.0.0.1:dns", "300.1.2.3:53",
		"::1:53", "[::1]", "[::1]:0", "host:53",
	} {
		t.Run(upstream, func(t *testing.T) {
			_, err := resolveNetwork("", []NetworkInput{{Type: NetworkTypeVirtle, DNS: &DNSInput{Upstream: upstream}}}, nil, HostInput{}, "mmio", CPUCount{})
			if err == nil || !strings.Contains(err.Error(), "manifest.networks[0].dns.upstream") {
				t.Fatalf("error = %v, want DNS upstream validation error", err)
			}
		})
	}
}

func TestNetworkDNSRequiresManagedLaunchNetwork(t *testing.T) {
	for _, networkType := range []string{"", NetworkTypeUser, NetworkTypeTAP, "bridge"} {
		t.Run("qemu/"+networkType, func(t *testing.T) {
			_, err := resolveNetwork("", []NetworkInput{{Type: networkType, DNS: &DNSInput{}}}, nil, HostInput{}, "mmio", CPUCount{})
			if err == nil || !strings.Contains(err.Error(), "dns applies to type virtle only") {
				t.Fatalf("error = %v", err)
			}
		})
	}
	for _, backend := range []string{BackendFirecracker, BackendCloudHypervisor} {
		t.Run(backend, func(t *testing.T) {
			_, err := tapNetworks(backend, []NetworkInput{{Type: NetworkTypeTAP, Tap: "tap0", DNS: &DNSInput{}}}, nil)
			if err == nil || !strings.Contains(err.Error(), "dns requires a virtle network") {
				t.Fatalf("error = %v", err)
			}
		})
	}
	for _, networkType := range []string{NetworkTypeUser, NetworkTypeTAP, NetworkTypeVirtle} {
		t.Run("hotplug/"+networkType, func(t *testing.T) {
			_, err := resolveNetworkHotplug(NetworkInput{Type: networkType, DNS: &DNSInput{}}, 0)
			if err == nil || !strings.Contains(err.Error(), "hotplug.networks[0].dns is not supported") {
				t.Fatalf("error = %v", err)
			}
		})
	}
}
