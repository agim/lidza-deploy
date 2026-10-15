#!/usr/bin/env python3
import importlib.util
import json
import pathlib
import subprocess
import sys
import tempfile
import unittest

sys.dont_write_bytecode = True

SOURCE = pathlib.Path(__file__).resolve().parents[1] / "internal/agent/scanner-firewall.py"
spec = importlib.util.spec_from_file_location("scanner_firewall", SOURCE)
firewall = importlib.util.module_from_spec(spec)
spec.loader.exec_module(firewall)


class ScannerFirewallTests(unittest.TestCase):
    def fixture(self):
        return {"apps": {"tls": {"automation": {"policies": [{"on_demand": True}], "on_demand": {"permission": {"module": "http", "endpoint": "http://127.0.0.1:9090/tls/allow"}}}}, "http": {"servers": {
            "public": {"listen": [":18888"], "routes": [{"handle": [{"handler": "static_response", "body": "ok", "status_code": 200}]}]},
            "gui": {"listen": [":18889"], "routes": [{"match": [{"host": ["deploy.example.com"]}], "handle": [{"handler": "reverse_proxy", "upstreams": [{"dial": "127.0.0.1:3000"}]}]}]},
        }}}}

    def test_preserves_tls_routing_and_is_idempotent(self):
        original = self.fixture()
        updated = firewall.protect(original)
        self.assertEqual(updated["apps"]["tls"], original["apps"]["tls"])
        ids = []
        for name, server in updated["apps"]["http"]["servers"].items():
            self.assertEqual(server["routes"][-1], original["apps"]["http"]["servers"][name]["routes"][0])
            self.assertEqual(len(server["routes"]), 3)
            self.assertEqual(server["routes"][1]["handle"][0]["log_skip"], True)
            ids.append(server["routes"][0]["@id"])
        self.assertEqual(len(set(ids)), 2)
        self.assertEqual(firewall.protect(updated), updated)
        self.assertEqual(firewall.unprotect(updated), original)
        self.assertNotIn("logging", original)

    def test_operator_logging_is_preserved_and_sensitive_fields_are_removed(self):
        current = self.fixture()
        current["apps"]["http"]["servers"]["gui"]["logs"] = {"default_logger_name": "operator"}
        updated = firewall.protect(current)
        self.assertEqual(updated["apps"]["http"]["servers"]["gui"]["logs"], {"default_logger_name": "operator"})
        self.assertEqual(len(updated["apps"]["http"]["servers"]["gui"]["routes"]), 2)
        fields = updated["logging"]["logs"][firewall.LOGGER]["encoder"]["fields"]
        self.assertEqual(fields["request>headers"]["filter"], "delete")
        self.assertEqual(fields["request>uri"]["filter"], "replace")

    def test_bans_first_hit_for_90_days_only_on_web_ports(self):
        files = firewall.configurations(["203.0.113.5"])
        jail = files["jail.d/lidza-scanners.conf"]
        self.assertIn("maxretry = 1", jail)
        self.assertIn("bantime = 7776000", jail)
        self.assertIn('port="80,443", protocol="tcp,udp"', jail)
        self.assertIn("203.0.113.5/32", jail)
        self.assertIn("7862400", files["fail2ban.d/90-lidza-scanners.conf"])
        self.assertNotIn("Forwarded", files["filter.d/lidza-scanners.conf"])
        with self.assertRaises(ValueError):
            firewall.configurations(["1.2.3.4; run-command"])

    def test_prepare_makes_reviewable_files_without_activation(self):
        with tempfile.TemporaryDirectory() as directory:
            root = pathlib.Path(directory)
            source = root / "original.json"
            source.write_text(json.dumps(self.fixture()))
            output = root / "review"
            result = subprocess.run(["python3", str(SOURCE), "--config", str(source), "--output", str(output)], capture_output=True, text=True)
            self.assertEqual(result.returncode, 0, result.stderr)
            self.assertTrue((output / "caddy.json").is_file())
            self.assertTrue((output / "fail2ban/jail.d/lidza-scanners.conf").is_file())
            self.assertEqual(json.loads(source.read_text()), self.fixture())
            self.assertEqual((output / "caddy.json").stat().st_mode & 0o777, 0o600)

    def test_enable_requires_explicit_proxy_topology(self):
        result = subprocess.run(["python3", str(SOURCE), "--enable"], capture_output=True, text=True)
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("--direct-public-traffic", result.stderr)

    def test_fail2ban_human_and_raw_retention_output(self):
        self.assertEqual(firewall.seconds_value("Current database purge age is:\n`- 7862400seconds\n"), 7862400)
        self.assertEqual(firewall.seconds_value("7862400\n"), 7862400)


if __name__ == "__main__":
    unittest.main()
