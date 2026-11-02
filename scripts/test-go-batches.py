#!/usr/bin/env python3
import argparse
from collections import Counter
import json
import math
import os
from pathlib import Path
import re
import shlex
import signal
import subprocess
import sys
import tempfile


class Cancelled(Exception):
    def __init__(self, signum):
        self.signum = signum
        super().__init__(f"cancelled by signal {signum}")


def cancel(signum, frame):
    signal.signal(signal.SIGINT, signal.SIG_IGN)
    signal.signal(signal.SIGTERM, signal.SIG_IGN)
    raise Cancelled(signum)


def run_command(command, cwd, capture=False, on_line=None):
    print("[go-batches] " + shlex.join(command), flush=True)
    process = subprocess.Popen(
        command, cwd=cwd, text=True, start_new_session=True,
        stdout=subprocess.PIPE if capture or on_line is not None else None,
    )
    try:
        if on_line is not None:
            for line in process.stdout:
                on_line(line)
        output, _ = process.communicate()
    except BaseException:
        try:
            os.killpg(process.pid, signal.SIGTERM)
        except ProcessLookupError:
            pass
        try:
            process.communicate(timeout=5)
        except subprocess.TimeoutExpired:
            pass
        finally:
            try:
                os.killpg(process.pid, signal.SIGKILL)
            except ProcessLookupError:
                pass
        process.communicate(timeout=5)
        raise
    else:
        try:
            os.killpg(process.pid, signal.SIGKILL)
        except ProcessLookupError:
            pass
    finally:
        if process.stdout is not None:
            process.stdout.close()
    if process.returncode != 0:
        raise subprocess.CalledProcessError(process.returncode, command, output)
    return output or ""


def event_object(pairs):
    event = dict(pairs)
    if len(event) != len(pairs):
        raise ValueError("test2json returned duplicate JSON fields")
    return event


class BatchResults:
    def __init__(self, names, count, package, allow_skip=()):
        self.names = set(names)
        self.count = count
        self.package = package
        self.allow_skip = set(allow_skip)
        self.started = Counter()
        self.completed = Counter()
        self.outcomes = Counter()
        self.skipped = Counter()
        self.active = {}
        self.package_started = False
        self.package_outcome = None
        self.failed = False
        self.cancelled = 0

    def feed(self, line):
        if not line.endswith("\n"):
            raise ValueError("unterminated test2json event")
        event = json.loads(line, object_pairs_hook=event_object)
        if not isinstance(event, dict) or not isinstance(event.get("Action"), str):
            raise ValueError("malformed test2json event")
        action = event["Action"]
        name = event.get("Test", "")
        if not isinstance(name, str) or ("Test" in event and not name):
            raise ValueError("malformed test2json test name")
        if event.get("Package") != self.package:
            raise ValueError("test2json returned an unknown package")
        if "Elapsed" in event:
            elapsed = event["Elapsed"]
            if type(elapsed) not in (int, float) or elapsed < 0 or (type(elapsed) is float and not math.isfinite(elapsed)):
                raise ValueError("malformed test2json elapsed time")
        if self.package_outcome is not None:
            raise ValueError("test2json event after package outcome")
        if action == "start":
            if name or self.package_started:
                raise ValueError("unexpected test2json start event")
            self.package_started = True
            return
        if not self.package_started:
            raise ValueError("test2json event before package start")
        top_level = name.split("/", 1)[0]
        if name and top_level not in self.names:
            raise ValueError(f"test2json returned an unknown test: {name}")
        if action == "output":
            if not isinstance(event.get("Output"), str):
                raise ValueError("malformed test2json output event")
            print(event["Output"], end="", flush=True)
            return
        if not name:
            if action not in ("pass", "fail", "skip"):
                raise ValueError(f"unexpected test2json package action: {action}")
            self.package_outcome = action
            return
        if action == "run":
            if name in self.active:
                raise ValueError(f"duplicate test2json run: {name}")
            if name == top_level:
                if self.started[name] >= self.count:
                    raise ValueError(f"too many test2json runs: {name}")
                self.started[name] += 1
            elif top_level not in self.active:
                raise ValueError(f"test2json subtest without active parent: {name}")
            self.active[name] = "running"
            return
        state = self.active.get(name)
        if action == "pause" and state == "running":
            self.active[name] = "paused"
        elif action == "cont" and state == "paused":
            self.active[name] = "running"
        elif action in ("attr", "artifacts") and state is not None:
            fields = ("Key", "Value") if action == "attr" else ("Path",)
            if any(not isinstance(event.get(field), str) for field in fields):
                raise ValueError(f"malformed test2json {action} event")
        elif action in ("pass", "fail", "skip") and state == "running":
            if any(active.startswith(name + "/") for active in self.active):
                raise ValueError(f"test2json outcome before subtests finished: {name}")
            del self.active[name]
            self.failed |= action == "fail"
            if name == top_level:
                self.completed[name] += 1
                self.outcomes[action] += 1
                if action == "skip":
                    self.skipped[name] += 1
        else:
            raise ValueError(f"unexpected test2json {action} for {name}: {state or 'not running'}")

    def verify(self):
        if self.active:
            raise ValueError("unfinished test2json tests: " + ", ".join(sorted(self.active)))
        missing = sorted(name for name in self.names if self.completed[name] != self.count)
        if missing:
            raise ValueError("missing test2json outcomes: " + ", ".join(missing))
        if self.package_outcome != "pass":
            raise ValueError(f"missing or unsuccessful test2json package outcome: {self.package_outcome}")
        if self.failed:
            raise ValueError("test2json reported failed tests")
        unexpected_skips = sorted(set(self.skipped) - self.allow_skip)
        if unexpected_skips:
            raise ValueError("unexpected top-level test skip: " + ", ".join(unexpected_skips))


def discover_tests(output):
    names = [line.strip() for line in output.splitlines() if line.strip()]
    if not names:
        raise ValueError("test discovery returned no tests")
    if any(re.fullmatch(r"Test[A-Za-z0-9_]+", name) is None for name in names):
        raise ValueError("test discovery returned an unexpected test name or output")
    if len(set(names)) != len(names):
        raise ValueError("test discovery returned duplicate test names")
    return sorted(names)


def positive_int(value):
    number = int(value)
    if number <= 0:
        raise argparse.ArgumentTypeError("must be positive")
    return number


def run_batches(args):
    cwd = Path.cwd()
    tags = ["-tags", args.tags] if args.tags else []
    directories = run_command(["go", "list", *tags, "-f", "{{.Dir}}", args.package], cwd, capture=True).splitlines()
    if len(directories) != 1 or not Path(directories[0]).is_dir():
        raise ValueError("select exactly one existing Go package")
    package_dir = Path(directories[0])
    with tempfile.TemporaryDirectory(prefix="cxt-go-batches-") as temporary:
        binary = str(Path(temporary) / "package.test")
        race = ["-race"] if args.race else []
        run_command(["go", "test", "-c", *tags, *race, "-o", binary, args.package], cwd)
        tests = discover_tests(run_command([binary, "-test.list=" + args.run], package_dir, capture=True))
        allow_skip = set(args.allow_skip)
        if len(allow_skip) != len(args.allow_skip):
            raise ValueError("duplicate --allow-skip test names")
        unknown_skips = sorted(allow_skip - set(tests))
        if unknown_skips:
            raise ValueError("--allow-skip names not in discovered selection: " + ", ".join(unknown_skips))
        required = set(args.require_test)
        if len(required) != len(args.require_test):
            raise ValueError("duplicate --require-test test names")
        missing_required = sorted(required - set(tests))
        if missing_required:
            raise ValueError("--require-test names not in discovered selection: " + ", ".join(missing_required))
        optional_required = sorted(required.intersection(allow_skip))
        if optional_required:
            raise ValueError("required tests cannot allow skips: " + ", ".join(optional_required))
        batches = [tests[start:start + args.batch_size] for start in range(0, len(tests), args.batch_size)]
        print(f"[go-batches] {len(tests)} tests, {len(batches)} serial batches, {args.count} repetitions; cwd={package_dir}", flush=True)
        results = []
        try:
            for number, batch in enumerate(batches, 1):
                print(f"[go-batches] batch {number}/{len(batches)}: " + ", ".join(batch), flush=True)
                pattern = "^(" + "|".join(re.escape(name) for name in batch) + ")$"
                result = BatchResults(batch, args.count, args.package, allow_skip.intersection(batch))
                results.append(result)
                run_command([
                    "go", "tool", "test2json", "-t", "-p", args.package,
                    binary, "-test.run=" + pattern, f"-test.count={args.count}",
                    "-test.timeout=120s", "-test.parallel=1", "-test.v=test2json",
                ], package_dir, on_line=result.feed)
                result.verify()
        except Cancelled:
            if results:
                results[-1].cancelled = sum("/" not in name for name in results[-1].active)
            raise
        finally:
            outcomes = Counter()
            for result in results:
                outcomes.update(result.outcomes)
            expected = len(tests) * args.count
            started = sum(sum(result.started.values()) for result in results)
            completed = sum(outcomes.values())
            cancelled = sum(result.cancelled for result in results)
            allowed_skips = sum(result.skipped[name] for result in results for name in result.allow_skip)
            print(
                f"[go-batches] results: selected={len(tests)} expected={expected} run={started} "
                f"pass={outcomes['pass']} fail={outcomes['fail']} skip={outcomes['skip']} "
                f"missing={expected - completed - cancelled} cancelled={cancelled} "
                f"unfinished={started - completed - cancelled} allowed_skip={allowed_skips}", flush=True,
            )
        print(f"[go-batches] passed {outcomes['pass']} top-level test runs; allowed skips={allowed_skips}", flush=True)


def main(argv=None):
    parser = argparse.ArgumentParser(description="Compile one Go package once and run serial batches with a fixed 120s timeout.")
    parser.add_argument("package")
    parser.add_argument("--tags", default="")
    parser.add_argument("--run", default="^Test")
    parser.add_argument("--require-test", action="append", default=[], metavar="TEST", help="require an exact top-level test name in the discovered selection and forbid allowing it to skip; repeat for each name")
    parser.add_argument("--allow-skip", action="append", default=[], metavar="TEST", help="allow an exact discovered top-level test name to skip; repeat for each name")
    parser.add_argument("--race", action="store_true")
    parser.add_argument("--count", type=positive_int, default=1)
    parser.add_argument("--batch-size", type=int, choices=range(1, 6), default=5)
    args = parser.parse_args(argv)
    handlers = {signum: signal.signal(signum, cancel) for signum in (signal.SIGINT, signal.SIGTERM)}
    try:
        run_batches(args)
    except Cancelled as error:
        print(f"[go-batches] {error}", file=sys.stderr)
        return 128 + error.signum
    except (OSError, ValueError, subprocess.SubprocessError) as error:
        print(f"[go-batches] failed: {error}", file=sys.stderr)
        return 1
    finally:
        for signum, handler in handlers.items():
            signal.signal(signum, handler)
    return 0


if __name__ == "__main__":
    sys.exit(main())
