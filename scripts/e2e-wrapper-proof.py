#!/usr/bin/env python3
"""Read-only proof for section G; stdout is seed_path, seed_id, resume_id TSV.

Pass the unchanged fixture directory spelling used by e2e-sync.sh. Each AGENT
record must contain the fake child's argv encoded as a JSON array of strings.
"""
import json
import os
from pathlib import Path
import re
import stat
import sys


LIMIT = 64 * 1024
UUID = re.compile(r"[0-9a-fA-F]{8}(?:-[0-9a-fA-F]{4}){3}-[0-9a-fA-F]{12}\Z")


class ProofError(Exception):
    """Messages contain only controlled reasons, never input contents."""


def directory(path):
    info = path.lstat()
    if not stat.S_ISDIR(info.st_mode) or info.st_uid != os.getuid():
        raise ProofError("unsafe fixture directory")
    return info


def regular(path):
    fd = os.open(path, os.O_RDONLY | os.O_NOFOLLOW | os.O_NONBLOCK)
    try:
        info = os.fstat(fd)
        if not stat.S_ISREG(info.st_mode) or info.st_uid != os.getuid() or info.st_nlink != 1:
            raise ProofError("unsafe fixture file")
        return os.fdopen(fd, "rb")
    except BaseException:
        os.close(fd)
        raise


def read_text(path):
    with regular(path) as source:
        data = source.read(LIMIT + 1)
    if len(data) > LIMIT:
        raise ProofError("fixture metadata exceeds bound")
    return data.decode("utf-8")


def unique(pairs):
    result = {}
    for key, value in pairs:
        if key in result:
            raise ProofError("duplicate boundary field")
        result[key] = value
    return result


def prove(fixture, original_id):
    if not UUID.fullmatch(original_id):
        raise ProofError("invalid original session ID")
    raw = Path(fixture)
    if not raw.is_absolute() or ".." in raw.parts or not str(raw).isprintable():
        raise ProofError("invalid fixture path")
    if directory(raw).st_mode & 0o077:
        raise ProofError("fixture directory must be private")
    base = raw.resolve(strict=True)
    project_name = re.sub(r"[^A-Za-z0-9]", "-", str(raw / "repo2"))
    relative_project = Path("home/.claude/projects") / project_name
    for relative in ("repo2", "repo2/.cxt", "home", "home/.claude", "home/.claude/projects", relative_project):
        directory(base / relative)
    project = base / relative_project
    boundary = json.loads(read_text(base / "repo2/.cxt/boundary.json"), object_pairs_hook=unique)
    if not isinstance(boundary, dict):
        raise ProofError("invalid boundary object")
    if boundary.get("prev_branch") != "main" or boundary.get("branch") != "feature-x":
        raise ProofError("unexpected boundary transition")
    seed_id = boundary.get("seed_id")
    if not isinstance(seed_id, str) or not UUID.fullmatch(seed_id) or seed_id == original_id:
        raise ProofError("invalid new seed ID")
    seed = project / (seed_id + ".jsonl")
    allowed = {str(seed), str(raw / relative_project / seed.name)}
    if not isinstance(boundary.get("seed_path"), str) or boundary["seed_path"] not in allowed:
        raise ProofError("seed path does not match fixture project and ID")
    if boundary.get("resume_cmd") != "claude --resume " + seed_id:
        raise ProofError("boundary resume selector mismatch")
    with regular(seed):
        pass  # Verify readability/type without reading the native seed body.
    original = project / (original_id + ".jsonl.superseded")
    original_names = {str(original), str(raw / relative_project / original.name)}
    superseded = boundary.get("superseded")
    if (not isinstance(superseded, list) or not all(isinstance(p, str) for p in superseded) or
            not any(p in original_names for p in superseded)):
        raise ProofError("original session is not listed as superseded")
    with regular(original):
        pass  # The holder must use this exact original file, not a glob result.
    log = read_text(base / "agent.log")
    if not log.endswith("\n"):
        raise ProofError("incomplete child log")
    launches = [line[len("AGENT "):] for line in log.splitlines() if line.startswith("AGENT ")]
    if len(launches) != 2:
        raise ProofError("expected exactly two AGENT records")
    for record, expected in zip(launches, (original_id, seed_id)):
        argv = json.loads(record)
        if (not isinstance(argv, list) or not all(isinstance(value, str) for value in argv) or
                argv[:2] != ["--resume", expected] or argv.count("--resume") != 1 or
                any(value.startswith("--resume=") for value in argv)):
            raise ProofError("child must have one exact leading resume selector")
    if not str(seed).isprintable():
        raise ProofError("unsafe TSV path")
    return str(seed), seed_id, seed_id


def main(args=None):
    args = sys.argv[1:] if args is None else args
    if len(args) != 2:
        print("usage: e2e-wrapper-proof.py FIXTURE_DIR ORIGINAL_SESSION_ID", file=sys.stderr)
        return 1
    try:
        values = prove(*args)
    except ProofError as error:
        print(f"wrapper proof failed: {error}", file=sys.stderr)
        return 1
    except (OSError, ValueError, RecursionError):
        print("wrapper proof failed: unreadable or invalid fixture metadata", file=sys.stderr)
        return 1
    print("\t".join(values))
    return 0


if __name__ == "__main__":
    sys.exit(main())
