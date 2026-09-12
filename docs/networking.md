# Networking

Start with a port forward, then add destination rules, DNS, and credential
injection as needed. Each TOML example replaces the networking and egress
sections of an existing boot manifest; the guest needs virtio-net and a DHCP
client for the `user` and `virtle` examples.

| Network type | Use it for | Backends |
| --- | --- | --- |
| `user` | QEMU NAT and local port forwards; the QEMU default | QEMU |
| `virtle` | An unprivileged userspace network with egress policy and HTTP inspection | QEMU |
| `tap` | Host-managed bridging, routing, and NAT | QEMU, Firecracker, Cloud Hypervisor |

QEMU can mix NIC types, with at most one `virtle` NIC per machine. Firecracker
and Cloud Hypervisor start without a NIC unless a TAP network is configured.
For a complete working guest, see the [networking recipe](recipes/networking/README.md).

## Forward a guest port

Use QEMU's built-in NAT to give the guest outbound connectivity and expose a
service on the host. This maps host `127.0.0.1:2222` to the guest's SSH port;
QEMU handles the traffic, so virtle egress rules do not apply.

```toml
[[networks]]
type = "user"
forward = [{ host = "127.0.0.1:2222", guest = ":22" }]
```

## Move traffic through virtle

Switch to `virtle` when you need to control outgoing connections: its gVisor
stack provides an IPv4 segment, fixed DHCP leases, gateway DNS, and host-dialed
TCP/UDP flows without changing host networking. With no `[egress]` section,
external traffic may reach the public internet; gateway services and peers on
the same network remain reachable independently of egress policy.

```toml
[[networks]]
type = "virtle"
forward = [{ host = "127.0.0.1:2222", guest = ":22" }]
```

The guest can ping the gateway, but external ICMP is not forwarded.

## Restrict outgoing connections

Set `reach = "rules"` to make the allow entries the complete set of external
destinations; `internet` additionally allows public destinations, and `all`
allows host-reachable destinations, subject to denies. An explicit `reach` is
required with allow entries, which accept hostname patterns, IPs, or CIDRs and
optional ports; deny entries win, including against addresses resolved from an
allowed name.

```toml
[[networks]]
type = "virtle"

[egress]
reach = "rules"

[[egress.allow]]
host = "*.github.com" # subdomains, including api.github.com; not github.com itself
ports = [443]

[[egress.deny]]
host = "uploads.github.com"
```

Explicit allows can reach LAN destinations, but the policy's
[default address denies](../vmnet/egress/egress.go) still block loopback,
link-local, multicast, and other special ranges under every `reach` setting.

## Resolve private names

Use a specific DNS server when allowed destinations live in a private zone.
Manifest-managed networks authorize DNS queries before forwarding them and
return synthetic A addresses to preserve the hostname for connection policy;
the host then resolves approved connections through the same upstream.

```toml
[[networks]]
type = "virtle"

[networks.dns]
upstream = "10.0.0.53:53" # IP:port; IPv6 addresses use brackets

[egress]
reach = "rules"

[[egress.allow]]
host = "packages.corp.example"
ports = [443] # DNS permission follows the name, independently of service ports
```

Omit `upstream` or use `"host"` to read nameservers from `/etc/resolv.conf`;
this is wire DNS, without `/etc/hosts`, NSS, or search suffixes. An explicit
upstream never falls back, and guest TCP/UDP port 53 traffic to destinations
other than the gateway is blocked.

## Use a credential without putting it in the guest

Inspection terminates TLS with a local CA and proxies HTTP to the authorized
host and port; matching UDP traffic, including QUIC, is refused. The guest
receives a placeholder token whose value is substituted on the host only
within the secret's host, method, path, and placement constraints.

```toml
[[networks]]
type = "virtle"

[egress]
reach = "rules"
# ca_dir defaults to <state_dir>/egress-ca

[[egress.allow]]
host = "api.github.com"
ports = [443]
inspect = true

[[egress.secrets]]
name = "GITHUB_TOKEN"
from = "{{.Env.GITHUB_TOKEN}}" # set this in the host environment before launch
hosts = ["api.github.com"]
methods = ["GET"]
paths = ["/user"]
in = ["header"]
```

The loader delivers the CA certificate and token environment file through the
guest agent; the workload must trust that CA and source the tokens. In the guest:

```sh
. /etc/virtle/secrets.env
curl --cacert /etc/virtle/ca.pem \
  -H "Authorization: Bearer $GITHUB_TOKEN" \
  https://api.github.com/user
```

Suspend/resume preserves the NIC's MAC and IP, synthetic DNS bindings, and
issued tokens; established connections are lost, and current policy supplies
the secret values and permissions. See the [network state contract](../vmnet/state.go).

## Compose a network in Go

Supply a `userspace.Network` to a QEMU backend and keep it open until its
machines exit; reuse it across backends to put guests on the same segment.
Unlike manifests, `userspace.Config{}` defaults to passthrough egress and real
DNS answers, so select `DNSFakeIP` for hostname rules and an explicit policy
for restrictions (`egress.Policy{}` denies all external flows).

```go
// Inside a function with ctx context.Context and kernel vm.Kernel, returning error.
policy := &egress.Policy{Reach: egress.ReachInternet}
network, err := userspace.New(userspace.Config{
	DNS:    userspace.DNSFakeIP,
	Egress: policy,
})
if err != nil {
	return err
}
defer network.Close()

b := &qemu.Backend{HostName: "worker", Network: network}
m, err := b.Start(ctx, &vm.Spec{Kernel: kernel})
if err != nil {
	return err
}
defer m.Kill()
return m.Wait(ctx)
```

While the guest is running, `network.DialContext(ctx, "tcp", "worker:22")`
reaches its SSH service directly, and `network.Listen("tcp", ":8080")` exposes
a host service at `network.Gateway()`. A guest's `vm.Spec.Egress` can only narrow
the shared policy; when using `manifest.Load` instead, close the returned
backend (`io.Closer`), which owns its network.

## Decide inspected requests in Go

Add a per-request decision when destination rules are too coarse:
`Policy.Admit` runs before token replacement, and `vmnet.ErrDenied` produces
HTTP 403. Use this policy in the Go example above to allow only GET and HEAD
requests to `api.github.com`:

```go
ca, err := egress.LoadOrCreateCA("./egress-ca")
if err != nil {
	return err
}
policy := &egress.Policy{
	CA: ca,
	Rules: []egress.Rule{{
		Hosts: []string{"api.github.com"}, Ports: []int{443}, Inspect: true,
	}},
	Admit: func(_ context.Context, r egress.Request) error {
		switch r.Method {
		case "GET", "HEAD":
			return nil
		default:
			return vmnet.ErrDenied
		}
	},
}
```

Before `Start`, append `policy.GuestFiles()` to `vm.Spec.Files` and set
`qemu.Backend.RemoteControl` to `qemu.QGA{}` to deliver the CA certificate; the
guest must run the agent and trust the certificate. See
[`egress.Policy`](../vmnet/egress/egress.go) and
[`egress.Injection`](../vmnet/egress/inject.go) for request hooks and dynamic values,
or [`vmnet`](../vmnet/vmnet.go) to implement a network or egress.

## Connect to a host TAP network

Use TAP when the guest should join a network managed by the host, or when
running Firecracker or Cloud Hypervisor. Prepare an existing TAP device owned
by the VMM user and bring it up; the operator supplies guest addressing,
bridging or routing, NAT, and any port forwards.

```toml
backend = "cloud-hypervisor"

[[networks]]
type = "tap"
tap = "tap0"
```

TAP networks reject manifest `forward` entries and Go `vm.Spec.Ports`; use
host networking tools for those mappings. See the
[Cloud Hypervisor setup](cloud-hypervisor.md#networking) and
[Firecracker guide](firecracker.md) for backend requirements.
