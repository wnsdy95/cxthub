#!/usr/bin/env python3
"""Validate synthetic repair prerequisites before corruption or success claims."""
import json
import re
import sys


def content_hash(value):
    return isinstance(value, str) and re.fullmatch(r"sha256:[0-9a-f]{64}", value)


def local_only(manifest, repository, baseline, local):
    if not all(content_hash(value) for value in (repository, baseline, local)) or baseline == local:
        raise ValueError("invalid or unchanged repair snapshot identity")
    if not isinstance(manifest, dict) or manifest.get("repo_id") != repository:
        raise ValueError("repair manifest repository mismatch")
    snapshots = manifest.get("snapshot_index")
    if not isinstance(snapshots, list) or not all(content_hash(value) for value in snapshots):
        raise ValueError("invalid repair manifest snapshot index")
    if baseline not in snapshots:
        raise ValueError("published repair baseline is missing")
    if local in snapshots:
        raise ValueError("local-only repair snapshot was unexpectedly published")
    return "repair local-only proof: repository=" + repository + " baseline=" + baseline + " local=" + local


def issue_count(report):
    if not isinstance(report, dict) or report.get("completed") is not True:
        raise ValueError("doctor inspection incomplete")
    issues = report.get("issues")
    if not isinstance(issues, list):
        raise ValueError("doctor issues is not an array")
    return len(issues)


def unique(pairs):
    result = {}
    for key, value in pairs:
        if key in result:
            raise ValueError("duplicate JSON field")
        result[key] = value
    return result


if __name__ == "__main__":
    try:
        mode, path, *args = sys.argv[1:]
        with open(path) as source:
            report = json.load(source, object_pairs_hook=unique)
        if mode == "local-only" and len(args) == 3:
            print(local_only(report, *args))
        elif mode == "doctor" and not args:
            print(issue_count(report))
        else:
            raise ValueError("invalid repair proof arguments")
    except (ValueError, OSError, RecursionError) as error:
        # These inputs are synthetic E2E metadata, never provider records.
        print("repair proof failed: " + str(error), file=sys.stderr)
        raise SystemExit(1)
