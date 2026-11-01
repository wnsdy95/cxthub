#!/usr/bin/env python3

import re
import subprocess
import sys


PUBLIC_SETTINGS_INSPECTION_COMMIT = b"9fe43f1dee4c296b844b9576269f1bf32645a47e"
PUBLIC_SETTINGS_INSPECTION_LINE = (
    b"- live signed-in inspection of /jy" b"min/cxthub/settings"
)
FORBIDDEN_IDENTIFIERS = re.compile(
    b"s" b"wyg|rii" b"do|team-context-save-" b"fork|jy" b"min", re.IGNORECASE
)
METADATA_FIELDS = ("author name", "author email", "committer name", "committer email", "message")


def metadata_violations(history):
    fields = history.split(b"\0")
    if len(fields) <= 1 or fields[-1] != b"" or (len(fields) - 1) % 6:
        raise ValueError("malformed Git metadata records")
    violations = []
    for offset in range(0, len(fields) - 1, 6):
        commit, *metadata = fields[offset:offset + 6]
        if re.fullmatch(b"[0-9a-f]{40}|[0-9a-f]{64}", commit) is None:
            raise ValueError("malformed Git commit ID")
        if commit == PUBLIC_SETTINGS_INSPECTION_COMMIT:
            lines = metadata[-1].split(b"\n")
            if PUBLIC_SETTINGS_INSPECTION_LINE in lines:
                lines.remove(PUBLIC_SETTINGS_INSPECTION_LINE)
            metadata[-1] = b"\n".join(lines)
        for name, value in zip(METADATA_FIELDS, metadata):
            if FORBIDDEN_IDENTIFIERS.search(value):
                violations.append((commit.decode("ascii"), name))
    return violations


def main():
    try:
        result = subprocess.run(
            ["git", "log", "--all", "--no-show-signature", "-z",
             "--format=%H%x00%an%x00%ae%x00%cn%x00%ce%x00%B"],
            stdout=subprocess.PIPE, stderr=subprocess.PIPE, check=True,
        )
        violations = metadata_violations(result.stdout)
    except (OSError, subprocess.CalledProcessError, ValueError):
        print("[public-check] could not validate Git metadata history.", file=sys.stderr)
        return 1
    if violations:
        for commit, name in violations:
            print(f"[public-check] forbidden identifier in Git metadata: {commit} ({name}).",
                  file=sys.stderr)
        return 1
    print("[public-check] Git metadata history passed.")
    return 0


if __name__ == "__main__":
    sys.exit(main())
