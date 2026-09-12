# Networking playground (QEMU)

This recipe boots a NixOS guest with virtle's managed network, a DNS allowlist,
HTTPS inspection, and a named secret. A local DNS resolver and mock HTTPS API
make the checks self-contained after the Nix build. No API account or real
credential is needed.

The guest has one vCPU, 768 MiB RAM, DHCP, `dig`, `curl`, QEMU Guest Agent,
and SSH over vsock. The host needs Linux x86_64, working KVM and vhost-vsock,
and an interface IPv4 address outside loopback and link-local ranges. The mock API binds
that address on an ephemeral port; the resolver binds only to host loopback.
Neither service changes the host's network configuration.

## Run it

From this directory:

```sh
nix run          # enter the guest over SSH
nix run .#check  # run all checks, shut down, and report success/failure
```

When testing changes from a local virtle checkout, run from the repository root
so the recipe uses your code:

```sh
nix run ./docs/recipes/networking#check \
  --override-input virtle path:. --no-write-lock-file
```

The launcher prints its workspace and `network.log` path. In another host
terminal, `tail -f` that log to watch DNS names, allow/deny decisions, upstream
resolution, and inspected HTTP requests. Use `--keep` to retain the generated
manifest, overlay, and logs after exit:

```sh
nix run .#check -- --keep
# On a host with several interfaces, select the address for the mock API:
nix run -- --bind-address 192.168.1.10
```

The workspace is otherwise disposable; failed runs retain it for diagnosis.
Exit the guest shell to stop the VM and its host helpers.

## Explore in the guest

Run `demo-check` for the full sequence, or try the steps below. Public demo
settings are loaded into interactive shells from `/etc/virtle/demo.env`.

```sh
dig @192.168.127.1 api.virtle.test A
dig @192.168.127.1 +tcp api.virtle.test A
dig @192.168.127.1 -x "$(dig @192.168.127.1 +short api.virtle.test A)"
dig @192.168.127.1 api.virtle.test TXT
dig @192.168.127.1 blocked.virtle.test A
dig @192.168.127.1 unlisted.test A
```

Allowed A queries return a synthetic address in `198.18.0.0/15`, while PTR
returns the original name. TXT returns `"virtle networking demo"` from the
upstream resolver. Explicitly denied and unlisted names return `REFUSED`.
The guest connects through the synthetic address, preserving the hostname
for egress decisions and secret injection.
These commands target virtle's default gateway directly; ordinary applications
such as curl use the resolver supplied by DHCP through systemd-resolved.

Try the mock API without a credential, then with the guest placeholder:

```sh
api="https://api.virtle.test:$DEMO_API_PORT/auth"
curl --cacert /etc/virtle/ca.pem -sS -o /dev/null -w '%{http_code}\n' "$api"
# 401

source /etc/virtle/secrets.env
curl --cacert /etc/virtle/ca.pem -sS -o /dev/null -w '%{http_code}\n' \
  -H "Authorization: Bearer $DEMO_TOKEN" "$api"
# 204

curl --cacert /etc/virtle/ca.pem -sS -o /dev/null -w '%{http_code}\n' \
  -H "Authorization: Bearer $DEMO_TOKEN" \
  "https://other.virtle.test:$DEMO_API_PORT/auth"
# 401: this hostname is allowed, but cannot receive the secret
```

The launcher generates a dummy credential in host memory and supplies it to
virtle's host environment. QGA provisions a different placeholder into the
guest. Virtle replaces that placeholder only in inspected requests to
`api.virtle.test`; the API returns a status with no credential in its response.
The guest trusts virtle's inspection CA via `--cacert`. Separately, the
launcher gives the host virtle process the mock API's certificate through
`SSL_CERT_FILE`, so both TLS connections are verified.

`demo-check` also expects direct UDP/TCP DNS requests to `9.9.9.9:53` to fail.
An offline host produces the same result, so these requests illustrate the
expected behavior rather than independently proving the DNS gate works.
It checks that dialing the API's reachable real IP cannot bypass the hostname
allowlist. The host check requires specific DNS decisions and HTTP statuses,
confirms that only the intended hostname received the secret injection,
and checks that the dummy credential does not appear in captured output.

## Change the policy or DNS upstream

[manifest.toml](manifest.toml) contains the networking policy. The launcher
fills in the mock services' ports and appends it to the generated QEMU boot
manifest. It allows `*.virtle.test` on the API port with inspection, explicitly
denies `blocked.virtle.test`, and scopes `DEMO_TOKEN` to `api.virtle.test`.
Unmatched destinations are denied by `reach = "rules"`.

The recipe selects a local dnsmasq resolver through `[networks.dns].upstream`.
It adds the three demo names and the TXT record, and forwards other queries
to the host's `/etc/resolv.conf` nameservers. `example.com:443` is also allowed
for optional internet exploration:

```sh
dig @192.168.127.1 example.com
curl https://example.com
```

Virtle itself defaults to the host's DNS when `[networks.dns]` is omitted or
`upstream = "host"`. To try another resolver, edit the template:

```toml
[networks.dns]
upstream = "10.0.0.53:53"
```

The selected resolver handles both guest DNS and virtle's subsequent upstream
address lookups. Synthetic guest addresses are preserved. The private
`*.virtle.test` names require the demo resolver or equivalent records in your
chosen resolver; public DNS will not resolve them. These experiments change
the expected check results.

See [Networking](../../networking.md) for the policy and DNS contracts.
