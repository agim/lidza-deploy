# Connecting a new server

**Servers → Connect server** adds a server to the control panel. A server
that has no hosting agent yet is set up by the control panel itself; nothing
is built by hand and no agent token is copied.

Before you start, give the server a DNS name (for example
`server1.example.com`) that points at it. The agent's HTTPS certificate is
requested as soon as the name resolves; if DNS is added later, the server
retries on its own within minutes.

## Install over SSH (recommended)

Enter the server ID, display name and hostname, then the SSH login: `root`, or
a user that may use `sudo`, with a password or a private key. The control
panel logs in once and runs the installation; its progress and the installer's
output are shown on the page, and the server opens when it is connected.

- The SSH credentials are used for this one run. They are kept in memory until
  it ends and are never stored, logged or shown again.
- The page shows the host key fingerprint the server presented. Enter a
  fingerprint beforehand to require it: on a mismatch nothing is run.
- **SSH address** is for a server whose hostname does not resolve to it yet.
- The control panel must be able to reach the server's SSH port.

## Run one command on the server

For a server the control panel cannot reach over SSH. The control panel shows
one command:

```sh
curl -fsSL https://deploy.example.com/enroll/<one-time token>/install.sh | sudo sh
```

Run it as root on the new server. The token is valid for one hour and for one
server; the command can be run again within that time (the installer keeps an
existing agent's credentials).

## What the installation does

Both ways run the same script on the server:

1. Download the agent release and verify its SHA-256 checksum. A control panel
   on a signed update channel serves the bundle it verified itself, so the new
   agent can be updated from the Updates page afterwards; other builds download
   the published release of the control panel's own version.
2. Run the release's installer for the hostname: Docker (when missing), Caddy,
   the agent and its services, the web ports in an active UFW or firewalld
   firewall, and the certificate retry.
3. Send the agent's connection to the control panel. The control panel saves
   the server only after it reached the agent at `https://<hostname>` with that
   token; until DNS, ports 80/443 and the certificate are ready the server
   keeps retrying for ten minutes.

The address saved is always the hostname entered in the control panel, never
one the server reports. Creating an enrollment and the server joining are both
in the audit log (`control.post` on `/api/control/server-enrollments`, then
`control.server.enrolled`).

## An agent that is already installed

Choose **The agent is already installed** and enter its URL and token (from
`/etc/lidza-agent/connection.json` on that server). Use this for a server set
up with the installer by hand.

## API

- `POST /api/control/server-enrollments` — `id`, `name`, `hostname`, optional
  `email`, and for the SSH way `ssh` (`user`, `port`, `password` or
  `private_key` with `passphrase`, optional `address` and `host_key`). Answers
  the enrollment; in command mode it carries `command`.
- `GET /api/control/server-enrollments/{id}` — `state` (`waiting`,
  `installing`, `connecting`, `connected`, `failed`), `message`, `log`,
  `host_key`. Needs the permission to manage infrastructure, as the pending
  command carries the token.
- `DELETE /api/control/server-enrollments/{id}` — cancels it.
- `GET /enroll/{token}/install.sh`, `GET /enroll/{token}/bundle/{arch}` and
  `POST /enroll/{token}/complete` are called by the new server. The reverse
  proxy in front of the control panel must pass `/enroll/` through.

Enrollments are kept in memory: after a restart of the control panel, start
again.
