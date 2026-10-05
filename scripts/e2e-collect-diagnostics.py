#!/usr/bin/env python3
"""Keep allowlisted synthetic E2E failure evidence, never the fixture HOME."""
import json
import os
from pathlib import Path
import stat
import sys


FILES = (
    "app-end.out", "app-drain.out", "fork-end.out", "repair-init.out", "repair-witness.out", "repair-drain.out",
    "repair-save.out", "repair-proof.out", "repair.out", "repair-retry.out", "repair-wrong.out",
    "doctor-before.json", "doctor-before.err", "doctor-before.status",
    "doctor-after.json", "doctor-after.err", "doctor-after.status",
    "wrapper-proof.tsv", "wrapper-proof.err", "wrapper-observation.out",
    "sw.out", "wrapper.out", "holder.out",
)
MAX_BYTES = 1024 * 1024


def collect(source, destination):
    source, destination = Path(source).resolve(), Path(destination).absolute()
    if destination.resolve() == source or source in destination.resolve().parents:
        raise ValueError("diagnostics must be outside the disposable fixture")
    # Refuse reuse, including symlinks. CI supplies a fresh private parent, so
    # stale results and unrelated files cannot enter the uploaded directory.
    destination.mkdir(parents=True, mode=0o700, exist_ok=False)
    def save(name, data):
        with os.fdopen(os.open(destination / name, os.O_WRONLY | os.O_CREAT | os.O_EXCL | os.O_NOFOLLOW, 0o600), "wb") as output:
            output.write(data)
    entries = []
    for name in FILES:
        path = source / name
        if path.is_symlink() or not path.is_file():
            continue
        with os.fdopen(os.open(path, os.O_RDONLY | os.O_NOFOLLOW), "rb") as stream:
            info = os.fstat(stream.fileno())
            if not stat.S_ISREG(info.st_mode) or info.st_nlink != 1:
                continue
            truncated = info.st_size > MAX_BYTES
            if truncated:
                stream.seek(-MAX_BYTES, os.SEEK_END)
            data = stream.read(MAX_BYTES)
        saved = name + (".tail" if truncated else "")
        save(saved, data)
        entries.append({"file": saved, "bytes": len(data), "truncated": truncated})
    save("index.json", (json.dumps(entries, indent=2) + "\n").encode())
    return entries


if __name__ == "__main__":
    if len(sys.argv) != 3:
        raise SystemExit("usage: e2e-collect-diagnostics.py FIXTURE OUTPUT")
    collect(*sys.argv[1:])
