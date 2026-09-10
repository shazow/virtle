# Networking

virtle gives a guest one NIC. What is behind it is a choice made per backend,
in Go or in the manifest's `[[networks]]` entry:

| `type` | Go | Frames go to | Backends |
| --- | --- | --- | --- |
| `user` (default) | `qemu.User{}` | QEMU's built-in user networking (slirp): NAT and `hostfwd` port forwards inside QEMU | QEMU |
| `virtle` | a `vmnet.Network` on `qemu.Backend.Network` | a network virtle runs in userspace: fixed addresses, DHCP and DNS, host-side dialing, forwards, and an egress policy | QEMU (Firecracker follows with the guest daemon) |
| `tap` | `qemu.TAP{Name}`, `firecracker.TAP{Name}` | a host TAP device the host kernel networks; the operator owns addressing, NAT, and forwards | QEMU, Firecracker |

`user` stays the default until the virtle network reaches parity with it.

## The virtle network

`vmnet/userspace` is an in-process network on gVisor's netstack. Every
attached machine is a port on one Ethernet segment with a fixed IPv4 address
and MAC (a static DHCP lease keyed by the MAC), the gateway serves DHCP and
DNS, guests on the same network reach each other, and every guest-initiated
TCP connection or UDP exchange is terminated in userspace and dialed on the
host through the network's **egress**. Nothing touches the host's network
configuration and no privilege is needed.

```go
network, err := userspace.New(userspace.Config{}) // 192.168.127.0/24, DNS forwarded to the host resolver
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
- `network.DialContext` dials a guest by address or by machine name;
  `network.Listen` serves a host service on the gateway address, which guests
  reach without leaving the network.
- A suspended machine keeps its address and MAC and re-attaches with them on
  resume, so the lease its kernel holds stays valid.
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

Name rules need names. The network's fake-IP DNS mode
(`userspace.DNSFakeIP`) answers every query with a synthetic address it
remembers, the flow to that address carries the name the guest resolved, and
the egress resolves it when it dials. The manifest loader selects that mode
whenever an `[egress]` section exists.

```go
policy := &egress.Policy{
	Rules:  []egress.Rule{{Hosts: []string{"*.github.com"}, Ports: []int{443}}},
	Logger: logger,
}
network, err := userspace.New(userspace.Config{DNS: userspace.DNSFakeIP, Egress: policy})
```

A guest's own `vm.Spec.Egress` only narrows the network's policy: its `Allow`
list is intersected with the rules, its `Deny` list wins, and only the
`Secrets` it names are issued to it. One network can therefore serve several
sandboxes with different rules.

### Inspection, injections, and secrets

A rule with `Inspect` terminates the flow's TLS with a certificate minted
from the policy's CA (`egress.LoadOrCreateCA`) and reverse-proxies the HTTP
inside to the real destination, recording each request's method, path, and
status. The guest must trust the CA certificate (`Policy.CAPEM`,
`Policy.GuestFiles`).

Secrets let a guest use a credential it never holds. A `Secret` pairs a name
with a function that reads the value when a request needs it and the hosts
that may receive it; the guest gets a generated token (`Policy.GuestEnv`),
and an inspected request to one of those hosts has the token replaced in its
headers, query, path, or body on the way out. The token is inert anywhere
else, and the recorded path is the one the guest sent, so a value never
reaches a log.

A secret is one case of an `Injection`: a token the guest writes and a
function that computes its replacement as the request passes, with the same
host, method, path, and placement scoping. The value is computed only when a
request carries the token, once per request, and can come from anywhere the
host can reach at that moment; an error leaves the token as it was.

```go
policy.Injections = []egress.Injection{
	{Token: "$VIRTLE_RANDOM$", Value: egress.Random(16)}, // a nonce per request
	{Token: "$BUILD_ID$", Value: func(ctx context.Context, r egress.Request) (string, error) {
		return lookupBuild(ctx, r.Flow.Guest) // the guest's name, method, URL, and headers are in r
	}},
}
```

Injections have no manifest form yet; secrets do.

```toml
[[networks]]
type = "virtle"

[egress]
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
from = "env:GITHUB_TOKEN"   # or file:/path; the value never appears in the manifest
hosts = ["api.github.com"]
in = ["header"]
```

The loader creates the CA under the state directory (`ca_dir` overrides it),
gives the guest the CA certificate at `/etc/virtle/ca.pem` and its tokens at
`/etc/virtle/secrets.env` (shell `export` lines), and lowers the same entries
to `Spec.Egress`. Add the CA to the guest's trust store
(`update-ca-certificates`, `SSL_CERT_FILE`, `NODE_EXTRA_CA_CERTS`, ...) and
source the tokens into the workload's environment.

## Kernel TAP

`type = "tap"` with `tap = "tap0"` (or `qemu.TAP{Name: "tap0"}`,
`firecracker.TAP{Name: "tap0"}`) hands an existing host TAP device to the
VMM. The host kernel provides the network: bridging, NAT, addressing, and
forwards are the operator's, so `Spec.Ports` and `[[networks.forward]]` are
rejected, and `Status.Networks` reports the NIC's MAC with no address. It is
how Firecracker is deployed elsewhere and the only NIC it offers today.

## Writing a network or an egress

`vmnet` holds the contracts. A `Link` moves Ethernet frames for one guest
NIC; backends build them from what their VMM offers (`vmnet.QEMUStream` for
QEMU's stream netdev, `vmnet.Tunnel` for a guest agent's frame tunnel,
`vmnet.NewDeferred` for a peer that arrives after boot). A `Network` is what
links attach to and returns a `Port` with the guest's address and MAC. An
`Egress` is one method, `DialFlow`, that returns the connection a guest flow
is spliced to, or an error wrapping `vmnet.ErrDenied` to refuse it before it
opens; `vmnet.Passthrough` allows everything and `vmnet.DenyAll` nothing. A
`Flow` carries the guest's name, its address, the destination as the guest
addressed it, the name it resolved when the network knows it, and the guest's
own `vm.Egress`.
