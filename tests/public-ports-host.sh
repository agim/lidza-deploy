#!/usr/bin/env bash
# Run only on a disposable, isolated Linux Docker host (never production).
set -euo pipefail
[[ ${TEST_ISOLATED_HOST:-} == 1 && $(id -u) == 0 ]] || { echo 'Requires TEST_ISOLATED_HOST=1 on a disposable rootful Linux Docker host.' >&2; exit 1; }
cd "$(dirname "$0")/.."
if iptables -S LIDZA-PUBLIC >/dev/null 2>&1; then
 echo 'Refusing to use a host with an existing Lidza public-port chain.' >&2; exit 1
fi
before=$(mktemp)
iptables -S INPUT >"$before"
ip netns add ports-client
cleanup() {
 iptables -D DOCKER-USER -i ports-host -m comment --comment fixture-deny-client -j DROP 2>/dev/null || true
 ip link del ports-host 2>/dev/null || true
 ip netns del ports-client 2>/dev/null || true
 if ! cmp -s "$before" <(iptables -S INPUT); then echo 'Host INPUT policy changed during acceptance.' >&2; exit 1; fi
 rm -f "$before"
}
trap cleanup EXIT
ip link add ports-host type veth peer name ports-peer
ip link set ports-peer netns ports-client
ip addr add 198.18.0.1/24 dev ports-host
ip link set ports-host up
ip netns exec ports-client ip addr add 198.18.0.2/24 dev ports-peer
ip netns exec ports-client ip link set ports-peer up
ip netns exec ports-client ip link set lo up
iptables -I DOCKER-USER 1 -i ports-host -m comment --comment fixture-deny-client -j DROP
TEST_DOCKER=1 TEST_PUBLIC_PORTS=1 go test ./internal/agent -run '^TestDockerStackProfilesLifecycle$' -v -count=1
