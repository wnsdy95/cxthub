#!/usr/bin/env python3
import importlib.util
import os
from pathlib import Path
import tempfile
import unittest

spec = importlib.util.spec_from_file_location("diagnostics", Path(__file__).with_name("e2e-collect-diagnostics.py"))
diagnostics = importlib.util.module_from_spec(spec)
spec.loader.exec_module(diagnostics)


class DiagnosticsTest(unittest.TestCase):
    def test_only_allowlisted_regular_files_are_retained(self):
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp)
            source, output = root / "fixture", root / "output"
            source.mkdir()
            (source / "setup.out").write_text("the first failure, exactly")
            (source / "earlier-branch.out").write_text("what Git said when the branch was made")
            for name in ("cookies", "auth.json", "session.jsonl", "srv.log", ".env", "agent.log", "boundary.json", "seed.jsonl"):
                (source / name).write_text("must not be collected")
            (source / "removed.out").symlink_to(source / ".env")
            entries = diagnostics.collect(source, output)
            self.assertEqual([entry["file"] for entry in entries], ["setup.out", "earlier-branch.out"])
            self.assertEqual((output / "setup.out").read_bytes(), (source / "setup.out").read_bytes())
            self.assertEqual((output / "earlier-branch.out").read_bytes(), (source / "earlier-branch.out").read_bytes())
            self.assertEqual({p.name for p in output.iterdir()}, {"setup.out", "earlier-branch.out", "index.json"})

    def test_oversized_output_is_bounded_and_explicitly_truncated(self):
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp)
            source = root / "fixture"
            source.mkdir()
            (source / "setup.out").write_bytes(b"prefix" + b"x" * diagnostics.MAX_BYTES)
            entries = diagnostics.collect(source, root / "output")
            self.assertEqual(entries, [{"file": "setup.out.tail", "bytes": diagnostics.MAX_BYTES, "truncated": True}])
            self.assertFalse((root / "output/setup.out").exists())

    def test_output_cannot_be_removed_with_fixture_cleanup(self):
        with tempfile.TemporaryDirectory() as tmp:
            with self.assertRaises(ValueError):
                diagnostics.collect(tmp, Path(tmp) / "output")

    def test_existing_destinations_are_rejected_without_writes(self):
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp)
            source, output, unrelated = root / "fixture", root / "output", root / "unrelated"
            source.mkdir()
            output.mkdir()
            unrelated.write_text("preserve")
            (source / "setup2.out").write_text("new result")
            for name in ("setup2.out", "index.json"):
                (output / name).symlink_to(unrelated)
            (output / "stale-private.txt").write_text("must not upload")
            with self.assertRaises(FileExistsError):
                diagnostics.collect(source, output)
            self.assertEqual(unrelated.read_text(), "preserve")
            alias = root / "alias"
            alias.symlink_to(output)
            with self.assertRaises(FileExistsError):
                diagnostics.collect(source, alias)

    def test_hardlinked_sources_are_excluded(self):
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp)
            source = root / "fixture"
            source.mkdir()
            secret = root / "credential-marker"
            secret.write_text("must not collect")
            os.link(secret, source / "removed.out")
            self.assertEqual(diagnostics.collect(source, root / "output"), [])


if __name__ == "__main__":
    unittest.main()
