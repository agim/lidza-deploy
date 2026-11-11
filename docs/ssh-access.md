# SSH access to hosting servers

In **Servers → Manage server → SSH access**, add your MacBook's SSH **public** key with a recognizable label. On your Mac, `cat ~/.ssh/id_ed25519.pub` prints the public key. If you don't have one, create it with `ssh-keygen -t ed25519 -C "MacBook"`. Never paste or upload the private key.

Wait for **Managed SSH access applied**, then use the connection command shown in the panel, for example:

```sh
ssh deploy@your-server.example.com
```

This creates a dedicated `deploy` account when needed. The agent continues running as `lidza-agent`. Membership in `adm` and `systemd-journal` lets deploy inspect service and kernel logs:

```sh
journalctl -u lidza-agent --since '1 hour ago'
journalctl -k --since '1 hour ago' | grep -Ei 'oom|out of memory|killed process'
free -h
```

## Reading service logs of adopted applications

On a server whose applications were adopted from an existing setup, the
shared deployment account (the one named under **Service control** on the
server's **Agent** tab, usually `deploy`) is how you sign in over SSH. Its
applications now run as system units of their own `app-<name>` accounts, not
as user units of the shared account, so `journalctl --user -u <unit>` finds
nothing and `journalctl -u <unit>` prints `-- No entries --` until the account
may read the system journal.

Turn on **System log access for the deployment account** on the server's
**Agent** tab. The agent adds the account to the `systemd-journal` group at
once, records a `log-access` host operation, and checks the membership at
every start and once a minute, so a membership changed by hand is put back to
the setting. Turning it off removes only the `systemd-journal` membership;
other groups stay. Until the setting is changed in the panel, the agent leaves
the membership as it is. The card shows whether the account is a member now.

Group changes apply to new SSH sessions only, so sign in again, then read
system units without `--user`:

```sh
ssh deploy@your-server.example.com
journalctl -u <unit> --since '1 hour ago'
journalctl -u <unit> -f
systemctl list-units --type=service   # unit names
```

Applications' logs may contain secrets: everyone who can sign in as the
account reads every service's logs. Only infrastructure managers can change
the setting.

**Allow full sudo administration without a password** is off by default. Enabling it grants root-level control to everyone who can log in as deploy, including via unmanaged keys. Use it when server administration is needed; disable it to remove the grant managed by this app. Privileges assigned outside this app remain effective. Deploy is never added to the Docker or sudo groups automatically.

Only control-panel administrators can manage server SSH access. The GUI lists managed labels and fingerprints and supports revocation. Existing root access, password policy, and unmanaged authorized keys remain intact. Removing a managed key prevents future logins through that key; established sessions remain open. Add and test deploy access before separately restricting root login.

The installer provides OpenSSH server and sudo. Existing installations should update the agent and managed CLI helper through **Updates**; if OpenSSH or sudo is missing, install those packages on the host first. This feature supports the installer's Ubuntu/Debian layout, default SSH port 22, and root service helper. It does not open cloud firewalls: allow TCP 22 from your client in your provider's firewall. The agent uses fixed root-owned SSH key, drop-in and sudoers files; it validates SSH configuration before reloading and restores its previous files if application fails. Custom sshd policies that override the managed key source are rejected rather than replaced.

For a colocated agent whose API uses a loopback address, the panel uses the control-panel hostname in its SSH command. For a remote agent, it uses the agent hostname. Both must resolve to the hosting server; a localhost control panel on your workstation needs the hosting server's actual address substituted.
