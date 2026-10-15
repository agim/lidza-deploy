#!/usr/bin/env python3
"""Run only in the dedicated disposable NET_ADMIN Docker fixture."""
import json
import importlib.util
import os
import pathlib
import re
import shutil
import subprocess
import tempfile
import time
import sys

if os.environ.get("LIDZA_FIREWALL_FIXTURE") != "1" or not pathlib.Path("/.dockerenv").exists():
    raise SystemExit("Refusing to run outside the explicitly selected disposable Docker firewall fixture")

sys.dont_write_bytecode = True
spec = importlib.util.spec_from_file_location("scanner_firewall", "/helper.py")
helper = importlib.util.module_from_spec(spec)
spec.loader.exec_module(helper)
helper.check_nftables()
assert "lidza_scanner_preflight_" not in subprocess.run(
    ["nft", "list", "tables"], check=True, capture_output=True, text=True).stdout


def run(*args, check=True):
    return subprocess.run(args, check=check, capture_output=True, text=True)


def wait_for(check):
    deadline = time.monotonic() + 15
    while time.monotonic() < deadline:
        if check():
            return
        time.sleep(.1)
    raise AssertionError("Timed out waiting for Fail2ban/nftables state")


with tempfile.TemporaryDirectory() as directory:
    root = pathlib.Path(directory)
    config = root / "config"
    shutil.copytree("/etc/fail2ban", config)
    for path in (config / "jail.d").glob("*"):
        path.unlink()
    for folder in ["filter.d", "jail.d", "fail2ban.d"]:
        (config / folder).mkdir(exist_ok=True)
        for source in (pathlib.Path("/review/fail2ban") / folder).glob("*.conf"):
            shutil.copyfile(source, config / folder / source.name)
    log = root / "probes.json"
    log.touch()
    jail = config / "jail.d/lidza-scanners.conf"
    jail.write_text(jail.read_text().replace("/var/log/caddy/lidza-scanners.json", str(log)))
    (config / "jail.local").write_text("[sshd]\nenabled = false\n")
    (config / "fail2ban.local").write_text(f"[Definition]\ndbfile = {root / 'bans.sqlite3'}\nlogtarget = {root / 'daemon.log'}\n")
    socket = str(root / "server.sock")
    pid = str(root / "server.pid")

    def client(*args, check=True):
        return run("fail2ban-client", "-s", socket, *args, check=check)

    def start():
        run("fail2ban-server", "-c", str(config), "-s", socket, "-p", pid, "-b")
        wait_for(lambda: client("status", "lidza-scanners", check=False).returncode == 0)

    run("nft", "add", "table", "inet", "lidza_fixture_existing")
    try:
        start()
        assert client("get", "lidza-scanners", "bantime").stdout.strip() == "7776000"
        assert int(re.search(r"(\d+)\s*(?:seconds)?\s*$", client("get", "dbpurgeage").stdout).group(1)) >= 7776000
        with log.open("a") as handle:
            for address in ["203.0.113.9", "2001:db8::7"]:
                handle.write(json.dumps({"level": "info", "ts": time.time(), "request": {"remote_ip": address, "uri": "redacted-probe"}, "lidza_scanner": "forbidden-path"}, separators=(",", ":")) + "\n")
        wait_for(lambda: all(address in client("get", "lidza-scanners", "banip").stdout for address in ["203.0.113.9", "2001:db8::7"]))
        wait_for(lambda: all(address in run("nft", "list", "table", "inet", "f2b-table", check=False).stdout for address in ["203.0.113.9", "2001:db8::7"]))
        rules = run("nft", "list", "table", "inet", "f2b-table").stdout
        assert "203.0.113.9" in rules and "2001:db8::7" in rules
        assert "tcp dport { 80, 443 }" in rules and "udp dport { 80, 443 }" in rules
        assert "22" not in rules
        client("stop")
        wait_for(lambda: not pathlib.Path(socket).exists())
        start()
        wait_for(lambda: "203.0.113.9" in client("get", "lidza-scanners", "banip").stdout)
        wait_for(lambda: all(address in run("nft", "list", "table", "inet", "f2b-table", check=False).stdout for address in ["203.0.113.9", "2001:db8::7"]))
        client("set", "lidza-scanners", "unbanip", "203.0.113.9")
        wait_for(lambda: "203.0.113.9" not in run("nft", "list", "table", "inet", "f2b-table").stdout)
        run("nft", "list", "table", "inet", "lidza_fixture_existing")
        print("PASS: one-hit IPv4/IPv6 bans, TCP/UDP web-port rules, 90-day retention, restart restoration, unban and preservation of an unrelated nftables table")
    except Exception:
        print((root / "daemon.log").read_text())
        print(run("nft", "list", "ruleset", check=False).stdout)
        raise
    finally:
        client("stop", check=False)
