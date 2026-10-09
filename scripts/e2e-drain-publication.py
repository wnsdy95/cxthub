#!/usr/bin/env python3
"""Fence publication in an idle e2e-sync fixture before local-only work.

Usage: e2e-drain-publication.py /fixture/bin/cxt /fixture/repo [...]
The caller must stop starting Git commands, end fixture app sessions through
SessionEnd before draining, and preserve the fixture on failure.
Timeouts never terminate children: a timed-out command may still be running.
"""
import contextlib
import fcntl
import json
import os
from pathlib import Path
import re
import stat
import subprocess
import sys
import tempfile
import time


BOUND = 45
POLL = 0.02
LIVE_EVENTS = {"live-watch", "live-capture", "live-publish"}
EVENTS = {"branch-replay", "branch-deletion-finalize", "pending-sync",
          "branch-state-sync", "capture-replay", "historical-sync"} | LIVE_EVENTS
HASH = re.compile(r"sha256:[0-9a-f]{64}\Z")


class DrainError(Exception):
    """Only controlled, body/argv-free diagnostics may reach the caller."""


def owned(path, directory=False):
    info = path.lstat()
    kind = stat.S_ISDIR if directory else stat.S_ISREG
    if not kind(info.st_mode) or info.st_uid != os.getuid():
        raise DrainError("fixture ownership/type check failed")
    return info


def fixture(binary, roots):
    raw = Path(binary)
    if (not raw.is_absolute() or ".." in raw.parts or
            any(c.isspace() or c in "\"'\\" for c in str(raw))):
        raise DrainError("fixture executable must be an unambiguous absolute path")
    info = owned(raw)
    exe = raw.resolve(strict=True)
    base = exe.parent.parent
    if (exe.name != "cxt" or exe.parent.name != "bin" or info.st_nlink != 1 or
            not os.access(exe, os.X_OK) or raw.parent.is_symlink()):
        raise DrainError("expected a unique fixture/bin/cxt executable")
    if owned(base, True).st_mode & 0o077:
        raise DrainError("fixture directory must be private")
    owned(exe.parent, True)
    home = base / "home"
    owned(home, True)
    if Path(os.environ.get("HOME", "")).resolve() != home:
        raise DrainError("HOME does not belong to this fixture")
    selected = []
    for value in roots:
        root = Path(value)
        if not root.is_absolute() or ".." in root.parts or root.is_symlink():
            raise DrainError("expected explicit fixture repository roots")
        root = root.resolve(strict=True)
        if root.parent != base or root in (home, exe.parent):
            raise DrainError("repository root is outside this fixture")
        owned(root, True)
        owned(root / ".cxt", True)
        owned(root / ".cxt" / "HEAD")
        selected.append(root)
    if not selected or len(set(selected)) != len(selected):
        raise DrainError("repository roots must be nonempty and distinct")
    return str(raw), {str(raw), str(exe)}, sorted(selected)


def matching_helpers(output, executables):
    if not output.strip():
        raise DrainError("invalid process inventory: empty")
    matches = []
    for line in output.splitlines():
        fields = line.split()
        if not fields:
            continue
        if len(fields) < 2 or not fields[0].isascii() or not fields[0].isdigit():
            raise DrainError("invalid process inventory")
        if fields[1] not in executables:
            continue
        if len(fields) == 2 or (fields[2] == "git-hook" and len(fields) == 3):
            raise DrainError(f"incomplete fixture helper argv: pid={int(fields[0])}")
        if fields[2] != "git-hook":
            continue
        event, args = fields[3], fields[4:]
        if event not in EVENTS:
            continue
        pid = int(fields[0])
        numeric = lambda x: x.isascii() and x.isdigit() and int(x) > 0
        valid = not args
        if event == "branch-replay":
            valid = not args or (len(args) == 1 and numeric(args[0]))
        elif event == "branch-deletion-finalize":
            valid = (len(args) == 3 and not args[0].startswith("-") and
                     re.fullmatch(r"[0-9a-f]{40}|[0-9a-f]{64}", args[1]) and numeric(args[2]))
        elif event == "pending-sync":
            valid = len(args) % 3 == 0 and all(
                args[i] == "--resolve" and HASH.fullmatch(args[i + 2])
                for i in range(0, len(args), 3))
        elif event in LIVE_EVENTS:
            valid = (len(args) == 2 and args[0] in ("claude", "codex") and
                     len(args[1].encode("utf-8")) <= 256 and args[1].isprintable())
        if not valid or pid <= 0:
            raise DrainError(f"unrecognized fixture helper argv: pid={pid} event={event}")
        matches.append((pid, event))
    return matches


def remaining(deadline, phase):
    left = deadline - time.monotonic()
    if left <= 0:
        raise DrainError(f"deadline exceeded: {phase}")
    return left


def command(args, deadline, phase, cwd=None, limit=1 << 20, seconds=45):
    # Unlike subprocess.run(timeout=...), wait(timeout=...) never kills a child.
    # A file also avoids deadlocking on pipes or echoing private command output.
    budget = min(seconds, remaining(deadline, phase))
    with tempfile.TemporaryFile() as output:
        child = subprocess.Popen(args, cwd=cwd, stdin=subprocess.DEVNULL,
                                 stdout=output, stderr=subprocess.DEVNULL)
        try:
            code = child.wait(timeout=max(0, min(budget, deadline - time.monotonic())))
        except subprocess.TimeoutExpired:
            identity = "event=inventory" if phase == "inventory" else f"pid={child.pid} event={phase}"
            raise DrainError(f"command still running: {identity}") from None
        if code:
            raise DrainError(f"command failed: event={phase} exit={code}")
        output.seek(0)
        raw = output.read(limit + 1)
    if len(raw) > limit:
        raise DrainError(f"command output exceeds fixture bound: event={phase}")
    try:
        return raw.decode("utf-8")
    except UnicodeError:
        raise DrainError(f"invalid command encoding: event={phase}") from None


def inventory(executables, deadline):
    return matching_helpers(command(["ps", "-ww", "-axo", "pid=,command="],
                                    deadline, "inventory", limit=4 << 20, seconds=5), executables)


def check_status(text):
    def unique(pairs):
        result = {}
        for key, value in pairs:
            if key in result:
                raise ValueError()
            result[key] = value
        return result
    try:
        report = json.loads(text, object_pairs_hook=unique)
        if (not isinstance(report, dict) or not isinstance(report["repository"], str) or
                not HASH.fullmatch(report["repository"]) or report["server_checked"] is not False or
                report["integrity_checked"] is not False):
            raise ValueError()
        for key in ("historical_uploads", "branch_operations", "issues"):
            if not isinstance(report[key], list):
                raise ValueError()
        if (not all(isinstance(x, str) for x in report["issues"]) or
                not all(isinstance(x, dict) for key in ("historical_uploads", "branch_operations") for x in report[key]) or
                not all(type(report[key]) is int and report[key] >= 0 for key in ("ready", "retry_waiting")) or
                report["ready"] + report["retry_waiting"] != len(report["historical_uploads"])):
            raise ValueError()
    except (ValueError, KeyError, TypeError, RecursionError):
        raise DrainError("invalid sync status structure") from None
    if any(report[key] for key in ("historical_uploads", "branch_operations", "issues")):
        raise DrainError("fixture publication unresolved: " + " ".join(
            f"{key}={len(report[key])}" for key in ("historical_uploads", "branch_operations", "issues")))


@contextlib.contextmanager
def publication_locks(root, deadline):
    directory = root / ".cxt"
    for name in ("locks", "historical-backfill"):
        directory /= name
        directory.mkdir(exist_ok=True)
        owned(directory, True)
    with contextlib.ExitStack() as stack:
        for name in ("daemon", "worker", "queue"):
            fd = os.open(directory / (name + ".flock"), os.O_CREAT | os.O_RDWR | os.O_NOFOLLOW | os.O_NONBLOCK, 0o600)
            lock = stack.enter_context(os.fdopen(fd, "r+"))
            info = os.fstat(lock.fileno())
            if not stat.S_ISREG(info.st_mode) or info.st_uid != os.getuid() or info.st_nlink != 1:
                raise DrainError("invalid fixture publication lock")
            while True:
                remaining(deadline, f"{name} lock")
                try:
                    fcntl.flock(lock, fcntl.LOCK_EX | fcntl.LOCK_NB)
                    break
                except BlockingIOError:
                    time.sleep(min(POLL, remaining(deadline, f"{name} lock")))
        yield


def drain(binary, roots, timeout=BOUND):
    if not 0 < timeout <= BOUND:
        raise DrainError("invalid fixture deadline")
    binary, executables, roots = fixture(binary, roots)
    deadline = time.monotonic() + timeout
    last_seen = "unavailable"
    def observe():
        nonlocal last_seen
        try:
            running = inventory(executables, deadline)
        except DrainError as error:
            raise DrainError(f"{error}; last fixture observation: {last_seen}") from None
        last_seen = " ".join(f"pid={pid} event={event}" for pid, event in running) or "none"
        return running
    while True:
        running = observe()
        producers = [(pid, event) for pid, event in running if event != "historical-sync"]
        if producers:
            phase = f"last fixture observation: {last_seen}"
            time.sleep(min(POLL, remaining(deadline, phase)))
            continue
        for root in roots:
            command([binary, "git-hook", "historical-sync"], deadline, "historical-sync", root)
        with contextlib.ExitStack() as locks:
            for root in roots:
                locks.enter_context(publication_locks(root, deadline))
            # A producer or just-launched daemon may have appeared during handoff.
            if not observe():
                for root in roots:
                    check_status(command([binary, "sync", "status", "--json"], deadline,
                                         "sync-status", root, seconds=10))
                if not observe():
                    remaining(deadline, "completion")
                    return
        time.sleep(min(POLL, remaining(deadline, f"publication handoff; last fixture observation: {last_seen}")))


def main(args=None):
    args = sys.argv[1:] if args is None else args
    if len(args) < 2:
        print("usage: e2e-drain-publication.py /fixture/bin/cxt /fixture/repo [...]", file=sys.stderr)
        return 1
    try:
        drain(args[0], args[1:])
    except DrainError as error:
        print(f"fixture publication fence failed: {error}", file=sys.stderr)
        return 1
    except (OSError, ValueError):
        print("fixture publication fence failed: filesystem/process access error", file=sys.stderr)
        return 1
    print("fixture publication drained")
    return 0


if __name__ == "__main__":
    sys.exit(main())
