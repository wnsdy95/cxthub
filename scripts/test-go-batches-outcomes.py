#!/usr/bin/env python3
"""Black-box outcome checks using only Python's standard library and fake Go tools."""

import json
import os
from pathlib import Path
import subprocess
import sys
import tempfile
import unittest


RUNNER = Path(__file__).resolve().with_name("test-go-batches.py")
INVENTORY = ("TestOutcomeGamma", "TestOutcomeBeta", "TestOutcomeAlpha")

FAKE_BINARY = r'''
import json
import os
from pathlib import Path
import re
import sys

root = Path(os.environ["GO_BATCH_OUTCOME_FIXTURE"])
config = json.loads((root / "config.json").read_text())
arguments = sys.argv[1:]
listing = next((argument.split("=", 1)[1] for argument in arguments if argument.startswith("-test.list=")), None)
if listing is not None:
    print("\n".join(name for name in config["names"] if re.search(listing, name)))
    sys.exit(0)

pattern = next(argument.split("=", 1)[1] for argument in arguments if argument.startswith("-test.run="))
count = int(next(argument.split("=", 1)[1] for argument in arguments if argument.startswith("-test.count=")))
selected = [name for name in sorted(config["names"]) if re.search(pattern, name)]
with (root / "batches.jsonl").open("a") as output:
    output.write(json.dumps({"tests": selected, "count": count, "cwd": os.getcwd(), "arguments": arguments}) + "\n")

scenario = config["scenario"]
if scenario == "exit_zero_without_output":
    sys.exit(0)
if scenario == "package_pass_without_tests":
    print("PASS")
    sys.exit(0)
if scenario == "missing_selected_test":
    selected = selected[:-1]

repetitions = 1 if scenario == "count_two_only_one_run" else count
executions = [(repetition, name) for repetition in range(repetitions) for name in selected]
if scenario == "uneven_repetitions" and len(selected) > 1:
    executions.remove((repetitions - 1, selected[-1]))
    executions.append((repetitions, selected[0]))

for repetition, name in executions:
    faulty = repetition == repetitions - 1 and name == selected[-1]
    if faulty and scenario == "terminal_without_run":
        print(f"--- PASS: {name} (0.00s)")
        continue
    print(f"=== RUN   {name}")
    if faulty and scenario == "overlapping_run":
        print(f"=== RUN   {name}")
    child = name + "/child"
    grandchild = child + "/grandchild"
    print(f"=== RUN   {child}")
    print(f"=== RUN   {grandchild}")
    print(f"        --- PASS: {grandchild} (0.00s)")
    if not (faulty and scenario == "child_without_completion"):
        outcome = "FAIL" if faulty and scenario == "child_fail_parent_pass" else "PASS"
        print(f"    --- {outcome}: {child} (0.00s)")
    sibling = name + "/sibling"
    print(f"=== RUN   {sibling}")
    print(f"    --- PASS: {sibling} (0.00s)")
    if not (faulty and scenario == "top_level_without_completion"):
        print(f"--- PASS: {name} (0.00s)")
    if faulty and scenario == "duplicate_terminal":
        print(f"--- PASS: {name} (0.00s)")

if scenario in ("unknown_top_level", "unexpected_skip"):
    print("=== RUN   TestOutsideInventory")
    outcome = "SKIP" if scenario == "unexpected_skip" else "PASS"
    print(f"--- {outcome}: TestOutsideInventory (0.00s)")
if scenario == "unexpected_skip_without_run":
    print("--- SKIP: TestOutsideInventory (0.00s)")
print("PASS")
'''

FAKE_GO = r'''
import json
import os
from pathlib import Path
import re
import subprocess
import sys

root = Path(os.environ["GO_BATCH_OUTCOME_FIXTURE"])
config = json.loads((root / "config.json").read_text())
arguments = sys.argv[1:]
with (root / "go.jsonl").open("a") as output:
    output.write(json.dumps(arguments) + "\n")

if arguments and arguments[0] == "list":
    print(root / "package")
    sys.exit(0)
if arguments and arguments[0] == "test" and "-c" in arguments:
    binary = Path(arguments[arguments.index("-o") + 1])
    binary.write_text((root / "binary.py").read_text())
    binary.chmod(0o700)
    sys.exit(0)
if arguments[:2] != ["tool", "test2json"]:
    sys.exit("unsupported fake Go command: " + repr(arguments))

remaining = arguments[2:]
package = "./package"
offset = 0
while offset < len(remaining):
    argument = remaining[offset]
    if argument == "-p":
        package = remaining[offset + 1]
        offset += 2
    elif argument.startswith("-p="):
        package = argument.split("=", 1)[1]
        offset += 1
    elif argument == "-t":
        offset += 1
    elif argument == "--":
        offset += 1
        break
    else:
        break

if remaining[offset:]:
    result = subprocess.run(remaining[offset:], text=True, capture_output=True)
    transcript = result.stdout
    sys.stderr.write(result.stderr)
    status = result.returncode
else:
    transcript = sys.stdin.read()
    status = 0

def emit(action, test=None, output=None):
    event = {"Time": "2026-10-25T00:00:00Z", "Action": action, "Package": package}
    if test is not None:
        event["Test"] = test
    if output is not None:
        event["Output"] = output
    if action in ("pass", "fail", "skip"):
        event["Elapsed"] = 0.0
    print(json.dumps(event), flush=True)

emit("start")
if config["scenario"] == "malformed_json":
    print("{not-json}", flush=True)
for line in transcript.splitlines():
    started = re.fullmatch(r"=== RUN\s+(\S+)", line)
    completed = re.fullmatch(r"\s*--- (PASS|FAIL|SKIP): (\S+) \([0-9.]+s\)", line)
    if started is not None:
        emit("run", test=started.group(1))
        emit("output", test=started.group(1), output=line + "\n")
    elif completed is not None:
        emit("output", test=completed.group(2), output=line + "\n")
        emit(completed.group(1).lower(), test=completed.group(2))
    else:
        emit("output", output=line + "\n")
emit("pass" if status == 0 else "fail")
if config["scenario"] == "truncated_json":
    sys.stdout.write('{"Action":"run","Test":')
    sys.stdout.flush()
sys.exit(status)
'''


class GoBatchOutcomeContractTest(unittest.TestCase):
    def setUp(self):
        temporary = tempfile.TemporaryDirectory(prefix="go-batches-outcomes-")
        self.addCleanup(temporary.cleanup)
        self.root = Path(temporary.name).resolve()
        (self.root / "package").mkdir()
        tools = self.root / "bin"
        tools.mkdir()
        executable = tools / "go"
        executable.write_text(f"#!{sys.executable}\n" + FAKE_GO)
        executable.chmod(0o700)
        (self.root / "binary.py").write_text(f"#!{sys.executable}\n" + FAKE_BINARY)
        self.environment = dict(
            os.environ,
            GO_BATCH_OUTCOME_FIXTURE=str(self.root),
            PATH=str(tools) + os.pathsep + os.environ.get("PATH", ""),
            PYTHONDONTWRITEBYTECODE="1",
        )

    def read_log(self, filename):
        path = self.root / filename
        return [json.loads(line) for line in path.read_text().splitlines()] if path.exists() else []

    def run_scenario(self, scenario):
        for filename in ("go.jsonl", "batches.jsonl"):
            (self.root / filename).unlink(missing_ok=True)
        (self.root / "config.json").write_text(json.dumps({"names": INVENTORY, "scenario": scenario}))
        return subprocess.run(
            [
                sys.executable, "-B", str(RUNNER), "./package",
                "--tags", "postgres", "--race", "--run", "^TestOutcome",
                "--count", "2", "--batch-size", "2",
            ],
            cwd=self.root,
            env=self.environment,
            text=True,
            capture_output=True,
            timeout=15,
        )

    def assert_rejected(self, scenario):
        result = self.run_scenario(scenario)
        diagnostics = result.stdout + result.stderr
        self.assertNotEqual(result.returncode, 0, diagnostics)
        self.assertNotRegex(diagnostics, r"(?m)^\[go-batches\] passed\b", diagnostics)
        runs = self.read_log("batches.jsonl")
        self.assertEqual(len(runs), 1, diagnostics)
        self.assertEqual(runs[0]["tests"], sorted(INVENTORY)[:2], diagnostics)
        self.assertEqual(runs[0]["count"], 2, diagnostics)

    def test_healthy_count_two_with_nested_subtests_passes(self):
        result = self.run_scenario("healthy")
        diagnostics = result.stdout + result.stderr
        self.assertEqual(result.returncode, 0, diagnostics)
        self.assertIn("selected=3 expected=6 run=6 pass=6 fail=0 skip=0 missing=0", result.stdout)
        self.assertIn("passed 6 top-level test runs", result.stdout)
        calls = self.read_log("go.jsonl")
        self.assertEqual(sum(call[0] == "list" for call in calls), 1, diagnostics)
        builds = [call for call in calls if call[0] == "test" and "-c" in call]
        self.assertEqual(len(builds), 1, diagnostics)
        self.assertIn("-race", builds[0])
        self.assertEqual(builds[0][builds[0].index("-tags") + 1], "postgres")
        conversions = [call for call in calls if call[:2] == ["tool", "test2json"]]
        self.assertEqual(len(conversions), 2, diagnostics)
        runs = self.read_log("batches.jsonl")
        self.assertEqual([run["tests"] for run in runs], [sorted(INVENTORY)[:2], sorted(INVENTORY)[2:]])
        for run in runs:
            self.assertEqual(run["count"], 2)
            self.assertEqual(run["cwd"], str(self.root / "package"))
            self.assertIn("-test.timeout=120s", run["arguments"])
            self.assertIn("-test.parallel=1", run["arguments"])

    def test_exit_zero_without_any_test_events_is_rejected(self):
        self.assert_rejected("exit_zero_without_output")

    def test_package_pass_does_not_turn_discovery_into_passes(self):
        self.assert_rejected("package_pass_without_tests")

    def test_missing_selected_test_is_not_discovered_as_passed(self):
        self.assert_rejected("missing_selected_test")

    def test_exit_zero_with_missing_top_level_completion_is_rejected(self):
        self.assert_rejected("top_level_without_completion")

    def test_unexpected_top_level_skip_is_rejected(self):
        for scenario in ("unexpected_skip", "unexpected_skip_without_run"):
            with self.subTest(scenario=scenario):
                self.assert_rejected(scenario)

    def test_unknown_top_level_pass_is_rejected(self):
        self.assert_rejected("unknown_top_level")

    def test_malformed_top_level_lifecycle_is_rejected(self):
        for scenario in ("terminal_without_run", "overlapping_run", "duplicate_terminal"):
            with self.subTest(scenario=scenario):
                self.assert_rejected(scenario)

    def test_parent_pass_does_not_complete_an_unfinished_child(self):
        self.assert_rejected("child_without_completion")

    def test_malformed_json_is_rejected(self):
        self.assert_rejected("malformed_json")

    def test_truncated_json_after_all_passes_is_rejected(self):
        self.assert_rejected("truncated_json")

    def test_count_two_requires_two_observed_runs_per_test(self):
        self.assert_rejected("count_two_only_one_run")

    def test_repetition_totals_cannot_hide_a_missing_run(self):
        self.assert_rejected("uneven_repetitions")

    def test_child_failure_overrides_parent_and_process_success(self):
        self.assert_rejected("child_fail_parent_pass")


if __name__ == "__main__":
    unittest.main()
