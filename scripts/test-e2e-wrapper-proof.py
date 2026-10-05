#!/usr/bin/env python3
"""Synthetic file-only tests; no providers, Git or child processes."""
import contextlib
import importlib.util
import io
import json
import os
from pathlib import Path
import re
import tempfile
import unittest


spec = importlib.util.spec_from_file_location("proof", Path(__file__).with_name("e2e-wrapper-proof.py"))
proof = importlib.util.module_from_spec(spec)
spec.loader.exec_module(proof)
ORIGINAL = "11111111-1111-4111-8111-111111111111"
SEED = "22222222-2222-4222-8222-222222222222"
OTHER = "33333333-3333-4333-8333-333333333333"


class WrapperProofTests(unittest.TestCase):
    def setUp(self):
        temp = tempfile.TemporaryDirectory(prefix="wrapper-proof-")
        self.addCleanup(temp.cleanup)
        self.base = Path(temp.name).resolve()
        (self.base / "repo2/.cxt").mkdir(parents=True)
        project_name = re.sub(r"[^A-Za-z0-9]", "-", str(self.base / "repo2"))
        self.project = self.base / "home/.claude/projects" / project_name
        self.project.mkdir(parents=True)
        self.seed = self.project / (SEED + ".jsonl")
        self.seed.write_text("synthetic body must not enter diagnostics")
        self.original = self.project / (ORIGINAL + ".jsonl.superseded")
        self.original.write_text("synthetic original body must not enter diagnostics")
        self.boundary = self.base / "repo2/.cxt/boundary.json"
        self.document = {"prev_branch": "main", "branch": "feature-x", "seed_id": SEED,
                         "seed_path": str(self.seed), "resume_cmd": "claude --resume " + SEED,
                         "superseded": [str(self.original)]}
        self.boundary.write_text(json.dumps(self.document))
        self.log = self.base / "agent.log"
        self.lines = [self.agent(["--resume", ORIGINAL, "--settings", json.dumps({"hint": "--resume " + SEED})]),
                      self.agent(["--resume", SEED, "--settings", json.dumps({"hint": "--resume " + OTHER})])]
        self.write_log(self.lines + ["MEMORY_PROFILE v1 64 /fixture/memory"])

    def agent(self, argv):
        return "AGENT " + json.dumps(argv)

    def write_log(self, lines):
        self.log.write_text("\n".join(lines) + "\n")

    def invoke(self, *args):
        out, err = io.StringIO(), io.StringIO()
        with contextlib.redirect_stdout(out), contextlib.redirect_stderr(err):
            code = proof.main(list(args) if args else [str(self.base), ORIGINAL])
        return code, out.getvalue(), err.getvalue()

    def reject(self):
        code, out, err = self.invoke()
        self.assertEqual(code, 1)
        self.assertEqual(out, "")
        self.assertTrue(err.startswith("wrapper proof failed:"))
        for private in (str(self.base), "synthetic body", "private-marker"):
            self.assertNotIn(private, err)

    def test_success_emits_only_three_safe_tsv_fields(self):
        before = self.seed.stat()
        self.assertEqual(self.invoke(), (0, f"{self.seed}\t{SEED}\t{SEED}\n", ""))
        self.assertEqual(self.seed.stat().st_mtime_ns, before.st_mtime_ns)

    def test_absent_malformed_nonobject_and_duplicate_boundary(self):
        self.boundary.unlink()
        self.reject()
        for raw in ("private-marker", "{", "[]", "null", "[" * 2000,
                    json.dumps(self.document)[:-1] + ',"seed_id":"private-marker"}'):
            with self.subTest(raw=raw[:20]):
                self.boundary.write_text(raw)
                self.reject()

    def test_empty_missing_and_mismatched_boundary_fields(self):
        cases = [("seed_id", ""), ("seed_id", ORIGINAL), ("seed_id", "../private-marker"),
                 ("seed_id", []), ("seed_id", OTHER), ("prev_branch", "other"),
                 ("branch", "feature-x-other"), ("seed_path", ""), ("seed_path", {}),
                 ("resume_cmd", "claude --resume " + OTHER),
                 ("resume_cmd", "cxt claude --resume " + SEED),
                 ("resume_cmd", "claude --resume " + SEED + " --continue")]
        for key, value in cases:
            with self.subTest(key=key, value=value):
                self.boundary.write_text(json.dumps(dict(self.document, **{key: value})))
                self.reject()
        for key in self.document:
            record = dict(self.document)
            del record[key]
            self.boundary.write_text(json.dumps(record))
            self.reject()

    def test_zero_one_or_three_children_and_incomplete_log(self):
        for lines in ([], self.lines[:1], self.lines + [self.lines[1]]):
            self.write_log(lines)
            self.reject()
        self.log.write_text("\n".join(self.lines))
        self.reject()

    def test_exact_prefix_and_leading_selector_not_settings_substrings(self):
        bad = ["prefix" + self.lines[1], " " + self.lines[1], self.lines[1].replace("AGENT ", "AGENT-other "),
               self.agent(["--resume", SEED + "-suffix"]), self.agent(["--resume", "prefix-" + SEED]),
               self.agent(["--resume=" + SEED]), self.agent(["--settings", "--resume " + SEED]),
               self.agent(["--resume", OTHER, "--settings", json.dumps({"hint": "--resume " + SEED})])]
        for line in bad:
            with self.subTest(line=line):
                self.write_log([self.lines[0], line])
                self.reject()
        self.write_log([self.agent(["--resume", OTHER]), self.lines[1]])
        self.reject()
        self.write_log(list(reversed(self.lines)))
        self.reject()

    def test_duplicate_resume_flags_and_invalid_argv_records(self):
        invalid = [["--resume", SEED, "--resume", OTHER], ["--resume", SEED, "--resume", SEED],
                   ["--resume", SEED, "--resume=" + OTHER], ["--resume", SEED, 7],
                   ["--resume"], [], {}, None, "--resume " + SEED]
        for argv in invalid:
            with self.subTest(argv=argv):
                self.write_log([self.lines[0], self.agent(argv)])
                self.reject()
        for malformed in ("AGENT [", "AGENT " + "[" * 2000, "AGENT --resume " + SEED):
            self.write_log([self.lines[0], malformed])
            self.reject()
        self.write_log([self.agent(["--resume", ORIGINAL, "--resume", ORIGINAL]), self.lines[1]])
        self.reject()

    def test_original_must_be_named_exactly_in_superseded_list(self):
        for value in (None, {}, "private-marker", [], [None], [str(self.seed)],
                      [str(self.original) + "-other"], [str(self.project / (OTHER + ".jsonl.superseded"))]):
            with self.subTest(superseded=value):
                self.boundary.write_text(json.dumps(dict(self.document, superseded=value)))
                self.reject()

    def test_seed_path_cannot_escape_or_use_a_sibling_prefix(self):
        for path in (self.base / (SEED + ".jsonl"),
                     self.project.with_name(self.project.name + "-other") / self.seed.name,
                     self.project / (OTHER + ".jsonl")):
            path.parent.mkdir(parents=True, exist_ok=True)
            path.write_text("private-marker")
            self.boundary.write_text(json.dumps(dict(self.document, seed_path=str(path))))
            self.reject()

    def test_missing_directory_fifo_symlink_and_hardlinked_provider_files(self):
        target = self.base / "outside"
        target.write_text("private-marker")
        for path in (self.seed, self.original):
            with self.subTest(path=path.name):
                path.unlink()
                self.reject()
                path.mkdir()
                self.reject()
                path.rmdir()
                os.mkfifo(path)
                self.reject()  # Nonblocking open; does not wait for a writer.
                path.unlink()
                path.symlink_to(target)
                self.reject()
                path.unlink()
                os.link(target, path)
                self.reject()
                path.unlink()
                path.write_text("synthetic fixture body")

    def test_symlinked_project_and_metadata_are_rejected(self):
        moved = self.base / "moved-project"
        self.project.rename(moved)
        self.project.symlink_to(moved, target_is_directory=True)
        self.reject()
        self.project.unlink()
        moved.rename(self.project)
        for path in (self.boundary, self.log):
            original = path.with_suffix(".original")
            path.rename(original)
            path.symlink_to(original)
            self.reject()
            path.unlink()
            original.rename(path)

    def test_limits_and_invalid_cli_never_emit_tsv(self):
        for path in (self.boundary, self.log):
            original = path.read_bytes()
            path.write_bytes(b"x" * (proof.LIMIT + 1))
            self.reject()
            path.write_bytes(original)
        for args in ((str(self.base), ""), (str(self.base), SEED + "\tprivate-marker"),
                     ("relative-fixture", ORIGINAL), (str(self.base),)):
            code, out, err = self.invoke(*args)
            self.assertEqual(code, 1)
            self.assertEqual(out, "")
            self.assertNotIn("private-marker", err)


if __name__ == "__main__":
    unittest.main()
