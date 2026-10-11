#!/usr/bin/env python3
import argparse
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


def run_command(command, cwd, capture=False):
    print("[go-batches] " + shlex.join(command), flush=True)
    process = subprocess.Popen(
        command, cwd=cwd, text=True, start_new_session=True,
        stdout=subprocess.PIPE if capture else None,
    )
    try:
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
    if process.returncode != 0:
        raise subprocess.CalledProcessError(process.returncode, command, output)
    return output or ""


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
        batches = [tests[start:start + args.batch_size] for start in range(0, len(tests), args.batch_size)]
        print(f"[go-batches] {len(tests)} tests, {len(batches)} serial batches, {args.count} repetitions; cwd={package_dir}", flush=True)
        for number, batch in enumerate(batches, 1):
            print(f"[go-batches] batch {number}/{len(batches)}: " + ", ".join(batch), flush=True)
            pattern = "^(" + "|".join(re.escape(name) for name in batch) + ")$"
            run_command([
                binary, "-test.run=" + pattern, f"-test.count={args.count}",
                "-test.timeout=120s", "-test.parallel=1", "-test.v",
            ], package_dir)
        print(f"[go-batches] passed {len(tests)} tests x {args.count} repetitions", flush=True)


def main(argv=None):
    parser = argparse.ArgumentParser(description="Compile one Go package once and run serial batches with a fixed 120s timeout.")
    parser.add_argument("package")
    parser.add_argument("--tags", default="")
    parser.add_argument("--run", default="^Test")
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
