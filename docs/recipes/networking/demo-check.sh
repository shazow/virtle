set -euo pipefail

# QGA installs the public demo settings, inspection CA, and secret placeholder
# before SSH starts. DHCP is independent of SSH over vsock, so wait for it.
systemctl start systemd-networkd-wait-online.service
# shellcheck disable=SC1091
source /etc/virtle/demo.env
# shellcheck disable=SC1091
source /etc/virtle/secrets.env

fail() { echo "FAIL: $*" >&2; exit 1; }
pass() { echo "PASS: $*"; }
# Query virtle's fixed default gateway directly, so TCP and refusal checks
# exercise the gateway instead of systemd-resolved's local stub or cache.
dns() { dig @192.168.127.1 +time=2 +tries=1 "$@"; }

address=$(dns +short api.virtle.test A)
[[ $address == 198.18.* || $address == 198.19.* ]] || fail "expected a synthetic address, got $address"
[[ $(dns +tcp +short api.virtle.test A) == "$address" ]] || fail "TCP DNS binding differs"
[[ $(dns +short -x "$address") == api.virtle.test. ]] || fail "synthetic PTR differs"
pass "UDP/TCP DNS and local PTR: api.virtle.test -> $address"

[[ $(dns +short api.virtle.test TXT) == '"virtle networking demo"' ]] || fail "upstream TXT not proxied"
pass "TXT record proxied from the configured resolver"
dns blocked.virtle.test A | grep -q 'status: REFUSED' || fail "explicit DNS deny"
dns unlisted.test A | grep -q 'status: REFUSED' || fail "DNS allowlist"
pass "explicitly denied and unlisted DNS names refused"

# Illustrate that alternate DNS cannot be reached from this guest. An offline
# host produces the same failure, so this is not an independent gate test.
if dig @9.9.9.9 +time=2 +tries=1 api.virtle.test A >/dev/null 2>&1; then fail "alternate UDP DNS unexpectedly reachable"; fi
if dig @9.9.9.9 +tcp +time=2 +tries=1 api.virtle.test A >/dev/null 2>&1; then fail "alternate TCP DNS unexpectedly reachable"; fi
pass "alternate UDP/TCP DNS requests failed as expected"

request() {
  curl --noproxy '*' --silent --show-error --connect-timeout 3 --max-time 10 \
    --cacert /etc/virtle/ca.pem --output /dev/null --write-out '%{http_code}' "$@"
}
api="https://api.virtle.test:$DEMO_API_PORT/auth"
other="https://other.virtle.test:$DEMO_API_PORT/auth"
[[ $(request "$api") == 401 ]] || fail "API accepted a missing credential"
[[ $(request --header "Authorization: Bearer $DEMO_TOKEN" "$api") == 204 ]] || fail "secret was not injected"
pass "HTTPS API: missing credential -> 401; guest placeholder -> 204"
[[ $(request --header "Authorization: Bearer $DEMO_TOKEN" "$other") == 401 ]] || fail "secret escaped its hostname scope"
pass "same placeholder at another allowed hostname -> 401"

# Resolve the allowed name to its real host address, bypassing the synthetic
# binding. The hostname allow must not permit this numeric destination.
if request --resolve "api.virtle.test:$DEMO_API_PORT:$DEMO_HOST_ADDRESS" "$api" >/dev/null 2>&1; then
  fail "direct IP bypass succeeded"
fi
pass "direct IP connection refused by the hostname allowlist"
echo 'PASS: networking demo complete'
