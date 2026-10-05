#!/usr/bin/env python3
import importlib.util
import json
from pathlib import Path
import subprocess
import sys
import tempfile
import unittest

SCRIPT = Path(__file__).with_name("e2e-repair-proof.py")
spec = importlib.util.spec_from_file_location("proof", SCRIPT)
proof = importlib.util.module_from_spec(spec)
spec.loader.exec_module(proof)
REPO, BASE, LOCAL = ("sha256:" + char * 64 for char in "abc")


class RepairProofTest(unittest.TestCase):
    def test_requires_matching_repository_published_baseline_and_absent_local(self):
        good = {"repo_id": REPO, "snapshot_index": [BASE]}
        self.assertIn(LOCAL, proof.local_only(good, REPO, BASE, LOCAL))
        for manifest in (None, {}, dict(good, repo_id=BASE), dict(good, snapshot_index={}),
                         dict(good, snapshot_index=[]), dict(good, snapshot_index=[BASE, LOCAL]),
                         dict(good, snapshot_index=[BASE, "not a hash"])):
            with self.subTest(manifest=manifest), self.assertRaises(ValueError):
                proof.local_only(manifest, REPO, BASE, LOCAL)
        for local in ("", None, BASE, "sha256:bad"):
            with self.subTest(local=local), self.assertRaises(ValueError):
                proof.local_only(good, REPO, BASE, local)

    def test_doctor_rejects_incomplete_or_malformed_even_under_optimization(self):
        with tempfile.TemporaryDirectory() as tmp:
            path = Path(tmp) / "doctor.json"
            for report, code in (({"completed": True, "issues": []}, 0),
                                 ({"completed": True, "issues": [{"kind": "corrupt"}]}, 0),
                                 ({"completed": False, "issues": []}, 1),
                                 ({"completed": 1, "issues": []}, 1),
                                 ({"completed": True, "issues": None}, 1), ({}, 1)):
                path.write_text(json.dumps(report))
                result = subprocess.run([sys.executable, "-O", str(SCRIPT), "doctor", str(path)], capture_output=True, text=True)
                self.assertEqual(result.returncode, code, result.stderr)
                if not code:
                    self.assertEqual(result.stdout.strip(), str(len(report["issues"])))
            path.write_text('{"completed":false,"completed":true,"issues":[]}')
            self.assertNotEqual(subprocess.run([sys.executable, "-O", str(SCRIPT), "doctor", str(path)], capture_output=True).returncode, 0)


if __name__ == "__main__":
    unittest.main()
