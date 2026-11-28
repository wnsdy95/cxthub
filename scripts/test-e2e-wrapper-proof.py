#!/usr/bin/env python3
"""No script and no document starts an agent tool through cxt.

The CI workflow runs this file by name. The helper it once tested proved what
the wrappers `cxt claude` and `cxt codex` did to a session; the wrappers were
removed with capture. What remains worth checking is that nothing still tells
anyone, or any script, to use them.
"""
from pathlib import Path
import re
import unittest

ROOT = Path(__file__).resolve().parent.parent
WRAPPER = re.compile(r"\bcxt (claude|codex)\b(?! <session>| [a-z0-9-]*<)")
# A session is named by its tool: `cxt push claude <session>` is not a wrapper.
VERBS = ("push", "log", "remove", "allow", "resend", "archive", "unarchive", "fork")


def wrapper_calls(text):
    found = []
    for number, line in enumerate(text.splitlines(), 1):
        for match in re.finditer(r"\bcxt (claude|codex)\b", line):
            before = line[:match.start()]
            if any(before.rstrip().endswith("cxt segments " + verb) or before.rstrip().endswith("cxt " + verb) for verb in VERBS):
                continue
            found.append((number, line.strip()))
    return found


class NoWrapperTest(unittest.TestCase):
    def test_the_matcher_tells_a_wrapper_from_a_session_name(self):
        self.assertEqual(wrapper_calls("run cxt claude --resume x\n"), [(1, "run cxt claude --resume x")])
        self.assertEqual(wrapper_calls("cxt codex\n"), [(1, "cxt codex")])
        self.assertEqual(wrapper_calls("cxt push claude 1234\ncxt segments fork codex abc --at d\n"), [])

    def test_scripts_do_not_start_a_tool_through_cxt(self):
        for path in sorted((ROOT / "scripts").iterdir()):
            if path.suffix not in (".sh", ".py") or path.name == Path(__file__).name:
                continue
            calls = wrapper_calls(path.read_text())
            # The end-to-end suite names the removed commands to show that they are gone.
            calls = [call for call in calls if "for removed in" not in call[1]]
            self.assertEqual(calls, [], path.name)

    def test_the_cli_reference_names_the_wrappers_only_as_removed(self):
        reference = (ROOT / "docs/CLI.md").read_text()
        calls = wrapper_calls(reference)
        self.assertTrue(calls, "the reference says the wrappers are gone")
        section = reference.split("## Coming from a version that captured", 1)[1].split("\n## ", 1)[0]
        for _, line in calls:
            self.assertIn(line, section)

    def test_the_cli_has_no_wrapper_command(self):
        table = (ROOT / "cli/internal/adapters/delivery/cli/args.go").read_text()
        specs = table.split("var commandArgSpecs = map[string]commandArgSpec{", 1)[1].split("\n}\n", 1)[0]
        names = set(re.findall(r'^\t"([a-z-]+)":', specs, flags=re.M))
        self.assertFalse(names & {"claude", "codex", "mcp"}, names)
        self.assertTrue({"setup", "segments", "hook", "hooks"} <= names, names)


if __name__ == "__main__":
    unittest.main()
