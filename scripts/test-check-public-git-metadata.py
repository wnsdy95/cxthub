#!/usr/bin/env python3

import json
import os
from pathlib import Path
import subprocess
import sys
import tempfile
import unittest


CHECKER = Path(__file__).resolve().with_name("check-public-git-metadata.py")
APPROVED_COMMIT = b"9fe43f1dee4c296b844b9576269f1bf32645a47e"
APPROVED_LINE = b"- live signed-in inspection of /jy" b"min/cxthub/settings"
FORBIDDEN_TERMS = (b"s" b"wyg", b"rii" b"do", b"team-context-save-" b"fork", b"jy" b"min")
FAKE_GIT = '''
import json
import os
from pathlib import Path
import sys

root = Path(os.environ["METADATA_FIXTURE"])
(root / "arguments.json").write_text(json.dumps(sys.argv[1:]))
with (root / "history.bin").open("rb") as source:
    while True:
        chunk = source.read(65536)
        if not chunk:
            break
        sys.stdout.buffer.write(chunk)
sys.stdout.buffer.flush()
(root / "completed").write_text("yes")
sys.exit(int(os.environ["METADATA_STATUS"]))
'''


def record(commit=b"1" * 40, message=b"Public change\n", identities=None):
    if identities is None:
        identities = [b"Author", b"author@example.invalid", b"Committer", b"committer@example.invalid"]
    return b"\0".join([commit, *identities, message, b""])


class PublicGitMetadataTest(unittest.TestCase):
    def setUp(self):
        temporary = tempfile.TemporaryDirectory()
        self.addCleanup(temporary.cleanup)
        self.root = Path(temporary.name)
        self.fake_git = self.root / "git"
        self.fake_git.write_text(f"#!{sys.executable}\n" + FAKE_GIT)
        self.fake_git.chmod(0o755)

    def run_check(self, history, status=0):
        (self.root / "history.bin").write_bytes(history)
        (self.root / "completed").unlink(missing_ok=True)
        return subprocess.run(
            [sys.executable, str(CHECKER)], cwd=self.root,
            env=dict(os.environ, PATH=str(self.root), METADATA_FIXTURE=str(self.root),
                     METADATA_STATUS=str(status)),
            capture_output=True, text=True, timeout=10,
        )

    def assert_rejected(self, result):
        self.assertEqual(result.returncode, 1, result.stdout + result.stderr)
        self.assertNotIn("passed", result.stdout)

    def test_clean_history_and_complete_git_format(self):
        result = self.run_check(record() + record(b"2" * 40))
        self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
        self.assertEqual(json.loads((self.root / "arguments.json").read_text()), [
            "log", "--all", "--no-show-signature", "-z",
            "--format=%H%x00%an%x00%ae%x00%cn%x00%ce%x00%B",
        ])

    def test_only_exact_approved_commit_message_line_is_allowed(self):
        result = self.run_check(record(APPROVED_COMMIT, b"Verification\n\n" + APPROVED_LINE + b"\nDone\n"))
        self.assertEqual(result.returncode, 0, result.stdout + result.stderr)

    def test_same_line_on_wrong_commit_is_rejected(self):
        self.assert_rejected(self.run_check(record(b"2" * 40, APPROVED_LINE + b"\n")))

    def test_other_path_or_nonexact_line_is_rejected(self):
        for line in (
            APPROVED_LINE.replace(b"/settings", b"/profile"),
            APPROVED_LINE + b"/extra",
            APPROVED_LINE + b"?view=public",
            b"prefix " + APPROVED_LINE,
            APPROVED_LINE + b" suffix",
            b" " + APPROVED_LINE,
            APPROVED_LINE + b" ",
            APPROVED_LINE + b"\r",
            APPROVED_LINE.upper(),
        ):
            with self.subTest(line=line):
                self.assert_rejected(self.run_check(record(APPROVED_COMMIT, line + b"\n")))

    def test_duplicate_approved_line_is_rejected(self):
        self.assert_rejected(self.run_check(record(APPROVED_COMMIT, (APPROVED_LINE + b"\n") * 2)))

    def test_additional_body_identifier_is_not_exempt(self):
        for term in FORBIDDEN_TERMS:
            for identifier in (term, term.upper()):
                for message in (identifier + b"\n" + APPROVED_LINE, APPROVED_LINE + b"\n" + identifier):
                    with self.subTest(message=message):
                        self.assert_rejected(self.run_check(record(APPROVED_COMMIT, message + b"\n")))

    def test_author_and_committer_names_are_not_exempt(self):
        for offset in (0, 2):
            for name in (FORBIDDEN_TERMS[0].upper(), APPROVED_LINE):
                identities = [b"Author", b"a@example.invalid", b"Committer", b"c@example.invalid"]
                identities[offset] = name
                with self.subTest(offset=offset, name=name):
                    self.assert_rejected(self.run_check(record(APPROVED_COMMIT, APPROVED_LINE, identities)))

    def test_author_and_committer_emails_are_not_exempt(self):
        for offset in (1, 3):
            for term in FORBIDDEN_TERMS:
                identities = [b"Author", b"a@example.invalid", b"Committer", b"c@example.invalid"]
                identities[offset] = term.upper() + b"@example.invalid"
                with self.subTest(offset=offset, term=term):
                    self.assert_rejected(self.run_check(record(APPROVED_COMMIT, APPROVED_LINE, identities)))

    def test_entire_large_history_is_consumed_and_scanned_after_first_violation(self):
        first_commit, last_commit = b"2" * 40, b"3" * 40
        history = (record(first_commit, FORBIDDEN_TERMS[0])
                   + record(APPROVED_COMMIT, APPROVED_LINE)
                   + record(message=b"Public details\n" * 300000)
                   + record(last_commit, FORBIDDEN_TERMS[-1]))
        result = self.run_check(history)
        self.assert_rejected(result)
        self.assertEqual((self.root / "completed").read_text(), "yes")
        self.assertIn(first_commit.decode(), result.stderr)
        self.assertIn(last_commit.decode(), result.stderr)

    def test_late_violation_after_clean_and_approved_records_is_rejected(self):
        history = record() + record(APPROVED_COMMIT, APPROVED_LINE) + record(b"3" * 40, FORBIDDEN_TERMS[1])
        self.assert_rejected(self.run_check(history))

    def test_git_command_failure_with_valid_output_is_rejected(self):
        for status in (1, 128, 141):
            with self.subTest(status=status):
                result = self.run_check(record(APPROVED_COMMIT, APPROVED_LINE), status)
                self.assert_rejected(result)
                self.assertIn("could not validate", result.stderr)

    def test_missing_git_is_rejected(self):
        self.fake_git.unlink()
        result = self.run_check(record())
        self.assert_rejected(result)
        self.assertIn("could not validate", result.stderr)

    def test_malformed_or_incomplete_git_output_is_rejected(self):
        for history in (b"", record()[:-1], record() + b"trailing data",
                        record(b"not-a-commit"), record(message=b"unexpected\0field"),
                        record() + record(b"invalid")):
            with self.subTest(history=history):
                result = self.run_check(history)
                self.assert_rejected(result)
                self.assertIn("could not validate", result.stderr)

    def test_real_git_rejects_same_path_on_new_commit_in_unmerged_ref(self):
        environment = dict(os.environ, GIT_CONFIG_NOSYSTEM="1", GIT_CONFIG_GLOBAL=os.devnull)

        def git(*arguments):
            return subprocess.run(
                ["git", "-c", "core.hooksPath=" + os.devnull, "-c", "commit.gpgsign=false",
                 "-c", "user.name=Fixture", "-c", "user.email=fixture@example.invalid", *arguments],
                cwd=self.root, env=environment, capture_output=True, text=True, check=True, timeout=10,
            ).stdout.strip()

        git("init", "--initial-branch=main")
        git("commit", "--allow-empty", "-m", "Public fixture")
        clean = subprocess.run([sys.executable, str(CHECKER)], cwd=self.root, env=environment,
                               capture_output=True, text=True, timeout=10)
        self.assertEqual(clean.returncode, 0, clean.stdout + clean.stderr)
        git("checkout", "-b", "unmerged")
        git("commit", "--allow-empty", "-m", APPROVED_LINE.decode())
        unmerged_commit = git("rev-parse", "HEAD")
        self.assertNotEqual(unmerged_commit, APPROVED_COMMIT.decode())
        git("checkout", "main")
        result = subprocess.run([sys.executable, str(CHECKER)], cwd=self.root, env=environment,
                                capture_output=True, text=True, timeout=10)
        self.assert_rejected(result)
        self.assertIn(unmerged_commit, result.stderr)


if __name__ == "__main__":
    unittest.main()
