# First-hit scanner blocking

The application Security activity page records observed probes. A `404` means
that request did not find its target; it does not prove a secret was exposed.
Those historical records do not contain a verified client address and must not
be used to create retrospective IP bans.

The hosting agent bundles an optional host firewall setup helper. It uses stock
Caddy, Fail2ban and nftables, without a custom Caddy build or application changes.
It is not enabled simply by installing an update: the source of public traffic
must be established before a network ban can safely identify a scanner.

For **public traffic arriving directly at Caddy**:

- Caddy rejects the first PHP, `.env`, `.git` directory, or backup/dump-file probe
  with HTTP 403 before passing it to an application. Uppercase and common encoded
  path variants are covered; query-string text alone does not trigger a ban.
- A dedicated, rotating scanner log includes Caddy's socket peer address. Request
  headers, query strings and response headers are removed. Normal request logging
  is preserved if it was configured, and is not newly enabled otherwise.
- Fail2ban bans on **one attempt**, for **7,776,000 seconds (90 days)**, on TCP and
  UDP ports 80 and 443 (including HTTP/3). The first request is rejected immediately;
  the network ban follows when Fail2ban consumes its log entry.
- IPv4 and IPv6 bans persist in Fail2ban's database and are restored after a
  service restart or reboot. SSH and unrelated firewall rules are not changed.
- Loopback, private/link-local addresses, the host's own addresses and explicitly
  allowlisted operator/monitoring addresses are exempt from automatic bans.

**Behind Cloudflare, a CDN or another load balancer, block at that edge instead.**
The origin firewall sees the proxy's socket address, not necessarily the visitor.
The helper requires `--direct-public-traffic`, and rejects Caddy configurations
that already declare trusted proxies. Forwarding headers supplied by a visitor
are never accepted as evidence for an nftables ban.

## Review and enable

Python 3 is required and is included in new installer dependency checks. After
updating the agent, prepare reviewable files without changing host services:

```sh
sudo /usr/local/bin/lidza-agent --scanner-firewall prepare \
  --scanner-firewall-output /root/lidza-scanner-review
```

Review `caddy.json` and the `fail2ban/` files. Existing Caddy TLS, OAuth routing and
application routes are retained. Activation uses Caddy's live configuration API
and autosave, so it also works on installations started with `caddy --resume`.

After confirming direct public traffic, run over SSH so package installation can
finish independently of a browser request. Replace `YOUR_PUBLIC_IP` with the
operator address that should remain exempt; multiple IPs/CIDRs are comma-separated:

```sh
sudo /usr/local/bin/lidza-agent --scanner-firewall enable \
  --direct-public-traffic --scanner-firewall-allow YOUR_PUBLIC_IP
```

The helper installs missing Fail2ban/nftables packages through the OS package
manager, validates both configurations, enables the Fail2ban service and reports
the jail status. It refuses to overwrite operator-owned files using its reserved
configuration names. Setup failures restore the preceding Caddy configuration and
managed configuration files rather than flushing the host firewall.

Inspect, unban or disable:

```sh
sudo fail2ban-client status lidza-scanners
sudo fail2ban-client get lidza-scanners banip --with-time
sudo fail2ban-client set lidza-scanners unbanip IP_ADDRESS
sudo /usr/local/bin/lidza-agent --scanner-firewall disable
```

An attacker can rotate addresses, and innocent visitors can share a public IP;
keep operator exemptions and the unban path available. These rules target scanner
paths on a host that does not serve PHP or public database/config backups; they
are not a general web application firewall.
