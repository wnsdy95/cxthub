#!/usr/bin/env python3
"""Fail on high or critical npm advisories, except dated exceptions.

`npm audit --audit-level=high` cannot ignore one advisory. When an advisory
has no patched release, the only choices it leaves are a broken build or no
gate at all. This gate keeps every other advisory blocking and lets a named
advisory through until its exception expires.

An exception names the advisory, the package it was reviewed for, the reason,
and the last day it applies. An expired exception, or one whose advisory now
reports another package, blocks again.
"""

import argparse
import datetime
import json
from pathlib import Path
import re
import subprocess
import sys


BLOCKING = {"high", "critical"}
ADVISORY_ID = re.compile(r"^GHSA(-[0-9a-z]{4}){3}$")


class GateError(Exception):
    pass


def load_exceptions(path):
    if not path.exists():
        return {}
    try:
        entries = json.loads(path.read_text())
    except (OSError, ValueError) as error:
        raise GateError(f"{path}: unreadable exceptions: {error}")
    if not isinstance(entries, list):
        raise GateError(f"{path}: exceptions must be a list")
    exceptions = {}
    for entry in entries:
        if not isinstance(entry, dict) or set(entry) != {"advisory", "package", "reason", "expires"}:
            raise GateError(f"{path}: every exception needs exactly advisory, package, reason and expires")
        advisory, package, reason = entry["advisory"], entry["package"], entry["reason"]
        if not isinstance(advisory, str) or not ADVISORY_ID.match(advisory) or advisory in exceptions:
            raise GateError(f"{path}: invalid or repeated advisory {advisory!r}")
        if not isinstance(package, str) or not package or not isinstance(reason, str) or len(reason.strip()) < 20:
            raise GateError(f"{path}: {advisory} needs a package and a reason of at least 20 characters")
        try:
            expires = datetime.date.fromisoformat(entry["expires"])
        except (TypeError, ValueError):
            raise GateError(f"{path}: {advisory} needs an expires date as YYYY-MM-DD")
        exceptions[advisory] = {"package": package, "expires": expires}
    return exceptions


def blocking_advisories(report):
    if not isinstance(report, dict) or "error" in report or not isinstance(report.get("vulnerabilities"), dict):
        raise GateError("npm audit did not return a vulnerability report")
    found = {}
    for entry in report["vulnerabilities"].values():
        for via in entry.get("via", []):
            # A string names another package in the chain; only objects are advisories.
            if not isinstance(via, dict) or via.get("severity") not in BLOCKING:
                continue
            url = str(via.get("url", ""))
            advisory = url.rsplit("/", 1)[-1]
            if not ADVISORY_ID.match(advisory):
                # An advisory that cannot be named cannot be excepted either.
                advisory = url or f"unidentified advisory for {via.get('name')}"
            found[advisory] = {"package": via.get("name"), "severity": via["severity"], "title": via.get("title", ""), "range": via.get("range", "")}
    return found


def evaluate(report, exceptions, today):
    blocked, excused = [], []
    found = blocking_advisories(report)
    for advisory, details in sorted(found.items()):
        line = f"{advisory} {details['package']} {details['range']} ({details['severity']}): {details['title']}"
        exception = exceptions.get(advisory)
        if exception is None:
            blocked.append(line)
        elif exception["package"] != details["package"]:
            blocked.append(f"{line} [exception was reviewed for {exception['package']}]")
        elif exception["expires"] < today:
            blocked.append(f"{line} [exception expired {exception['expires'].isoformat()}]")
        else:
            excused.append(f"{line} [excepted until {exception['expires'].isoformat()}]")
    unused = sorted(advisory for advisory in exceptions if advisory not in found)
    return blocked, excused, unused


def main():
    parser = argparse.ArgumentParser(description=__doc__.splitlines()[0])
    parser.add_argument("--exceptions", type=Path, default=Path("audit-exceptions.json"))
    parser.add_argument("--report", type=Path, help="read a saved `npm audit --json` report instead of running npm")
    parser.add_argument("--today", type=datetime.date.fromisoformat, default=datetime.datetime.now(datetime.timezone.utc).date())
    arguments = parser.parse_args()
    try:
        exceptions = load_exceptions(arguments.exceptions)
        if arguments.report:
            raw = arguments.report.read_text()
        else:
            # npm exits nonzero whenever it reports anything; the report decides.
            raw = subprocess.run(["npm", "audit", "--json"], stdout=subprocess.PIPE, text=True, check=False).stdout
        try:
            report = json.loads(raw)
        except ValueError:
            raise GateError("npm audit did not return JSON")
        blocked, excused, unused = evaluate(report, exceptions, arguments.today)
    except (GateError, OSError) as error:
        print(f"npm audit gate: {error}", file=sys.stderr)
        return 2
    for line in excused:
        print(f"excepted: {line}")
    for advisory in unused:
        print(f"::warning::audit exception {advisory} no longer matches a high or critical advisory; remove it")
    for line in blocked:
        print(f"blocking: {line}", file=sys.stderr)
    if blocked:
        print(f"npm audit gate: {len(blocked)} high or critical advisory(ies) without a valid exception", file=sys.stderr)
        return 1
    print(f"npm audit gate: no blocking advisories ({len(excused)} excepted)")
    return 0


if __name__ == "__main__":
    sys.exit(main())
