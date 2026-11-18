#!/usr/bin/env python3

import json
from pathlib import Path
import subprocess
import sys
import tempfile
import unittest


GATE = Path(__file__).resolve().with_name("npm-audit-gate.py")
PICOMATCH = "GHSA-fc5q-39xc-vjpf"
OTHER = "GHSA-aaaa-bbbb-cccc"


def advisory(identifier, package, severity="high"):
    return {"source": 1, "name": package, "title": f"{package} problem", "url": f"https://github.com/advisories/{identifier}", "severity": severity, "range": "<=1.0.0"}


def report(*advisories):
    vulnerabilities = {}
    for item in advisories:
        vulnerabilities[item["name"]] = {"name": item["name"], "severity": item["severity"], "via": [item], "fixAvailable": False}
        # A dependent package lists the vulnerable one by name only.
        vulnerabilities["depends-on-" + item["name"]] = {"name": "depends-on-" + item["name"], "severity": item["severity"], "via": [item["name"]]}
    return {"auditReportVersion": 2, "vulnerabilities": vulnerabilities}


def exception(identifier=PICOMATCH, package="picomatch", expires="2026-12-16", **changes):
    entry = {"advisory": identifier, "package": package, "reason": "No patched release exists; build tooling only.", "expires": expires}
    entry.update(changes)
    return entry


class NpmAuditGateTest(unittest.TestCase):
    def gate(self, audit, exceptions=None, today="2026-11-18"):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            saved = root / "audit.json"
            saved.write_text(audit if isinstance(audit, str) else json.dumps(audit))
            arguments = [sys.executable, str(GATE), "--report", str(saved), "--today", today, "--exceptions", str(root / "exceptions.json")]
            if exceptions is not None:
                (root / "exceptions.json").write_text(exceptions if isinstance(exceptions, str) else json.dumps(exceptions))
            return subprocess.run(arguments, capture_output=True, text=True, check=False)

    def test_clean_report_passes_without_exceptions(self):
        result = self.gate(report())
        self.assertEqual(result.returncode, 0, result.stderr)

    def test_high_and_critical_advisories_block(self):
        for severity in ("high", "critical"):
            result = self.gate(report(advisory(PICOMATCH, "picomatch", severity)))
            self.assertEqual(result.returncode, 1)
            self.assertIn(PICOMATCH, result.stderr)

    def test_lower_severities_do_not_block(self):
        result = self.gate(report(advisory(OTHER, "left-pad", "moderate"), advisory(PICOMATCH, "picomatch", "low")))
        self.assertEqual(result.returncode, 0, result.stderr)

    def test_dated_exception_lets_only_its_advisory_through(self):
        result = self.gate(report(advisory(PICOMATCH, "picomatch")), [exception()])
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertIn("excepted until 2026-12-16", result.stdout)
        result = self.gate(report(advisory(PICOMATCH, "picomatch"), advisory(OTHER, "left-pad")), [exception()])
        self.assertEqual(result.returncode, 1)
        self.assertIn(OTHER, result.stderr)
        self.assertNotIn(f"blocking: {PICOMATCH}", result.stderr)

    def test_exception_applies_through_its_last_day_and_then_blocks(self):
        audit = report(advisory(PICOMATCH, "picomatch"))
        self.assertEqual(self.gate(audit, [exception()], today="2026-12-16").returncode, 0)
        expired = self.gate(audit, [exception()], today="2026-12-17")
        self.assertEqual(expired.returncode, 1)
        self.assertIn("exception expired 2026-12-16", expired.stderr)

    def test_exception_is_bound_to_the_reviewed_package(self):
        result = self.gate(report(advisory(PICOMATCH, "another-package")), [exception()])
        self.assertEqual(result.returncode, 1)
        self.assertIn("reviewed for picomatch", result.stderr)

    def test_unused_exception_warns_without_blocking(self):
        result = self.gate(report(), [exception()])
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertIn(f"::warning::audit exception {PICOMATCH}", result.stdout)

    def test_malformed_exceptions_fail_closed(self):
        audit = report()
        for broken in (
            "not json",
            {"advisory": PICOMATCH},
            [exception(reason="short")],
            [exception(expires="next month")],
            [exception(identifier="CVE-2026-1")],
            [exception(), exception()],
            [exception(note="extra field")],
            [{"advisory": PICOMATCH, "package": "picomatch", "expires": "2026-12-16"}],
        ):
            result = self.gate(audit, broken)
            self.assertEqual(result.returncode, 2, broken)

    def test_unusable_audit_output_fails_closed(self):
        for broken in ("", "npm ERR! network", {"error": {"code": "ENOTFOUND"}}, {"auditReportVersion": 2}, []):
            result = self.gate(broken, [exception()])
            self.assertEqual(result.returncode, 2, broken)

    def test_advisory_without_identifier_cannot_be_excepted(self):
        unnamed = advisory(PICOMATCH, "picomatch")
        unnamed["url"] = "https://example.invalid/advisory"
        result = self.gate(report(unnamed), [exception()])
        self.assertEqual(result.returncode, 1)
        self.assertIn("https://example.invalid/advisory", result.stderr)

    def test_repository_exceptions_are_valid(self):
        committed = GATE.parent.parent / "frontend" / "web" / "audit-exceptions.json"
        result = self.gate(report(), committed.read_text(), today="2026-11-18")
        self.assertEqual(result.returncode, 0, result.stderr)


if __name__ == "__main__":
    unittest.main()
