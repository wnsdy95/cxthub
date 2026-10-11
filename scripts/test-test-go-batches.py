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
if config.get("cancel"):
    (root / "child.pid").write_text(str(os.getpid()))
    if config.get("descendant"):
        child = subprocess.Popen([sys.executable, "-c", "import signal; signal.signal(signal.SIGTERM, signal.SIG_IGN); print('ready', flush=True); signal.pause()"], stdout=subprocess.PIPE, text=True)
        assert child.stdout.readline().strip() == "ready"
        (root / "descendant.pid").write_text(str(child.pid))
    print("fixture-batch-ready", flush=True)
    signal.pause()
sys.exit(config.get("batch_status", 0))
'''

FAKE_GO = '''#!/usr/bin/env python3
import json
import os
from pathlib import Path
import sys
root = Path(os.environ["GO_BATCH_FIXTURE"])
config = json.loads((root / "config.json").read_text())
args = sys.argv[1:]
with (root / "go.jsonl").open("a") as output:
    output.write(json.dumps(args) + "\\n")
if args[0] == "list":
    print(root / "package")
    sys.exit(config.get("list_status", 0))
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

    def run_fixture(self):
        return subprocess.run(self.command(), cwd=self.root, env=self.env, text=True, capture_output=True, timeout=10)

    def test_compiles_once_batches_all_175_tests_exactly_once_per_repetition(self):
        config = self.configure(names=[f"TestPGCase{number:03d}" for number in range(175, 0, -1)])
        result = self.run_fixture()
        self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
        calls = self.read_log("go.jsonl")
        self.assertEqual(len(calls), 2)
        self.assertEqual(calls[0], ["list", "-tags", "postgres", "-f", "{{.Dir}}", "./package"])
        self.assertEqual(calls[1][:5], ["test", "-c", "-tags", "postgres", "-race"])
        runs = self.read_log("batches.jsonl")
        self.assertEqual(len(runs), 35)
        expected = sorted(config["names"])
        for number, run in enumerate(runs):
            self.assertEqual(sorted(run["tests"]), expected[number * 5:(number + 1) * 5])
            self.assertEqual(run["count"], 3)
            self.assertEqual(run["cwd"], str(self.root / "package"))
            self.assertIn("-test.timeout=120s", run["args"])
            self.assertIn("-test.parallel=1", run["args"])
            self.assertIn("-test.v", run["args"])
            pattern = next(arg.removeprefix("-test.run=") for arg in run["args"] if arg.startswith("-test.run="))
            self.assertTrue(pattern.startswith("^(") and pattern.endswith(")$"))
            self.assertIsNone(re.search(pattern, run["tests"][0] + "Suffix"))
        for repetition in range(3):
            actual = Counter(name for run in runs if repetition < run["count"] for name in run["tests"])
            self.assertEqual(actual, Counter(config["names"]))
        self.assertIn("passed 175 tests x 3 repetitions", result.stdout)

    def test_final_partial_batch_is_not_lost(self):
        self.configure()
        result = self.run_fixture()
        self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
        self.assertEqual([len(run["tests"]) for run in self.read_log("batches.jsonl")], [5, 5, 2])

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
                self.assertNotIn("passed 12 tests", result.stdout)
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
                    if descendant:
                        descendant_pid = int((self.root / "descendant.pid").read_text())
                        self.assertEqual(os.getpgid(descendant_pid), child)
                    process.send_signal(signum)
                    remainder, _ = process.communicate(timeout=8)
                    self.assertEqual(process.returncode, 128 + signum, (output + remainder).decode())
                    with self.assertRaises(ProcessLookupError):
                        os.kill(child, 0)
                    self.assertEqual(len(self.read_log("batches.jsonl")), 1)
                finally:
                    if child is not None:
                        try:
                            os.killpg(child, signal.SIGKILL)
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


if __name__ == "__main__":
    unittest.main()
