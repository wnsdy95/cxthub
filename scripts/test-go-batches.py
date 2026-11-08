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
import threading
import time


class Cancelled(Exception):
    def __init__(self, signum):
        self.signum = signum
        super().__init__(f"cancelled by signal {signum}")


def cancel(signum, frame):
    signal.signal(signal.SIGINT, signal.SIG_IGN)
    signal.signal(signal.SIGTERM, signal.SIG_IGN)
    raise Cancelled(signum)


def echo(text):
    print(text, flush=True)


def run_command(command, cwd, capture=False, on_line=None, env=None, log=echo, started=None):
    log("[go-batches] " + shlex.join(command))
    process = subprocess.Popen(
        command, cwd=cwd, text=True, start_new_session=True, env=env,
        stdout=subprocess.PIPE if capture or on_line is not None else None,
    )
    if started is not None:
        started(process)
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
        self.output = None
        self.finished = False

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
            if self.output is None:
                print(event["Output"], end="", flush=True)
            else:
                self.output.append(event["Output"])
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
        self.finished = True
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


def shard_spec(value):
    match = re.fullmatch(r"([1-9][0-9]*)/([1-9][0-9]*)", value)
    if match is None or int(match[1]) > int(match[2]):
        raise argparse.ArgumentTypeError("must be INDEX/TOTAL with 1 <= INDEX <= TOTAL")
    return int(match[1]), int(match[2])


def shard_tests(tests, index, total):
    # Deal the ordered selection in snake order (1..N, N..1, ...). Every shard
    # derives the same discovery and order, so the shards are disjoint and their
    # union is the complete selection. Listing the slowest tests first with
    # --first-test spreads them across shards instead of stacking them.
    if total > len(tests):
        raise ValueError(f"--shard total {total} exceeds the {len(tests)} discovered tests")
    selected = []
    for position, name in enumerate(tests):
        lap, offset = divmod(position, total)
        if (offset if lap % 2 == 0 else total - 1 - offset) == index - 1:
            selected.append(name)
    return selected


def read_heavy_list(path, package):
    # One "<package> <TestName>" pair per line; "#" starts a comment. Only the
    # entries whose package equals this invocation's package apply.
    pairs = []
    for number, raw in enumerate(Path(path).read_text().splitlines(), 1):
        line = raw.split("#", 1)[0].strip()
        if not line:
            continue
        fields = line.split()
        if len(fields) != 2 or re.fullmatch(r"Test[A-Za-z0-9_]+", fields[1]) is None:
            raise ValueError(f"malformed --heavy-list line {number}: {raw.strip()}")
        pairs.append((fields[0].rstrip("/"), fields[1]))
    if len(set(pairs)) != len(pairs):
        raise ValueError("duplicate --heavy-list entries")
    return {name for owner, name in pairs if owner == package.rstrip("/")}


def worker_environment(specs):
    values = {}
    for spec in specs:
        match = re.fullmatch(r"([A-Za-z_][A-Za-z0-9_]*)=(.*)", spec, re.S)
        if match is None or "{worker}" not in match[2]:
            raise ValueError("--worker-env must be NAME=VALUE with a {worker} placeholder in VALUE")
        if match[1] in values:
            raise ValueError("duplicate --worker-env name: " + match[1])
        values[match[1]] = match[2]
    return values


def stop_processes(processes):
    for process in processes:
        try:
            os.killpg(process.pid, signal.SIGTERM)
        except ProcessLookupError:
            pass
    deadline = time.monotonic() + 5
    while time.monotonic() < deadline and any(process.poll() is None for process in processes):
        time.sleep(0.05)
    for process in processes:
        try:
            os.killpg(process.pid, signal.SIGKILL)
        except ProcessLookupError:
            pass


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
        first_tests = set(args.first_test)
        if len(first_tests) != len(args.first_test):
            raise ValueError("duplicate --first-test test names")
        unknown_first = sorted(first_tests - set(tests))
        if unknown_first:
            raise ValueError("--first-test names not in discovered selection: " + ", ".join(unknown_first))
        no_timeout = set(args.no_batch_timeout)
        if len(no_timeout) != len(args.no_batch_timeout):
            raise ValueError("duplicate --no-batch-timeout test names")
        unknown_no_timeout = sorted(no_timeout - set(tests))
        if unknown_no_timeout:
            raise ValueError("--no-batch-timeout names not in discovered selection: " + ", ".join(unknown_no_timeout))
        if no_timeout and args.batch_size != 1:
            raise ValueError("--no-batch-timeout requires --batch-size 1 to isolate the selected tests")
        tests = args.first_test + [name for name in tests if name not in first_tests]
        if args.heavy_list is not None:
            listed = read_heavy_list(args.heavy_list, args.package)
            package_tests = set(discover_tests(run_command([binary, "-test.list=^Test"], package_dir, capture=True)))
            stale = sorted(listed - package_tests)
            if stale:
                raise ValueError("--heavy-list names not in package: " + ", ".join(stale))
            heavy = [name for name in tests if name in listed]
            fast = [name for name in tests if name not in listed]
            print(f"[go-batches] tier {args.tier}: discovered={len(tests)} fast={len(fast)} heavy={len(heavy)}", flush=True)
            tests = {"all": tests, "fast": fast, "heavy": heavy}[args.tier]
        elif args.tier != "all":
            raise ValueError("--tier requires --heavy-list")
        if args.shard is not None:
            discovered = len(tests)
            tests = shard_tests(tests, *args.shard)
            print(f"[go-batches] shard {args.shard[0]}/{args.shard[1]}: {len(tests)} of {discovered} discovered tests", flush=True)
        batches = [tests[start:start + args.batch_size] for start in range(0, len(tests), args.batch_size)]
        environment = worker_environment(args.worker_env)
        print(f"[go-batches] {len(tests)} tests, {len(batches)} batches on {args.workers} worker(s), {args.count} repetitions; cwd={package_dir}", flush=True)

        def command_for(batch):
            pattern = "^(" + "|".join(re.escape(name) for name in batch) + ")$"
            timeout = "0" if batch[0] in no_timeout else "120s"
            return [
                "go", "tool", "test2json", "-t", "-p", args.package,
                binary, "-test.run=" + pattern, f"-test.count={args.count}",
                "-test.timeout=" + timeout, "-test.parallel=1", "-test.v=test2json",
            ]

        def env_for(worker):
            if not environment:
                return None
            return dict(os.environ, **{name: value.replace("{worker}", str(worker)) for name, value in environment.items()})

        results = []
        try:
            if args.workers == 1:
                for number, batch in enumerate(batches, 1):
                    print(f"[go-batches] batch {number}/{len(batches)}: " + ", ".join(batch), flush=True)
                    result = BatchResults(batch, args.count, args.package, allow_skip.intersection(batch))
                    results.append(result)
                    run_command(command_for(batch), package_dir, on_line=result.feed, env=env_for(1))
                    result.verify()
            else:
                run_parallel(batches, args, allow_skip, results, command_for, env_for, package_dir)
        except Cancelled:
            for result in results:
                if not result.finished:
                    result.cancelled = sum("/" not in name for name in result.active)
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


def run_parallel(batches, args, allow_skip, results, command_for, env_for, package_dir):
    # Each worker owns one environment (for example its own database) and runs
    # one batch at a time; the next batch in priority order goes to whichever
    # worker is free. Batch output is printed whole when the batch ends. After a
    # failure no new batch starts, and the first failure is raised.
    lock = threading.Lock()
    pending = list(enumerate(batches, 1))
    running = {}
    errors = []
    state = {"stop": False, "cancel": False}

    def work(worker):
        env = env_for(worker)
        while True:
            with lock:
                if state["stop"] or not pending:
                    return
                number, batch = pending.pop(0)
                result = BatchResults(batch, args.count, args.package, allow_skip.intersection(batch))
                result.output = []
                results.append(result)
                header = f"[go-batches] worker {worker} batch {number}/{len(batches)}: " + ", ".join(batch)
                echo(header + " (started)")

            def started(process):
                # A batch that starts while cancellation is in progress is stopped here,
                # because the canceller may already have collected the running set.
                with lock:
                    running[worker] = process
                    stopping = state["cancel"]
                if stopping:
                    stop_processes([process])

            error = None
            try:
                run_command(command_for(batch), package_dir, on_line=result.feed, env=env,
                            log=lambda text: result.output.append(text + "\n"), started=started)
                result.verify()
            except Exception as failure:
                error = failure
            finally:
                with lock:
                    running.pop(worker, None)
            with lock:
                print(header + (" (failed)" if error else " (done)") + "\n" + "".join(result.output), end="", flush=True)
                if error is not None and not state["stop"]:
                    errors.append(error)
                if error is not None:
                    state["stop"] = True

    threads = [threading.Thread(target=work, args=(worker,), daemon=True) for worker in range(1, args.workers + 1)]
    for thread in threads:
        thread.start()
    try:
        for thread in threads:
            while thread.is_alive():
                thread.join(0.1)
    except Cancelled:
        with lock:
            state["stop"] = state["cancel"] = True
            processes = list(running.values())
        stop_processes(processes)
        for thread in threads:
            thread.join(10)
        raise
    if errors:
        raise errors[0]


def main(argv=None):
    parser = argparse.ArgumentParser(description="Compile one Go package once and run serial batches with a default 120s timeout.")
    parser.add_argument("package")
    parser.add_argument("--tags", default="")
    parser.add_argument("--run", default="^Test")
    parser.add_argument("--first-test", action="append", default=[], metavar="TEST", help="move an exact discovered top-level test name to the front of the batch order; repeat in priority order")
    parser.add_argument("--no-batch-timeout", action="append", default=[], metavar="TEST", help="disable the binary timeout only for an exact discovered test; requires --batch-size 1 and an external CI watchdog")
    parser.add_argument("--require-test", action="append", default=[], metavar="TEST", help="require an exact top-level test name in the discovered selection and forbid allowing it to skip; repeat for each name")
    parser.add_argument("--allow-skip", action="append", default=[], metavar="TEST", help="allow an exact discovered top-level test name to skip; repeat for each name")
    parser.add_argument("--race", action="store_true")
    parser.add_argument("--count", type=positive_int, default=1)
    parser.add_argument("--batch-size", type=int, choices=range(1, 6), default=5)
    parser.add_argument("--shard", type=shard_spec, metavar="INDEX/TOTAL", help="run only this shard of the ordered selection; name validation still covers the whole selection")
    parser.add_argument("--heavy-list", metavar="FILE", help="checked-in '<package> <TestName>' list; names for this package must exist in it")
    parser.add_argument("--tier", choices=("all", "fast", "heavy"), default="all", help="with --heavy-list, run all discovered tests, only the unlisted (fast) ones, or only the listed (heavy) ones")
    parser.add_argument("--workers", type=positive_int, default=1, help="run batches concurrently on this many workers, each with its own --worker-env values")
    parser.add_argument("--worker-env", action="append", default=[], metavar="NAME=VALUE", help="per-worker environment; {worker} in VALUE becomes the worker number (1..N)")
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
