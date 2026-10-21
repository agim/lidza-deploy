# Public ports and host firewall ownership

The existing `deploy/lidza.stack.json` service `public_ports` contract is unchanged:

```json
"public_ports": [
  {"port":25,"protocol":"tcp"},
  {"port":7881,"protocol":"tcp"},
  {"port":7882,"protocol":"udp"}
]
```

These validated publications belong to the app's network namespace, including
sidecars that explicitly share it. They publish IPv4 `0.0.0.0:port` to the same
container port. Only the existing supported public-port declarations are accepted;
service/private HTTP ports are not inferred as public. HTTP3000 and SFU signaling
7880 stay on loopback behind the host's HTTPS proxy. Shared-namespace mail targets
the app namespace even when the mail process lives in its sidecar.

Each host IP/port/protocol binding has one owner. The agent reserves declared
ports against other committed stacks; Docker checks actual host listeners and
concurrent allocations. Conflicts fail without replacing or stopping another app.
Stopped stacks retain their declared app ownership until changed or removed.

## Host firewall backend

The agent supports rootful Linux Docker's IPv4 iptables backend, including
iptables-nft compatibility. `DOCKER-USER` must be the first FORWARD rule. Native
Docker nftables, rootless Docker, missing iptables, insufficient privileges or
unrecognized forwarding order are reported as unsupported; activation fails
before stopping current writers. The agent does not reorder operator FORWARD
rules to claim support. Normal UFW INPUT rules do not govern Docker forwarding.
Other independent firewall base chains can still reject traffic; an installed
allow rule is not an external reachability result.

An owned `LIDZA-PUBLIC` chain is reached through a marked DOCKER-USER jump. Every
allow matches TCP/UDP, the actual container IPv4 address and target port, original
host destination port, ORIGINAL direction, and conntrack DNAT state. Directly
routed private-IP traffic and unrelated publications do not match. No INPUT
rules, SSH/management access, default policies or unrelated chains are changed.
The agent never flushes the host ruleset. Unrecognized occupants of its chain or
unowned jumps are preserved and reported for operator review.

Registration metadata is persisted atomically under `builds/.public-ports` with
private permissions and a process lock; it contains no credentials. The shared
chain is reconciled across all registered owners, retaining other apps' rules
when one app changes. Rules are installed only for exact running publications.
A mismatched bind/publication gets no allowance. Deployment/start/reload, stop,
rollback and removal reconcile rules; failed candidates are cleaned up and the
previous stack's rules restored with its actual current IP. A terminal owned
RETURN chain may remain empty after the last app is removed.

Startup backfills current/previous/pending releases from encrypted agent state;
the regular reconciliation loop repairs Docker/firewall restart changes. Older
installations do not need a redeploy solely to discover existing declarations.
Existing hosts without a pending interrupted activation keep management available
and show firewall warnings. Pending recovery retains its journal and requires the
backend problem to be resolved before safe recovery can finish. Metadata must not
be manually removed while publications are active.

## Inventory

`GET /v1/apps/{id}/ports` (private agent token) and
`GET /api/control/apps/{id}/ports` (fleet.read) return `{ports, warning?}`. Each row
contains app_id, service, container, release, declared, protocol, host_ip,
host_port, container_port, publication, firewall, external and actual bindings.
Only the requested app's current release is returned. Container inventory also
includes these port rows. No environment, tokens or arbitrary Docker labels are
exposed. Settings capability `public_ports_supported` lets older agents retain
their existing UI without false support claims.

Publication states distinguish published, stopped, missing, mismatch, private
proxy and undeclared bindings. Firewall states distinguish an installed owned
allow rule from missing/unsupported/not-managed and loopback-not-required states.
App Settings and grouped server containers share the same display component.

Cloud firewall rules, NAT, DNS and provider SMTP restrictions are separate. The
agent has no authorized cloud-provider integration here and does not open cloud
rules. Public external reachability remains `unverified` until independently
tested; neither a Docker binding nor a host allow rule proves internet access.

## Isolated host acceptance

On a disposable Linux host with Go, Docker, Python3, iproute2, iptables and curl,
run `TEST_ISOLATED_HOST=1 tests/public-ports-host.sh` as root. Never run it on a
production host: it creates a test namespace and a narrowly scoped client DROP.
The test performs real deployments with managed PostgreSQL and profile limits,
checks TCP25 and UDP7882 from a separate namespace through Docker DNAT, checks
another published service remains blocked, verifies publication/firewall
inventory and tests redeploy, restart repair, rollback, candidate recovery and
removal. INPUT rules and the operator DROP are checked for preservation.
