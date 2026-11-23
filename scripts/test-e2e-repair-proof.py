#!/usr/bin/env python3
"""The end-to-end suites call only commands that cxt has.

The CI workflow runs this file by name. The helper it once tested proved what
`cxt repair --from-server` restored; that command was removed with capture. A
suite that still called a removed command would fail in a way that says little,
so this reads the command table of the CLI and checks every `cxt <command>` the
suites run against it.
"""
from pathlib import Path
import re
import unittest

ROOT = Path(__file__).resolve().parent.parent
ARGS = (ROOT / "cli/internal/adapters/delivery/cli/args.go").read_text()
ROOT_GO = (ROOT / "cli/internal/adapters/delivery/cli/root.go").read_text()


def commands():
    specs = ARGS.split("var commandArgSpecs = map[string]commandArgSpec{", 1)[1].split("\n}\n", 1)[0]
    names = set(re.findall(r'^\t"([a-z-]+)":', specs, flags=re.M))
    verbs = ROOT_GO.split("var segmentVerbs = map[string]bool{", 1)[1].split("}", 1)[0]
    names |= set(re.findall(r'"([a-z]+)": true', verbs))
    # What a hook script runs, and help.
    return names | {"git-hook", "help"}


def invoked(text):
    found = set()
    for line in text.splitlines():
        if line.lstrip().startswith("#"):
            continue
        # A command position: the start of a line, or after a shell separator.
        for match in re.finditer(r'(?:^|[;&|(]|\$\(|\bthen |\bdo |=1 |>/dev/null 2>&1; )\s*cxt ([a-z][a-z-]*)\b', line):
            found.add(match.group(1))
    return found


class SuiteCommandsTest(unittest.TestCase):
    def test_the_command_table_is_read(self):
        names = commands()
        self.assertTrue({"setup", "push", "doctor", "segments", "hooks", "config", "git-hook"} <= names, names)
        self.assertFalse({"repair", "save", "checkout", "pull"} & names, names)

    def test_the_matcher_finds_commands_and_skips_prose(self):
        text = 'cxt setup "$REMOTE" --no-login >"$TMP/out" 2>&1\nexpect "x" "$(cxt doctor --json | python3 -c 1)" 0\n# cxt save is gone\necho "run cxt repair later"\n'
        self.assertEqual(invoked(text), {"setup", "doctor"})

    def test_suites_call_only_commands_that_exist(self):
        known = commands()
        for name in ("e2e.sh", "e2e-sync.sh"):
            text = (ROOT / "scripts" / name).read_text()
            # The suite runs the removed commands once, by a loop variable, to show that they are gone.
            self.assertEqual(invoked(text) - known, set(), name)

    def test_the_removed_commands_are_checked_to_be_unknown(self):
        suite = (ROOT / "scripts/e2e-sync.sh").read_text()
        removed = suite.split("for removed in ", 1)[1].split(";", 1)[0].split()
        self.assertIn("repair", removed)
        self.assertEqual(set(removed) & commands(), set())


if __name__ == "__main__":
    unittest.main()
