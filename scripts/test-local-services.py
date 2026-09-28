#!/usr/bin/env python3
"""Synthetic lifecycle tests: no real launchctl, network or service changes."""
import copy
import importlib.util
import json
import os
from pathlib import Path
import plistlib
import subprocess
import tempfile
import unittest
from unittest.mock import patch

spec = importlib.util.spec_from_file_location("local_services", Path(__file__).with_name("local-services.py"))
module = importlib.util.module_from_spec(spec)
spec.loader.exec_module(module)


class LocalServicesTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.root = Path(self.temp.name)
        (self.root / "schemas/db/migrations").mkdir(parents=True)
        binary = self.root / "fixture-server"
        binary.write_text("#!/bin/sh\nexit 0\n")
        binary.chmod(0o700)
        self.cfg = {
            "root": str(self.root), "runtime": str(self.root / "runtime"),
            "api_binary": str(binary), "mcp_binary": str(binary),
            "environment": {
                "CXT_AUTH": "firebase", "CXT_FIREBASE_PROJECT": "fixture-project",
                "CXT_PUBLIC_URL": "http://localhost:5173",
                "CXT_POSTGRES_DSN": "postgres://fixture:synthetic-secret@127.0.0.1:55484/fixture?sslmode=disable",
                "CXT_GITHUB_TOKEN": "synthetic-api-only", "RESEND_API_KEY": "synthetic-mail-only",
                "CXT_ENV_FILE": "/private/api.env",
            },
        }
        self.path = self.root / "config.json"
        self.save()

    def save(self):
        self.path.write_text(json.dumps(self.cfg))
        self.path.chmod(0o600)

    def test_separate_services_share_identity_without_api_write_credentials(self):
        cfg = module.load_config(self.path)
        plists = module.service_plists(cfg)
        api, mcp = plists["com.cxthub.api"], plists["com.cxthub.mcp"]
        for key in module.MCP_KEYS:
            self.assertEqual(api["EnvironmentVariables"][key], mcp["EnvironmentVariables"][key])
        self.assertNotIn("CXT_GITHUB_TOKEN", mcp["EnvironmentVariables"])
        self.assertNotIn("RESEND_API_KEY", mcp["EnvironmentVariables"])
        self.assertEqual(mcp["EnvironmentVariables"]["CXT_ENV_FILE"], "/dev/null")
        for value in plists.values():
            self.assertNotIn("synthetic-secret", " ".join(value["ProgramArguments"]))
            self.assertNotIn("--data", value["ProgramArguments"])
            self.assertTrue(value["KeepAlive"])
        self.assertNotEqual(api["ProgramArguments"][-1], mcp["ProgramArguments"][-1])

    def test_rejects_remote_routing_and_implicit_auth(self):
        for value in ("", "postgres://user@cloud.example:5432/db", "postgres://user@127.0.0.1:5432/postgres",
                      "postgres://user@127.0.0.1:5432/db?host=cloud.example", "postgres://user@127.0.0.1:5432/db?service=external"):
            with self.subTest(dsn_shape=value.split("?")[-1]):
                bad = copy.deepcopy(self.cfg)
                bad["environment"]["CXT_POSTGRES_DSN"] = value
                self.path.write_text(json.dumps(bad))
                with self.assertRaises(module.ConfigurationError): module.load_config(self.path)
        for key, value in (("CXT_AUTH", ""), ("CXT_FIREBASE_PROJECT", ""), ("CXT_PUBLIC_URL", "https://cloud.example"), ("LD_PRELOAD", "unsafe")):
            bad = copy.deepcopy(self.cfg)
            bad["environment"][key] = value
            self.path.write_text(json.dumps(bad))
            with self.assertRaises(module.ConfigurationError): module.load_config(self.path)

    def test_private_files_and_atomic_configuration(self):
        self.path.chmod(0o644)
        with self.assertRaises(module.ConfigurationError): module.load_config(self.path)
        self.save()
        linked = self.root / "link"
        linked.symlink_to(self.path)
        with self.assertRaises(OSError): module.load_config(linked)
        out = self.root / "agents/api.plist"
        value = module.service_plists(self.cfg)["com.cxthub.api"]
        module.atomic_private(out, plistlib.dumps(value))
        self.assertEqual(out.stat().st_mode & 0o777, 0o600)
        self.assertEqual(plistlib.loads(out.read_bytes()), value)
        out.unlink()
        out.symlink_to(self.path)
        before = self.path.read_bytes()
        with self.assertRaises(module.ConfigurationError): module.atomic_private(out, b"unsafe")
        self.assertEqual(self.path.read_bytes(), before)

    def run_main(self, action):
        with patch.object(module.sys, "argv", ["local-services", action, "--config", str(self.path)]), patch.object(module.sys, "platform", "darwin"), patch.object(module.Path, "home", return_value=self.root):
            module.main()

    def test_configure_is_not_cutover_and_stop_touches_only_own_services(self):
        calls = []
        def launch(*args, **kwargs):
            calls.append(args)
            return False
        with patch.object(module, "launch", side_effect=launch):
            self.run_main("configure")
        self.assertTrue(all(call[0] == "print" for call in calls))
        calls.clear()
        with patch.object(module, "launch", side_effect=lambda *args, **kwargs: calls.append(args) or True):
            self.run_main("stop")
        stops = [call[1] for call in calls if call[0] == "bootout"]
        self.assertEqual(stops, [f"gui/{os.getuid()}/com.cxthub.api", f"gui/{os.getuid()}/com.cxthub.mcp"])
        self.assertFalse(any("com.cxthub.cxtd" in str(call) or "postgres" in str(call) for call in calls))

    def test_start_rolls_back_only_newly_started_service_on_failure(self):
        with patch.object(module, "launch", return_value=False): self.run_main("configure")
        calls = []
        def launch(*args, **kwargs):
            calls.append(args)
            if args[0] == "print": return False
            if args[0] == "bootstrap" and args[-1].endswith("com.cxthub.api.plist"):
                raise module.ConfigurationError("synthetic bootstrap failure")
            return True
        with patch.object(module, "launch", side_effect=launch), patch.object(module.socket, "socket"):
            with self.assertRaises(module.ConfigurationError): self.run_main("start")
        self.assertEqual([c for c in calls if c[0] == "bootout"], [("bootout", f"gui/{os.getuid()}/com.cxthub.mcp")])

    def test_launchctl_errors_never_echo_private_environment(self):
        with patch.object(module.subprocess, "run", return_value=subprocess.CompletedProcess([], 1, b"synthetic-secret", b"synthetic-secret")):
            with self.assertRaises(module.ConfigurationError) as raised: module.launch("print", "fixture")
        self.assertNotIn("synthetic-secret", str(raised.exception))

    def test_unrelated_listener_is_not_readiness(self):
        for listener, want in ((b"123\n", True), (b"456\n", False), (b"", False)):
            with patch.object(module.subprocess, "run", side_effect=[
                subprocess.CompletedProcess([], 0, b"\tpid = 123\n", b""),
                subprocess.CompletedProcess([], 0, listener, b""),
            ]):
                self.assertEqual(module.service_listens("gui/501", "com.cxthub.api", 8907), want)


if __name__ == "__main__":
    unittest.main()
