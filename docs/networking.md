# Networking

virtle gives a guest one NIC. What is behind it is a choice made per backend,
in Go or in the manifest's `[[networks]]` entry:

| `type` | Go | Frames go to | Backends |
| --- | --- | --- | --- |
| `user` (default) | `qemu.User{}` | QEMU's built-in user networking (slirp): NAT and `hostfwd` port forwards inside QEMU | QEMU |
| `virtle` | a `vmnet.Network` on `qemu.Backend.Network` | a network virtle runs in userspace: fixed addresses, DHCP and DNS, host-side dialing, forwards, and an egress policy | QEMU |
| `tap` | `qemu.TAP{Name}`, `firecracker.TAP{Name}`, `cloudhypervisor.TAP{Name}` | a host TAP device the host kernel networks; the operator owns addressing, NAT, and forwards | QEMU, Firecracker, Cloud Hypervisor |

`user` stays the default until the virtle network reaches parity with it. On
QEMU any other `type` still reaches QEMU verbatim as its `-netdev` backend,
forwards and all, and `type = "tap"` without a `tap` name still leaves the
device and its ifup/ifdown scripts to QEMU, as before virtle knew the types.

## The virtle network

`vmnet/userspace` is an in-process network on gVisor's netstack. Every
attached machine is a port on one Ethernet segment with a fixed IPv4 address
and MAC (a static DHCP lease keyed by the MAC), the gateway serves DHCP and
DNS, guests on the same network reach each other, and every guest-initiated
TCP connection or UDP exchange is terminated in userspace and dialed on the
host through the network's **egress**. Nothing touches the host's network
configuration and no privilege is needed.

```go
network, err := userspace.New(userspace.Config{}) // 192.168.127.0/24, host DNS with synthetic guest addresses
defer network.Close()

b := &qemu.Backend{Network: network}
m, err := b.Start(ctx, &vm.Spec{
	Kernel: vm.Kernel{Path: "bzImage", Initrd: "initrd"},
	Ports:  []vm.Forward{{HostAddr: "127.0.0.1:2222", GuestAddr: ":22"}},
})

status, _ := m.(backend.StatusReporter).Status(ctx)
conn, err := network.DialContext(ctx, "tcp", status.Networks[0].Addr+":22") // no forward needed
```

- `Status.Networks` lists the guest's NICs; on a virtle network the entry is
  `Attached` and carries the guest's address, known before the guest boots.
- `Spec.Ports` are exposed on the port instead of becoming slirp `hostfwd`
  options. `Attach(vm.Forward)` and `Detach` on the machine expose and remove
  forwards at runtime with no hotplug ports involved; `Detach` also removes a
  forward given in `Spec.Ports`.
- `network.DialContext` dials a guest by address or by machine name.
- A suspended machine keeps its address and MAC and re-attaches with them on
  resume, so the lease its kernel holds stays valid. Its synthetic DNS
  bindings and issued secret tokens are saved too, including across CLI
  invocations, and restored before the NIC attaches. Saved bindings get a
  fresh TTL protection window because the guest's cache clock may have
  stopped. Conflicts with an active shared network fail resume.
- Closing a port ends its outgoing connections before releasing the address;
  a replacement guest establishes new flows under its own policy.
- The segment carries IPv4 only. The gateway answers ping; nothing forwards
  ICMP to the outside.

In a manifest:

```toml
[[networks]]
type = "virtle"
forward = [{ host = "127.0.0.1:2222", guest = ":22" }]
```

The loader builds the network and hands it to the backend, which owns it:
close the backend (it implements `io.Closer`) when its machines are done. The
network logs through the backend's `Logger`.

### Guest side

The guest needs a virtio-net driver and a DHCP client; the lease carries the
address, netmask, router, DNS server (the gateway), the segment MTU, and the
machine's name as the hostname. Nothing else is required.

## Egress policy

`vmnet/egress.Policy` is the standard egress: rules allow destinations by
name pattern, CIDR, or address, with optional ports; everything else is
refused before the guest sees a connection open (a TCP reset, an ICMP port
unreachable for UDP). Address ranges no flow may reach, loopback, link-local
(and the cloud metadata services there), multicast, are checked on what a
name resolves to as well as on addresses dialed directly, so a rebinding name
cannot get through. Every decision is an `Event` for a `Recorder`, or a
structured log line by default.

`Policy.Reach` says what a flow no rule matches may reach: nothing
(`ReachRules`, the zero value, an allowlist), the public internet
(`ReachInternet`), or anything the host can (`ReachAll`). `ReachInternet`
refuses every address in `egress.LocalPrefixes`, the private, carrier-grade
NAT, loopback, link-local, multicast, reserved, and documentation ranges of
both families, and every address the host itself holds, on what a name
resolves to as well as on addresses dialed directly. A rule is an explicit
decision and is not held to that, so a rule can still name a host on the
LAN under `ReachInternet`.

The gateway checks a DNS query against the guest's egress policy before
contacting upstream DNS. Positive A answers become synthetic addresses;
a flow to one carries the requested name, which the egress resolves again
after approving the connection. Both use the same configured DNS upstream.
Negative answers such as NXDOMAIN reach the guest. AAAA answers are empty
because the guest segment carries IPv4; PTR queries for synthetic addresses
are answered locally. Ordinary records such as TXT, CNAME, MX, NS, and SRV
are proxied after authorization.

Each synthetic binding stays protected for at least its advertised TTL
after a lookup or use. When the range fills, DNS returns SERVFAIL until a
binding expires; cached addresses are never reassigned within that window.

DNS defaults to the host nameservers listed in `/etc/resolv.conf` when the
network is created, including a local DNS stub when configured. Set an
explicit resolver with one manifest option:

```toml
[[networks]]
type = "virtle"

[networks.dns]
upstream = "10.0.0.53:53" # omitted or "host" uses host DNS
```

An explicit upstream must be an IP:port (IPv6 uses brackets), and failures
never fall back to host or public DNS. The proxy supports UDP and TCP with
bounded timeouts and TCP retry for truncated replies. The configured DNS
service may be on a private or loopback address; connection destination
checks still apply after resolution.

DNS permissions use hostname allows without their service-port restrictions.
Hostname denies without ports block queries; port-specific denies and IP
restrictions apply when connecting. `reach = "rules"` with no hostname
allows denies remote DNS, while `internet` and `all` allow it subject to
guest restrictions. Other record types follow the same hostname policy.
Custom Go egress implementations opt in through `vmnet.DNSAuthorizer`;
without it, remote DNS is refused.

The network's existing logger records DNS queries and decisions, including
guest, source, name, type, response code, upstream, and duration. UDP/TCP
port 53 traffic to destinations other than the gateway is blocked so it
cannot bypass these checks. DNS carried over other protocols remains
subject to ordinary egress rules.

DNS requests belong to the port that sent them and are canceled when that
port closes, including pending TCP connections. Fragmented TCP and UDP
packets addressed to the gateway are rejected to keep fragment reassembly
from crossing guest lifetimes. UDP replies fit the segment MTU and signal
truncation when necessary, so clients can retry large DNS messages over TCP.

```go
policy := &egress.Policy{
	Rules:  []egress.Rule{{Hosts: []string{"*.github.com"}, Ports: []int{443}}},
	Logger: logger,
}
network, err := userspace.New(userspace.Config{Egress: policy})
```

In Go, `userspace.Config.DNSUpstream` selects the same upstream as the
manifest setting. An explicitly supplied `egress.Policy.Resolver` overrides
address lookups for that policy; leave it unset to share the network's DNS.

A guest's own `vm.Spec.Egress` only narrows the network's policy: its `Allow`
list is intersected with the rules, its `Deny` list wins, and only the
`Secrets` it names are issued to it. One network can therefore serve several
sandboxes with different rules.

Address and CIDR deny entries also apply to the addresses an approved name
resolves to. A DNS name cannot bypass a denied destination address.

### Inspection, injections, and secrets

A rule with `Inspect` terminates the flow's TLS with a certificate minted
from the policy's CA (`egress.LoadOrCreateCA`) and reverse-proxies the HTTP
inside to the real destination, recording each request's method, path, and
status. The guest must trust the CA certificate (`Policy.CAPEM`,
`Policy.GuestFiles`). Only TCP is inspected: a UDP flow to a host an
inspecting rule matches (QUIC, say) is refused, so the guest falls back to
what the policy can see.

An inspected request's HTTP authority must match the flow's authorized host
and destination port. A mismatch is refused before admission hooks or secret
injection run, so a guest cannot route a credential to another virtual host
sharing the same upstream server.

Inspected requests can be decided on and rewritten as they pass. An
`Injection` is a token the guest writes and a function that computes its
replacement when a request carries it, scoped by host, method, path, and
placement (header, query, path, body, including bodies that stream). The
value is computed once per request, only when the token is present, and can
come from anywhere the host can reach at that moment; an error leaves the
token as it was, and an error wrapping `vmnet.ErrDenied` refuses the request
with 403. `Policy.Admit` decides on every inspected request before any token
is replaced, with the same refusal. Both decisions are on record.

```go
policy.Injections = []egress.Injection{
	{Token: "$VIRTLE_RANDOM$", Value: func(context.Context, egress.Request) (string, error) {
		return newNonce(), nil // a fresh value per request
	}},
	{Token: "$VIRTLE_REJECT$", Value: func(context.Context, egress.Request) (string, error) {
		return "", vmnet.ErrDenied // a request carrying it is refused
	}},
}
policy.Admit = func(ctx context.Context, r egress.Request) error {
	return decide(ctx, r.Flow.Guest, r.Method, r.URL, r.Header) // nil, or an error wrapping vmnet.ErrDenied
}
```

Secrets are named injections: a guest never holds the credential, only a
token generated per name (`Policy.GuestEnv`), and an inspected request to
one of the hosts the injection names has the token replaced on the way
out. The token is inert anywhere else, a guest's `vm.Egress.Secrets` lists
the names it may use, and the recorded path is the one the guest sent, so a
value never reaches a log. The library ships no values of its own; a
program using it brings them, as the e2e scenario in `tests/e2e` does.

A virtle network with no `[egress]` section reaches the internet and nothing
on the host or its networks. In the section, `reach` says what lies beyond
its entries: `rules` (only the allow entries, an allowlist), `internet` (the
default), or `all` (anything the host can reach). Allow entries read as an
allowlist and may or may not be one, so with any of them `reach` is required
and its absence is an error. Deny entries always apply, and allow entries
can reach the host's networks under any reach.

```toml
[[networks]]
type = "virtle"

[egress]
reach = "rules"   # only the entries below; "internet" would make them exceptions and inspection points
[[egress.allow]]
host = "api.github.com"
ports = [443]
inspect = true

[[egress.allow]]
host = "*.githubusercontent.com"

[[egress.deny]]
host = "uploads.github.com"

[[egress.secrets]]
name = "GITHUB_TOKEN"
from = "{{.Env.GITHUB_TOKEN}}"   # a template; {{fromFile "path"}} reads a file. The value never appears in the manifest.
hosts = ["api.github.com"]
in = ["header"]
```

The loader creates the CA under the state directory (`ca_dir` overrides it),
gives the guest the CA certificate at `/etc/virtle/ca.pem` and its tokens at
`/etc/virtle/secrets.env` (shell `export` lines), and lowers the same entries
to `Spec.Egress`. Add the CA to the guest's trust store
(`update-ca-certificates`, `SSL_CERT_FILE`, `NODE_EXTRA_CA_CERTS`, ...) and
source the tokens into the workload's environment.
Concurrent launches sharing the CA directory use the same certificate.
Suspend state contains the issued placeholders, while secret values and
injection permissions continue to come from the current manifest on resume.
Removing a saved token's injection or changing an explicitly configured
token makes resume fail rather than silently invalidating the guest's token.
Injections other than secrets, and `Admit`, have no manifest form yet.

## Kernel TAP

`type = "tap"` with `tap = "tap0"` (or `qemu.TAP{Name: "tap0"}`,
`firecracker.TAP{Name: "tap0"}`, `cloudhypervisor.TAP{Name: "tap0"}`) hands
an existing host TAP device to the VMM. Cloud Hypervisor opens it and brings
it up itself, which needs `CAP_NET_ADMIN` unless the device exists, is the
user's, and is already up; with that capability it also creates a missing
device. The host kernel provides the network: bridging, NAT,
addressing, and forwards are the operator's, so `Spec.Ports` and
`[[networks.forward]]` are rejected, and `Status.Networks` reports the NIC's
MAC with no address. It is how Firecracker and Cloud Hypervisor are deployed
elsewhere and the only NIC they offer today.

## Writing a network or an egress

`vmnet` holds the contracts. A `Link` moves Ethernet frames for one guest
NIC; QEMU uses `vmnet.QEMUStream` for its stream netdev. A `Network` is what
links attach to, reports their MTU, and returns a `Port` with the guest's
address and MAC. An
`Egress` is one method, `DialFlow`, that returns the connection a guest flow
is spliced to, or an error wrapping `vmnet.ErrDenied` to refuse it before it
opens; `vmnet.Passthrough` allows everything and an empty `egress.Policy`
denies outgoing traffic. A `Flow` carries the guest's name, its address,
the destination as the guest
addressed it, the name it resolved when the network knows it, and the guest's
own `vm.Egress`.

A network can also implement `vmnet.StatefulNetwork` to preserve host-side
state across QEMU suspend/resume. `SaveNetworkState` returns a
`vmnet.NetworkState` containing DNS bindings and issued tokens;
`RestoreNetworkState` restores it before the saved NIC attaches. Restore must
reject conflicts without changing state already used by attached guests.
The userspace network implements this capability. Established connections
are not saved, and secret values and permissions remain in the current policy.
