# Additional application hostnames

Open **Applications → Settings → General → Additional hostnames** to add or
remove up to 20 domains alongside the primary application hostname. Save changes
to apply the routing assignments. Additional domains serve the same running
application; the agent preserves the original Host header and does not redirect
them to the primary hostname.

Point each hostname's A/AAAA records (or a CNAME) to the application's hosting
server. Every resolved address must reach that server on port 80 before HTTPS
is issued. Ports 80 and 443 must remain reachable. The existing Caddy integration
issues and renews certificates; each hostname has its own bounded DNS and
certificate checks. A pending domain does not block the others. The General tab
refreshes domain status without replacing unsaved form values.

The primary hostname remains the canonical APP_URL used by deployments and
workers. Applications that enforce their own host allowlists or redirects may
also need those settings updated. Names must be lowercase DNS hostnames without
schemes, paths, ports, wildcards, or IP addresses. Duplicate names are rejected,
including names already assigned to another app on another managed server.

Adding or removing only additional hostnames does not reload the container or
run a build. Changing the primary hostname or other runtime settings follows
the normal reload process. Maintenance, stopped-app handling, removal, and
certificate authorization cover every explicit hostname. Removing a hostname
revokes its route and certificate authorization. Update both the control panel
and hosting agent before managing additional hostnames; older agents are shown
an upgrade notice and legacy settings clients preserve existing assignments.
