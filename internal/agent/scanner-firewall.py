#!/usr/bin/env python3
"""Managed Caddy probe rejection and Fail2ban/nftables web-port bans.

No application log or forwarding header supplies the ban address. Fail2ban reads
Caddy's actual socket peer. Enabling therefore requires an explicit assertion
that public traffic reaches Caddy directly (no CDN/load-balancer in front).
"""
import argparse
import copy
import ipaddress
import json
import os
import pathlib
import pwd
import re
import shutil
import subprocess
import sys
import urllib.request

API = "http://127.0.0.1:2019"
LOGGER = "lidza_scanners"
LOGFILE = "/var/log/caddy/lidza-scanners.json"
GUARD = "lidza-scanner-guard"
SKIP = "lidza-scanner-skip-default-logging"


def managed(route, prefix):
    name = route.get("@id", "")
    return name == prefix or name.startswith(prefix + ":")


def seconds_value(value):
    match = re.search(r"(\d+)\s*(?:seconds)?\s*$", value)
    if not match:
        raise ValueError("Could not verify Fail2ban retention: " + value.strip())
    return int(match.group(1))
# Caddy matches the decoded URL path, not the query. Also reject common double
# encodings, while leaving /.well-known/ challenges and ordinary app routes alone.
DOT = r"(?:\.|%2[eE]|%252[eE])"
SLASH = r"(?:/|%2[fF]|%252[fF])"
PATTERN = (r"(?i)(?:" + DOT + r"(?:php[0-9]*|phtml|phar)(?:" + SLASH + r"|$)"
           r"|(?:^|" + SLASH + r")" + DOT + r"(?:env(?:[.~-]|" + SLASH + r"|$)|git(?:" + SLASH + r"|$)|backup(?:[.~-]|" + SLASH + r"|$))"
           r"|" + DOT + r"(?:bak|backup|old|orig|sql|dump)(?:" + SLASH + r"|$))")


def protect(config):
    result = copy.deepcopy(config)
    servers = result.get("apps", {}).get("http", {}).get("servers", {})
    if not servers:
        raise ValueError("Caddy has no HTTP servers to protect")
    guard = {
        "@id": GUARD,
        "match": [{"path_regexp": {"name": "lidza_scanner", "pattern": PATTERN}}],
        "handle": [
            {"handler": "vars", "access_logger_names": [LOGGER]},
            {"handler": "log_append", "key": "lidza_scanner", "value": "forbidden-path"},
            {"handler": "static_response", "status_code": 403, "body": "Forbidden\n"},
        ],
        "terminal": True,
    }
    already_protected = any(any(managed(route, GUARD) for route in server.get("routes", [])) for server in servers.values())
    logs = result.setdefault("logging", {}).setdefault("logs", {})
    if LOGGER in logs and not already_protected:
        raise ValueError("Caddy logger name lidza_scanners is already in use; refusing to overwrite it")
    logs[LOGGER] = {
        "include": ["http.log.access." + LOGGER],
        "writer": {"output": "file", "filename": LOGFILE, "mode": "0640", "roll_size_mb": 10, "roll_keep": 5, "roll_keep_days": 7},
        "encoder": {"format": "filter", "wrap": {"format": "json"}, "fields": {
            "request>headers": {"filter": "delete"},
            "request>uri": {"filter": "replace", "value": "redacted-probe"},
            "request>tls": {"filter": "delete"},
            "resp_headers": {"filter": "delete"},
        }},
    }
    default = logs.setdefault("default", {})
    default.setdefault("exclude", [])
    if "http.log.access." + LOGGER not in default["exclude"]:
        default["exclude"].append("http.log.access." + LOGGER)
    for server_name, server in servers.items():
        access = server.get("logs")
        if access and (access.get("skip_unmapped_hosts") or access.get("skip_hosts")):
            raise ValueError("Existing Caddy access-log exclusions require manual review before automatic bans")
        routes = [route for route in server.get("routes", []) if not managed(route, GUARD) and not managed(route, SKIP)]
        # Do not start logging normal traffic on servers that did not log it.
        needs_skip = access is None or any(managed(route, SKIP) for route in server.get("routes", []))
        server.setdefault("logs", {})
        entry = copy.deepcopy(guard)
        entry["@id"] = GUARD + ":" + server_name
        prefix = [entry]
        if needs_skip:
            prefix.append({"@id": SKIP + ":" + server_name, "handle": [{"handler": "vars", "log_skip": True}]})
        server["routes"] = prefix + routes
    return result


def configurations(allow):
    trusted = ["127.0.0.0/8", "::1", "10.0.0.0/8", "172.16.0.0/12", "192.168.0.0/16", "169.254.0.0/16", "fc00::/7", "fe80::/10"]
    trusted.extend(str(ipaddress.ip_network(value, strict=False)) for value in allow)
    return {
        "filter.d/lidza-scanners.conf": '''# Managed by lidza-deploy scanner firewall
[Definition]
# Match only the Caddy-controlled socket peer and our fixed probe marker.
failregex = ^.*"request":\\{"remote_ip":"<HOST>".*"lidza_scanner":"forbidden-path".*$
ignoreregex =
datepattern = "ts":{EPOCH}
''',
        "jail.d/lidza-scanners.conf": '''# Managed by lidza-deploy scanner firewall
[lidza-scanners]
enabled = true
filter = lidza-scanners
backend = polling
logpath = /var/log/caddy/lidza-scanners.json
usedns = no
ignoreself = true
maxretry = 1
maxmatches = 1
findtime = 86400
bantime = 7776000
bantime.increment = false
ignoreip = ''' + " ".join(trusted) + '''
# Both TCP HTTP/HTTPS and UDP HTTP/3; SSH and other ports are untouched.
action = nftables-multiport[name=lidza-scanners, port="80,443", protocol="tcp,udp", blocktype=drop]
''',
        "fail2ban.d/90-lidza-scanners.conf": '''# Managed by lidza-deploy scanner firewall
[Definition]
# Retain the database long enough to restore 90-day bans after service/reboot.
dbpurgeage = 7862400
''',
    }


def unprotect(config):
    result = copy.deepcopy(config)
    servers = result.get("apps", {}).get("http", {}).get("servers", {})
    owned = any(any(managed(route, GUARD) for route in server.get("routes", [])) for server in servers.values())
    if not owned:
        return result
    for server in servers.values():
        skipped = any(managed(route, SKIP) for route in server.get("routes", []))
        server["routes"] = [route for route in server.get("routes", []) if not managed(route, GUARD) and not managed(route, SKIP)]
        if skipped and server.get("logs") == {}:
            server.pop("logs")
    logging = result.get("logging", {})
    logs = logging.get("logs", {})
    logs.pop(LOGGER, None)
    default = logs.get("default", {})
    if "http.log.access." + LOGGER in default.get("exclude", []):
        default["exclude"].remove("http.log.access." + LOGGER)
        if not default["exclude"]:
            default.pop("exclude")
    if "default" in logs and not default:
        logs.pop("default")
    if "logs" in logging and not logs:
        logging.pop("logs")
    if "logging" in result and not logging:
        result.pop("logging")
    return result


def api(method, value=None):
    data = None if value is None else json.dumps(value).encode()
    request = urllib.request.Request(API + ("/config/" if method == "GET" else "/load"), data=data, method=method, headers={"Content-Type": "application/json"})
    # Local management never goes through an outbound proxy.
    opener = urllib.request.build_opener(urllib.request.ProxyHandler({}))
    with opener.open(request, timeout=20) as response:
        body = response.read(16 << 20)
        return json.loads(body) if body else None


def check_nftables():
    name = "lidza_scanner_preflight_" + str(os.getpid())
    rules = [f"add table inet {name}",
             f"add chain inet {name} input {{ type filter hook input priority -1; policy accept; }}"]
    for family, kind in [("ip", "ipv4_addr"), ("ip6", "ipv6_addr")]:
        rules.append(f"add set inet {name} peers_{family} {{ type {kind}; }}")
        for protocol in ["tcp", "udp"]:
            rules.append(f"add rule inet {name} input {protocol} dport {{ 80, 443 }} {family} saddr @peers_{family} drop")
    # --check validates kernel support and privileges without applying any rule.
    subprocess.run(["nft", "--check", "--file", "-"], input="\n".join(rules) + "\n", text=True, check=True, timeout=15)


def write(path, data, mode=0o600):
    path.parent.mkdir(parents=True, exist_ok=True)
    temporary = path.with_name(path.name + ".lidza-next")
    with open(temporary, "w", encoding="utf-8") as handle:
        os.chmod(temporary, mode)
        handle.write(data)
    temporary.replace(path)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--config", type=pathlib.Path, help="Read an exported Caddy JSON configuration instead of the live API")
    parser.add_argument("--output", type=pathlib.Path, help="Prepare configuration files here; make no host changes")
    parser.add_argument("--enable", action="store_true", help="Install packages, validate configuration, and enable protection")
    parser.add_argument("--disable", action="store_true", help="Remove only this guard and its jail, preserving other firewall rules and Caddy routes")
    parser.add_argument("--direct-public-traffic", action="store_true", help="Confirm there is no CDN/load-balancer in front of Caddy")
    parser.add_argument("--allow-ip", action="append", default=[], help="Operator/monitoring IP or CIDR exempt from bans; repeat as needed")
    args = parser.parse_args()
    if sum([bool(args.output), args.enable, args.disable]) != 1:
        parser.error("Choose --output DIRECTORY to prepare, --enable to activate, or --disable")
    if args.enable and (not args.direct_public_traffic or args.config):
        parser.error("Activation requires --direct-public-traffic and the live Caddy API; CDN/proxy setups need a different ban location")
    if (args.enable or args.disable) and os.geteuid() != 0:
        parser.error("Activation must run as root")
    files = configurations(args.allow_ip)
    current = json.loads(args.config.read_text()) if args.config else api("GET")
    if args.disable:
        if args.config:
            parser.error("Disabling protection requires the live Caddy API")
        target = pathlib.Path("/etc/fail2ban/jail.d/lidza-scanners.conf")
        if target.exists() and not target.read_text().startswith("# Managed by lidza-deploy"):
            raise ValueError("Refusing to overwrite an operator-managed scanner jail")
        updated = unprotect(current)
        if shutil.which("fail2ban-client"):
            subprocess.run(["fail2ban-client", "stop", "lidza-scanners"], check=False, timeout=15)
        if target.exists():
            write(target, "# Managed by lidza-deploy scanner firewall\n[lidza-scanners]\nenabled = false\n", 0o644)
        if api("GET") != current:
            raise ValueError("Caddy changed during preparation; rerun to preserve those changes")
        api("POST", updated)
        print("Disabled this scanner guard and its web-port jail. Other Caddy routes and firewall policies were preserved.")
        return
    if args.enable and any(server.get("trusted_proxies") for server in current.get("apps", {}).get("http", {}).get("servers", {}).values()):
        raise ValueError("Caddy is configured with trusted proxies; refusing socket-peer bans until proxy topology is reviewed")
    updated = protect(current)
    if args.output:
        write(args.output / "caddy.json", json.dumps(updated, indent=2) + "\n")
        for relative, content in files.items():
            write(args.output / "fail2ban" / relative, content)
        print("Prepared only: first forbidden request rejected; one attempt bans the socket peer on web ports for 90 days. No host changes.")
        return
    caddy = pwd.getpwnam("caddy")
    if not shutil.which("fail2ban-client") or not shutil.which("nft"):
        subprocess.run(["apt-get", "update"], check=True, timeout=300)
        subprocess.run(["apt-get", "install", "-y", "fail2ban", "nftables"], check=True, timeout=600, env={**os.environ, "DEBIAN_FRONTEND": "noninteractive"})
    check_nftables()
    previous_retention = subprocess.run(["fail2ban-client", "get", "dbpurgeage"], capture_output=True, text=True, timeout=15)
    if previous_retention.returncode == 0:
        age = seconds_value(previous_retention.stdout)
        if age > 7862400:
            files["fail2ban.d/90-lidza-scanners.conf"] = files["fail2ban.d/90-lidza-scanners.conf"].replace("7862400", str(age))
    logdir = pathlib.Path(LOGFILE).parent
    if not logdir.exists():
        logdir.mkdir(mode=0o750)
        os.chown(logdir, caddy.pw_uid, caddy.pw_gid)
    logfile = pathlib.Path(LOGFILE)
    if not logfile.exists():
        write(logfile, "", 0o640)
        os.chown(logfile, caddy.pw_uid, caddy.pw_gid)
    state = pathlib.Path("/var/lib/lidza-scanner-firewall")
    state.mkdir(mode=0o700, exist_ok=True)
    write(state / "caddy-before.json", json.dumps(current, indent=2) + "\n")
    planned = state / "caddy-planned.json"
    write(planned, json.dumps(updated, indent=2) + "\n")
    backups = {}
    applied = False
    try:
        for relative, content in files.items():
            target = pathlib.Path("/etc/fail2ban") / relative
            previous = target.read_text() if target.exists() else None
            if previous is not None and not previous.startswith("# Managed by lidza-deploy"):
                raise ValueError("Refusing to overwrite an operator-managed configuration: " + str(target))
            backups[target] = previous
            write(target, content, 0o644)
        subprocess.run(["fail2ban-client", "-t"], check=True, timeout=30)
        subprocess.run(["caddy", "validate", "--config", str(planned)], check=True, timeout=30)
        if api("GET") != current:
            raise ValueError("Caddy configuration changed during preparation; rerun to preserve those changes")
        api("POST", updated)
        applied = True
        subprocess.run(["systemctl", "enable", "--now", "fail2ban"], check=True, timeout=60)
        subprocess.run(["fail2ban-client", "reload"], check=True, timeout=30)
        retention = subprocess.check_output(["fail2ban-client", "get", "dbpurgeage"], text=True, timeout=15).strip()
        if seconds_value(retention) < 7776000:
            raise ValueError("An existing Fail2ban override reduces ban persistence below 90 days")
        database = subprocess.check_output(["fail2ban-client", "get", "dbfile"], text=True, timeout=15)
        if re.search(r"\bNone\s*$", database):
            raise ValueError("Fail2ban's persistent ban database is disabled")
        subprocess.run(["fail2ban-client", "status", "lidza-scanners"], check=True, timeout=15)
    except Exception:
        if applied:
            # Stop only our jail; never flush or stop other firewall policies.
            subprocess.run(["fail2ban-client", "stop", "lidza-scanners"], check=False, timeout=15)
            api("POST", current)
        for target, previous in backups.items():
            if previous is None:
                target.unlink(missing_ok=True)
            else:
                write(target, previous, 0o644)
        subprocess.run(["fail2ban-client", "reload"], check=False, timeout=30)
        raise
    print("Enabled: first PHP/secret-file/backup probe is rejected and bans its direct peer for 90 days on TCP/UDP web ports. SSH remains available.")
    print("Inspect bans: sudo fail2ban-client status lidza-scanners")
    print("Unban an address: sudo fail2ban-client set lidza-scanners unbanip ADDRESS")


if __name__ == "__main__":
    try:
        main()
    except Exception as error:
        print("Scanner firewall setup failed: " + str(error), file=sys.stderr)
        sys.exit(1)
