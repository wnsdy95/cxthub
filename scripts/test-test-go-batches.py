#!/usr/bin/env python3
from collections import Counter
import importlib.util
import json
import os
from pathlib import Path
import re
import selectors
import signal
import subprocess
import sys
import tempfile
import unittest
from unittest import mock

sys.dont_write_bytecode = True
RUNNER = Path(__file__).with_name("test-go-batches.py")
SPEC = importlib.util.spec_from_file_location("go_batches", RUNNER)
batches = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(batches)

FAKE_BINARY = '''#!/usr/bin/env python3
import json
import os
from pathlib import Path
import re
import signal
import subprocess
import sys
root = Path(os.environ["GO_BATCH_FIXTURE"])
config = json.loads((root / "config.json").read_text())
args = sys.argv[1:]
listing = next((arg.split("=", 1)[1] for arg in args if arg.startswith("-test.list=")), None)
if listing is not None:
    print("\\n".join(name for name in config["names"] if re.search(listing, name)))
    sys.exit(config.get("discovery_status", 0))
pattern = next(arg.split("=", 1)[1] for arg in args if arg.startswith("-test.run="))
count = int(next(arg.split("=", 1)[1] for arg in args if arg.startswith("-test.count=")))
selected = [name for name in config["names"] if re.search(pattern, name)]
with (root / "batches.jsonl").open("a") as output:
    output.write(json.dumps({"tests": selected, "count": count, "cwd": os.getcwd(), "args": args}) + "\\n")
def frame(line):
    print("\\x16" + line, flush=True)
if config.get("cancel"):
    frame("=== RUN   " + selected[0])
    (root / "child.pid").write_text(str(os.getpid()))
    if config.get("descendant"):
        child = subprocess.Popen([sys.executable, "-c", "import signal; signal.signal(signal.SIGTERM, signal.SIG_IGN); print('ready', flush=True); signal.pause()"], stdout=subprocess.PIPE, stderr=subprocess.DEVNULL, text=True)
        assert child.stdout.readline().strip() == "ready"
        (root / "descendant.pid").write_text(str(child.pid))
    print("fixture-batch-ready", flush=True)
    signal.pause()
for repetition in range(config.get("actual_count", count)):
    for name in selected:
        if name in config.get("omit", []):
            continue
        frame("=== RUN   " + name)
        if config.get("spoof_output"):
            print("=== RUN   TestUnknown\\n--- PASS: TestUnknown (0.00s)", flush=True)
        if config.get("subtest_outcome"):
            frame("=== RUN   " + name + "/detail")
            frame("=== PAUSE " + name + "/detail")
            frame("=== CONT  " + name + "/detail")
            frame("--- " + config["subtest_outcome"].upper() + ": " + name + "/detail (0.00s)")
        if not config.get("unfinished"):
            outcome = config.get("outcomes", {}).get(name, config.get("outcome", "pass"))
            if isinstance(outcome, list):
                outcome = outcome[repetition]
            frame("--- " + outcome.upper() + ": " + name + " (0.00s)")
frame("FAIL" if config.get("batch_status", 0) else "PASS")
sys.exit(config.get("batch_status", 0))
'''

FAKE_GO = '''#!/usr/bin/env python3
import json
import os
from pathlib import Path
import re
import signal
import subprocess
import sys
root = Path(os.environ["GO_BATCH_FIXTURE"])
config = json.loads((root / "config.json").read_text())
args = sys.argv[1:]
with (root / "go.jsonl").open("a") as output:
    output.write(json.dumps(args) + "\\n")
if args[0] == "list":
    print(root / "package")
    sys.exit(config.get("list_status", 0))
if args[:2] == ["tool", "test2json"]:
    assert args[2:4] == ["-t", "-p"]
    package = args[4]
    command = args[5:]
    assert "-test.v=test2json" in command
    def emit(action, name="", output=None):
        event = {"Time": "2006-01-02T15:04:05Z", "Action": action, "Package": package}
        if name:
            event["Test"] = name
        if action in ("pass", "fail", "skip"):
            event["Elapsed"] = 0.0
        if output is not None:
            event["Output"] = output
        print(json.dumps(event), flush=True)
    emit("start")
    child = subprocess.Popen(command, stdout=subprocess.PIPE, stderr=subprocess.STDOUT, text=True)
    (root / "converter.pid").write_text(str(os.getpid()))
    signal.signal(signal.SIGINT, signal.SIG_IGN)
    signal.signal(signal.SIGTERM, signal.SIG_IGN)
    current = ""
    for line in child.stdout:
        framed = line.startswith("\\x16")
        line = line.removeprefix("\\x16")
        match = re.fullmatch(r"(?:=== (RUN|PAUSE|CONT) +|--- (PASS|FAIL|SKIP): )(.+?)(?: \\(.*s\\))?\\n", line) if framed else None
        if match:
            action = (match[1] or match[2]).lower()
            current = match[3]
            if action in ("run", "cont"):
                emit(action, current)
            emit("output", current, line)
            if action not in ("run", "cont"):
                emit(action, current)
        else:
            if framed and line in ("PASS\\n", "FAIL\\n"):
                current = ""
            emit("output", current, line)
    status = child.wait()
    emit("fail" if status else "pass")
    sys.exit(1 if status else 0)
if args[0] != "test" or "-c" not in args:
    sys.exit(91)
if config.get("build_status", 0):
    sys.exit(config["build_status"])
binary = Path(args[args.index("-o") + 1])
binary.write_text((root / "binary.py").read_text())
binary.chmod(0o700)
'''


class GoBatchesTest(unittest.TestCase):
    def setUp(self):
        temporary = tempfile.TemporaryDirectory(prefix="go-batches-unit-")
        self.addCleanup(temporary.cleanup)
        self.root = Path(temporary.name).resolve()
        (self.root / "package").mkdir()
        tools = self.root / "bin"
        tools.mkdir()
        executable = tools / "go"
        executable.write_text(FAKE_GO)
        executable.chmod(0o700)
        (self.root / "binary.py").write_text(FAKE_BINARY)
        self.env = dict(os.environ, GO_BATCH_FIXTURE=str(self.root), PATH=str(tools) + os.pathsep + os.environ["PATH"])

    def configure(self, **changes):
        config = {"names": [f"TestPGCase{number:03d}" for number in range(12, 0, -1)]}
        config.update(changes)
        (self.root / "config.json").write_text(json.dumps(config))
        return config

    def command(self):
        return [sys.executable, str(RUNNER), "./package", "--tags", "postgres", "--race", "--run", "^TestPG", "--count", "3"]

    def read_log(self, name):
        path = self.root / name
        return [json.loads(line) for line in path.read_text().splitlines()] if path.exists() else []

    def run_fixture(self, *arguments):
        return subprocess.run(self.command() + list(arguments), cwd=self.root, env=self.env, text=True, capture_output=True, timeout=10)

    def test_compiles_once_batches_all_175_tests_exactly_once_per_repetition(self):
        config = self.configure(names=[f"TestPGCase{number:03d}" for number in range(175, 0, -1)])
        result = self.run_fixture()
        self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
        calls = self.read_log("go.jsonl")
        self.assertEqual(len(calls), 37)
        self.assertEqual(calls[0], ["list", "-tags", "postgres", "-f", "{{.Dir}}", "./package"])
        self.assertEqual(calls[1][:5], ["test", "-c", "-tags", "postgres", "-race"])
        self.assertEqual(sum(call[:2] == ["test", "-c"] for call in calls), 1)
        for call in calls[2:]:
            self.assertEqual(call[:5], ["tool", "test2json", "-t", "-p", "./package"])
        runs = self.read_log("batches.jsonl")
        self.assertEqual(len(runs), 35)
        expected = sorted(config["names"])
        for number, run in enumerate(runs):
            self.assertEqual(sorted(run["tests"]), expected[number * 5:(number + 1) * 5])
            self.assertEqual(run["count"], 3)
            self.assertEqual(run["cwd"], str(self.root / "package"))
            self.assertIn("-test.timeout=120s", run["args"])
            self.assertIn("-test.parallel=1", run["args"])
            self.assertIn("-test.v=test2json", run["args"])
            pattern = next(arg.removeprefix("-test.run=") for arg in run["args"] if arg.startswith("-test.run="))
            self.assertTrue(pattern.startswith("^(") and pattern.endswith(")$"))
            self.assertIsNone(re.search(pattern, run["tests"][0] + "Suffix"))
        for repetition in range(3):
            actual = Counter(name for run in runs if repetition < run["count"] for name in run["tests"])
            self.assertEqual(actual, Counter(config["names"]))
        self.assertIn("expected=525 run=525 pass=525 fail=0 skip=0 missing=0 cancelled=0 unfinished=0", result.stdout)
        self.assertIn("passed 525 top-level test runs", result.stdout)

    def test_final_partial_batch_is_not_lost(self):
        self.configure()
        result = self.run_fixture()
        self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
        self.assertEqual([len(run["tests"]) for run in self.read_log("batches.jsonl")], [5, 5, 2])

    def test_batch_size_is_preserved(self):
        self.configure()
        result = subprocess.run(self.command() + ["--batch-size", "2"], cwd=self.root, env=self.env, text=True, capture_output=True, timeout=10)
        self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
        self.assertEqual([len(run["tests"]) for run in self.read_log("batches.jsonl")], [2] * 6)

    def test_nested_skip_and_unframed_output_do_not_inflate_top_level_counts(self):
        self.configure(subtest_outcome="skip", spoof_output=True)
        result = self.run_fixture()
        self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
        self.assertIn("expected=36 run=36 pass=36 fail=0 skip=0 missing=0", result.stdout)

    def test_exact_skip_allowlist_counts_observed_outcomes_across_batches_and_repetitions(self):
        self.configure(outcomes={
            "TestPGCase001": "skip",
            "TestPGCase006": ["skip", "pass", "skip"],
            "TestPGCase012": "skip",
        })
        result = self.run_fixture(
            "--allow-skip", "TestPGCase001", "--allow-skip", "TestPGCase006",
            "--allow-skip", "TestPGCase012",
        )
        self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
        self.assertEqual([len(run["tests"]) for run in self.read_log("batches.jsonl")], [5, 5, 2])
        self.assertIn("selected=12 expected=36 run=36 pass=28 fail=0 skip=8 missing=0 cancelled=0 unfinished=0 allowed_skip=8", result.stdout)
        self.assertIn("passed 28 top-level test runs; allowed skips=8", result.stdout)

    def test_all_allowed_skips_do_not_count_as_passes(self):
        self.configure(names=["TestPGOne", "TestPGTwo"], outcome="skip")
        result = self.run_fixture("--allow-skip", "TestPGOne", "--allow-skip", "TestPGTwo")
        self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
        self.assertIn("expected=6 run=6 pass=0 fail=0 skip=6 missing=0 cancelled=0 unfinished=0 allowed_skip=6", result.stdout)
        self.assertIn("passed 0 top-level test runs; allowed skips=6", result.stdout)

    def test_allowlisted_test_can_pass_without_inventing_skips(self):
        self.configure(names=["TestPGOne"])
        result = self.run_fixture("--allow-skip", "TestPGOne")
        self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
        self.assertIn("expected=3 run=3 pass=3 fail=0 skip=0 missing=0 cancelled=0 unfinished=0 allowed_skip=0", result.stdout)
        self.assertIn("passed 3 top-level test runs; allowed skips=0", result.stdout)

    def test_allowlist_does_not_allow_other_selected_tests_to_skip(self):
        self.configure(names=["TestPGOne", "TestPGTwo"], outcome="skip")
        result = self.run_fixture("--allow-skip", "TestPGOne")
        self.assertNotEqual(result.returncode, 0, result.stdout + result.stderr)
        self.assertIn("unexpected top-level test skip: TestPGTwo", result.stderr)
        self.assertIn("pass=0 fail=0 skip=6 missing=0 cancelled=0 unfinished=0 allowed_skip=3", result.stdout)
        self.assertNotIn("[go-batches] passed", result.stdout)

    def test_duplicate_unknown_pattern_and_out_of_selection_allowlist_names_are_rejected(self):
        cases = (
            (["TestPGOne", "TestPGOne"], "duplicate --allow-skip"),
            (["TestPGUnknown"], "not in discovered selection"),
            (["TestOutsideSelection"], "not in discovered selection"),
            (["TestPG.*"], "not in discovered selection"),
            (["^TestPGOne$"], "not in discovered selection"),
            (["TestPGOne/detail"], "not in discovered selection"),
            (["TestPGOneSuffix"], "not in discovered selection"),
            ([""], "not in discovered selection"),
        )
        self.configure(names=["TestPGOne", "TestOutsideSelection"])
        for names, message in cases:
            with self.subTest(names=names):
                arguments = [argument for name in names for argument in ("--allow-skip", name)]
                result = self.run_fixture(*arguments)
                self.assertNotEqual(result.returncode, 0, result.stdout + result.stderr)
                self.assertIn(message, result.stderr)
                self.assertEqual(self.read_log("batches.jsonl"), [])
                self.assertNotIn("[go-batches] passed", result.stdout)

    def test_allowed_skip_still_requires_complete_counts_and_successful_children(self):
        cases = (
            ({"actual_count": 2}, "missing=1", "missing test2json outcomes"),
            ({"omit": ["TestPGOne"]}, "missing=3", "missing test2json outcomes"),
            ({"unfinished": True, "actual_count": 1}, "unfinished=1", "unfinished test2json tests"),
            ({"outcome": "fail"}, "fail=3", "reported failed tests"),
            ({"subtest_outcome": "fail"}, "skip=3", "reported failed tests"),
            ({"batch_status": 7}, "skip=3", "failed:"),
        )
        for changes, summary, message in cases:
            with self.subTest(changes=changes):
                (self.root / "batches.jsonl").unlink(missing_ok=True)
                self.configure(names=["TestPGOne"], **{"outcome": "skip", **changes})
                result = self.run_fixture("--allow-skip", "TestPGOne")
                self.assertNotEqual(result.returncode, 0, result.stdout + result.stderr)
                self.assertIn(summary, result.stdout)
                self.assertIn(message, result.stderr)
                self.assertNotIn("[go-batches] passed", result.stdout)
                self.assertEqual(len(self.read_log("batches.jsonl")), 1)

    def test_zero_exit_does_not_hide_skip_fail_missing_or_unfinished_runs(self):
        cases = (
            ({"outcome": "skip"}, "skip=15", "unexpected top-level test skip"),
            ({"outcome": "fail"}, "fail=15", "reported failed tests"),
            ({"actual_count": 2}, "missing=26", "missing test2json outcomes"),
            ({"omit": ["TestPGCase001"]}, "missing=24", "missing test2json outcomes"),
            ({"omit": [f"TestPGCase{number:03d}" for number in range(1, 6)]}, "pass=0", "missing test2json outcomes"),
            ({"unfinished": True, "actual_count": 1}, "unfinished=5", "unfinished test2json tests"),
        )
        for changes, summary, message in cases:
            with self.subTest(changes=changes):
                (self.root / "batches.jsonl").unlink(missing_ok=True)
                self.configure(**changes)
                result = self.run_fixture()
                self.assertNotEqual(result.returncode, 0, result.stdout + result.stderr)
                self.assertIn(summary, result.stdout)
                self.assertIn(message, result.stderr)
                self.assertNotIn("[go-batches] passed", result.stdout)
                self.assertEqual(len(self.read_log("batches.jsonl")), 1)

    def account(self, events, count=1, allow_skip=()):
        result = batches.BatchResults(["TestPGOne"], count, "./package", allow_skip)
        for event in events:
            result.feed(json.dumps({"Package": "./package", **event}) + "\n")
        result.verify()
        return result

    def test_repeated_lifecycle_counts_only_completed_top_level_runs(self):
        events = [{"Action": "start"}]
        for repetition in range(3):
            events.extend([
                {"Action": "run", "Test": "TestPGOne"},
                {"Action": "pause", "Test": "TestPGOne"},
                {"Action": "cont", "Test": "TestPGOne"},
                {"Action": "pass", "Test": "TestPGOne"},
            ])
        events.append({"Action": "pass"})
        result = self.account(events, count=3)
        self.assertEqual(result.outcomes, {"pass": 3})
        self.assertEqual(result.started, {"TestPGOne": 3})

    def test_allowed_skip_requires_valid_run_and_terminal_lifecycle(self):
        start = {"Action": "start"}
        run = {"Action": "run", "Test": "TestPGOne"}
        skipped = {"Action": "skip", "Test": "TestPGOne"}
        package_pass = {"Action": "pass"}
        result = self.account([start, run, skipped, package_pass], allow_skip=["TestPGOne"])
        self.assertEqual(result.started, {"TestPGOne": 1})
        self.assertEqual(result.completed, {"TestPGOne": 1})
        self.assertEqual(result.outcomes, {"skip": 1})
        self.assertEqual(result.skipped, {"TestPGOne": 1})
        cases = (
            [start, skipped, package_pass],
            [start, run, skipped, skipped, package_pass],
            [start, run, run, skipped, package_pass],
            [start, run, skipped, run, skipped, package_pass],
            [start, run, {"Action": "pause", "Test": "TestPGOne"}, skipped, package_pass],
            [start, run, {"Action": "run", "Test": "TestPGOne/detail"}, skipped, package_pass],
            [start, run, skipped, {"Action": "skip"}],
        )
        for events in cases:
            with self.subTest(events=events), self.assertRaises(ValueError):
                self.account(events, allow_skip=["TestPGOne"])

    def test_malformed_unknown_and_inconsistent_events_are_rejected(self):
        start = {"Action": "start"}
        run = {"Action": "run", "Test": "TestPGOne"}
        passed = {"Action": "pass", "Test": "TestPGOne"}
        package_pass = {"Action": "pass"}
        cases = (
            [run, passed, package_pass],
            [start, start],
            [start, {"Action": "run", "Test": "TestUnknown"}],
            [start, {"Action": "run", "Test": "TestUnknown/subtest"}],
            [start, {"Action": "run", "Test": "TestPGOne/subtest"}],
            [start, passed],
            [start, run, run],
            [start, run, passed, passed],
            [start, run, passed, run],
            [start, run, {"Action": "cont", "Test": "TestPGOne"}],
            [start, run, {"Action": "pause", "Test": "TestPGOne"}, passed],
            [start, run, {"Action": "cancel", "Test": "TestPGOne"}],
            [start, run, {"Action": "pass", "Test": "TestPGOne", "Elapsed": "0"}],
            [start, run, {"Action": "run", "Test": "TestPGOne/subtest"}, passed],
            [start, run, passed],
            [start, run, passed, {"Action": "fail"}],
            [start, run, passed, {"Action": "skip"}],
            [start, run, passed, package_pass, package_pass],
            [start, {"Action": "output"}],
            [start, {"Action": "run", "Test": 1}],
            [start, {"Action": "pass", "Package": "other"}],
        )
        for events in cases:
            with self.subTest(events=events), self.assertRaises(ValueError):
                self.account(events)
        for line in ("not json\n", "[]\n", "null\n", '{"Action":"start","Package":"./package"}', '{"Action":"start","Action":"pass"}\n'):
            with self.subTest(line=line), self.assertRaises(ValueError):
                batches.BatchResults(["TestPGOne"], 1, "./package").feed(line)

    def test_empty_and_duplicate_discovery_fail_without_running_batches(self):
        for names, message in (([], "no tests"), (["TestPGOne", "TestPGOne"], "duplicate")):
            with self.subTest(names=names):
                self.configure(names=names)
                result = self.run_fixture()
                self.assertNotEqual(result.returncode, 0)
                self.assertIn(message, result.stderr)
                self.assertEqual(self.read_log("batches.jsonl"), [])

    def test_discovery_rejects_unexpected_output(self):
        for output in ("TestPGGood\nunexpected diagnostic\n", "BenchmarkPGCase\n", "TestPGCase/subtest\n"):
            with self.subTest(output=output), self.assertRaises(ValueError):
                batches.discover_tests(output)

    def test_nonzero_children_stop_before_successor_batches(self):
        for stage in ("list_status", "build_status", "discovery_status", "batch_status"):
            with self.subTest(stage=stage):
                for name in ("go.jsonl", "batches.jsonl"):
                    (self.root / name).unlink(missing_ok=True)
                self.configure(**{stage: 7})
                result = self.run_fixture()
                self.assertNotEqual(result.returncode, 0)
                self.assertIn("failed:", result.stderr)
                self.assertNotIn("[go-batches] passed", result.stdout)
                self.assertEqual(len(self.read_log("batches.jsonl")), 1 if stage == "batch_status" else 0)

    def test_invalid_bounds_fail_before_go(self):
        for option, value in (("--count", "0"), ("--count", "-1"), ("--batch-size", "0"), ("--batch-size", "6")):
            with self.subTest(option=option, value=value):
                result = subprocess.run(self.command() + [option, value], cwd=self.root, env=self.env, text=True, capture_output=True, timeout=5)
                self.assertEqual(result.returncode, 2)
                self.assertEqual(self.read_log("go.jsonl"), [])

    def test_cancel_fails_and_reaps_active_batch(self):
        for signum, descendant in ((signal.SIGINT, False), (signal.SIGTERM, False), (signal.SIGINT, True), (signal.SIGTERM, True)):
            with self.subTest(signal=signum, descendant=descendant):
                (self.root / "batches.jsonl").unlink(missing_ok=True)
                self.configure(cancel=True, descendant=descendant)
                process = subprocess.Popen(self.command(), cwd=self.root, env=self.env, stdout=subprocess.PIPE, stderr=subprocess.STDOUT)
                child = None
                try:
                    output = b""
                    with selectors.DefaultSelector() as selector:
                        selector.register(process.stdout, selectors.EVENT_READ)
                        while b"fixture-batch-ready\n" not in output:
                            self.assertTrue(selector.select(timeout=5), output.decode())
                            chunk = os.read(process.stdout.fileno(), 4096)
                            self.assertTrue(chunk, output.decode())
                            output += chunk
                    child = int((self.root / "child.pid").read_text())
                    converter = int((self.root / "converter.pid").read_text())
                    if descendant:
                        descendant_pid = int((self.root / "descendant.pid").read_text())
                        self.assertEqual(os.getpgid(descendant_pid), converter)
                    process.send_signal(signum)
                    remainder, _ = process.communicate(timeout=8)
                    self.assertEqual(process.returncode, 128 + signum, (output + remainder).decode())
                    with self.assertRaises(ProcessLookupError):
                        os.kill(child, 0)
                    with self.assertRaises(ProcessLookupError):
                        os.kill(converter, 0)
                    self.assertIn(b"cancelled=1", output + remainder)
                    self.assertNotIn(b"[go-batches] passed", output + remainder)
                    self.assertEqual(len(self.read_log("batches.jsonl")), 1)
                finally:
                    if child is not None:
                        try:
                            converter = int((self.root / "converter.pid").read_text())
                            os.killpg(converter, signal.SIGKILL)
                        except ProcessLookupError:
                            pass
                    if process.poll() is None:
                        process.kill()
                    process.communicate(timeout=5)

    def test_cancel_kills_a_child_that_ignores_termination(self):
        process = mock.Mock(pid=123)
        process.communicate.side_effect = [batches.Cancelled(signal.SIGTERM), subprocess.TimeoutExpired("child", 5), (None, None)]
        with mock.patch.object(batches.subprocess, "Popen", return_value=process), mock.patch.object(batches.os, "killpg") as kill:
            with self.assertRaises(batches.Cancelled):
                batches.run_command(["synthetic-child"], self.root)
        self.assertEqual(kill.call_args_list, [mock.call(123, signal.SIGTERM), mock.call(123, signal.SIGKILL)])

    def test_completed_command_still_cleans_up_descendants(self):
        process = mock.Mock(pid=123, returncode=0, stdout=None)
        process.communicate.return_value = (None, None)
        with mock.patch.object(batches.subprocess, "Popen", return_value=process), mock.patch.object(batches.os, "killpg") as kill:
            batches.run_command(["synthetic-child"], self.root)
        kill.assert_called_once_with(123, signal.SIGKILL)


if __name__ == "__main__":
    unittest.main()
