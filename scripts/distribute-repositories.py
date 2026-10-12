#!/usr/bin/env python3
import argparse
import hashlib
import json
import os
from pathlib import Path, PurePosixPath
import re
import subprocess
import sys
import tempfile


MANIFEST = ".cxthub-source.json"
CONFIG = "distrib/repositories.json"
SOURCE = "wnsdy95/cxthub"
COMPONENTS = {"cli": "public", "backend": "private", "frontend": "private", "mcp": "private"}
BLOCKED_PARTS = {".git", ".cxt", "cxt-data", ".claude", ".codex", ".agents", "node_modules", "dist", "bin", "coverage", "playwright-report", "test-results", "__pycache__", "internal-private"}
BLOCKED_SUFFIXES = {".pem", ".key", ".p12", ".pfx", ".sqlite", ".sqlite3", ".db", ".dump", ".log", ".jsonl", ".tfstate", ".pyc"}


class DistributionError(Exception):
    pass


def run(arguments, cwd=None, data=None, env=None):
    result = subprocess.run(arguments, cwd=cwd, input=data, stdout=subprocess.PIPE, stderr=subprocess.PIPE, env=env)
    if result.returncode:
        raise DistributionError(f"{arguments[0]} failed (exit {result.returncode}); no release set was certified")
    return result.stdout


def git(root, *arguments, data=None):
    return run(["git", "-c", "core.hooksPath=/dev/null", "-c", "core.autocrlf=false", "-c", "core.safecrlf=false", "-c", "credential.helper=", "-c", "credential.https://github.com.helper=!gh auth git-credential", *arguments], cwd=root, data=data)


def revision(root, ref):
    return git(root, "rev-parse", "--verify", "--end-of-options", ref + "^{commit}").decode().strip()


def safe_path(path):
    parts = PurePosixPath(path).parts
    if not parts or path.startswith("/") or str(PurePosixPath(path)) != path or any(part in {"..", "."} for part in parts) or "\\" in path or any(ord(char) < 32 for char in path):
        raise DistributionError("unsafe export path")
    if any(part in BLOCKED_PARTS or (part.startswith(".env") and part != ".env.example") for part in parts):
        raise DistributionError(f"private/generated path in export: {path}")
    if PurePosixPath(path).suffix in BLOCKED_SUFFIXES or parts[-1] in {"credentials.json", "auth.json"} or (parts[-1] == ".mcp.json" and path != "integrations/claude-code/.mcp.json"):
        raise DistributionError(f"private/generated file in export: {path}")


def tree(root, commit):
    entries = {}
    for entry in git(root, "ls-tree", "-rz", "--full-tree", commit).split(b"\0"):
        if not entry:
            continue
        header, path = entry.split(b"\t", 1)
        mode, kind, object_id = header.decode().split()
        entries[path.decode()] = (mode, kind, object_id)
    return entries


def blobs(root, object_ids):
    object_ids = list(dict.fromkeys(object_ids))
    result = git(root, "cat-file", "--batch", data=("\n".join(object_ids) + "\n").encode())
    offset = 0
    contents = {}
    for object_id in object_ids:
        end = result.index(b"\n", offset)
        header = result[offset:end].decode().split()
        if len(header) != 3 or header[0] != object_id or header[1] != "blob":
            raise DistributionError("export object is not a blob")
        size = int(header[2])
        if size > 2 * 1024 * 1024:
            raise DistributionError("export file exceeds the 2 MiB public-source limit")
        offset = end + 1
        contents[object_id] = result[offset:offset + size]
        offset += size + 1
    return contents


def config_at(root, commit):
    config = json.loads(git(root, "show", f"{commit}:{CONFIG}"))
    if config.get("version") != 1 or config.get("source") != SOURCE or set(config.get("components", {})) != set(COMPONENTS):
        raise DistributionError("unexpected distribution contract")
    for name, visibility in COMPONENTS.items():
        component = config["components"][name]
        if component["repository"] != f"wnsdy95/cxthub-{name}" or component["visibility"] != visibility:
            raise DistributionError("unexpected distribution destination or visibility")
    return config


def export_files(root, commit, name):
    config = config_at(root, commit)
    entries = tree(root, commit)
    selected = {}
    for source, destination in config["common"] + config["components"][name]["paths"]:
        safe_path(source.rstrip("/"))
        if destination:
            safe_path(destination.rstrip("/"))
        if source.endswith("/") and destination and not destination.endswith("/"):
            raise DistributionError("directory mappings require a directory destination")
        matches = [(path, value) for path, value in entries.items() if path == source or (source.endswith("/") and path.startswith(source))]
        if not matches:
            raise DistributionError(f"missing required export input: {source}")
        for path, (mode, kind, object_id) in matches:
            target = destination + path[len(source):] if source.endswith("/") else destination
            safe_path(path)
            safe_path(target)
            if kind != "blob" or mode not in {"100644", "100755"}:
                raise DistributionError(f"symlink/submodule is not exportable: {path}")
            if target == MANIFEST or target in selected:
                raise DistributionError(f"duplicate/reserved export target: {target}")
            selected[target] = (mode, object_id)
    contents = blobs(root, [entry[1] for entry in selected.values()])
    files = {path: (mode, contents[object_id]) for path, (mode, object_id) in selected.items()}
    manifest = {
        "version": 1,
        "component": name,
        "source_repository": SOURCE,
        "source_commit": commit,
        "source_tree": git(root, "rev-parse", f"{commit}^{{tree}}").decode().strip(),
        "files": {path: {"mode": mode, "sha256": hashlib.sha256(content).hexdigest()} for path, (mode, content) in sorted(files.items())},
    }
    files[MANIFEST] = ("100644", (json.dumps(manifest, indent=2, sort_keys=True) + "\n").encode())
    return files


def write_export(files, destination):
    destination = Path(destination)
    destination.mkdir(parents=True, exist_ok=False)
    for path, (mode, content) in files.items():
        target = destination / path
        target.parent.mkdir(parents=True, exist_ok=True)
        target.write_bytes(content)
        target.chmod(int(mode[-3:], 8))


def remote_files(root, commit):
    entries = tree(root, commit)
    for path, (mode, kind, _) in entries.items():
        safe_path(path)
        if mode not in {"100644", "100755"} or kind != "blob":
            raise DistributionError("unexpected target file type")
    contents = blobs(root, [entry[2] for entry in entries.values()])
    return {path: (mode, contents[object_id]) for path, (mode, _, object_id) in entries.items()}


def remote_head(root, remote):
    refs = git(root, "ls-remote", remote).decode().splitlines()
    main = [line.split()[0] for line in refs if line.endswith("\trefs/heads/main")]
    if refs and not main:
        raise DistributionError("target is not empty but has no main; refusing bootstrap")
    return main[0] if main else None


def check_previous(source, commit, name, files):
    try:
        manifest = json.loads(files[MANIFEST][1])
        previous = manifest["source_commit"]
    except (KeyError, ValueError):
        raise DistributionError("target has no valid provenance; refusing overwrite") from None
    if not re.fullmatch(r"[0-9a-f]{40}", previous):
        raise DistributionError("invalid previous source identity")
    git(source, "merge-base", "--is-ancestor", previous, commit)
    if files != export_files(source, previous, name):
        raise DistributionError("target drift detected; preserve and port edits to the monorepo first")


def publish_component(source, commit, name, remote, files):
    with tempfile.TemporaryDirectory(prefix="cxt-distribution-publish-") as directory:
        checkout = Path(directory) / "checkout"
        write_export(files, checkout)
        git(checkout, "init", "--template=", "-b", "main")
        previous = remote_head(checkout, remote)
        if previous:
            git(checkout, "fetch", "--no-tags", remote, "refs/heads/main")
            fetched = revision(checkout, "FETCH_HEAD")
            if fetched != previous:
                raise DistributionError("target changed during preflight; retry")
            old_files = remote_files(checkout, previous)
            check_previous(source, commit, name, old_files)
            if old_files == files:
                return previous
            git(checkout, "update-ref", "refs/heads/main", previous)
        git(checkout, "add", "-f", "--all")
        git(checkout, "commit", "-m", f"build: synchronize {name} from {SOURCE}@{commit}")
        head = revision(checkout, "HEAD")
        if remote_files(checkout, head) != files:
            raise DistributionError("Git filters changed exported bytes")
        if previous:
            git(checkout, "merge-base", "--is-ancestor", previous, head)
        git(checkout, "push", f"--force-with-lease=refs/heads/main:{previous or ''}", remote, "HEAD:refs/heads/main")
        return head


def github(endpoint, source=False):
    env = os.environ.copy()
    if source and env.get("SOURCE_GITHUB_TOKEN"):
        env["GH_TOKEN"] = env["SOURCE_GITHUB_TOKEN"]
    return json.loads(run(["gh", "api", endpoint], env=env))


def check_source_eligibility(commit):
    head = github(f"repos/{SOURCE}/git/ref/heads/main", source=True)
    if head["object"]["sha"] != commit:
        raise DistributionError("publish requires the current source main commit")
    runs = github(f"repos/{SOURCE}/actions/workflows/ci.yml/runs?head_sha={commit}&event=push&branch=main&per_page=100", source=True)["workflow_runs"]
    runs = [record for record in runs if record["head_sha"] == commit and record["head_branch"] == "main" and record["event"] == "push" and record["head_repository"]["full_name"] == SOURCE]
    if not runs or max(runs, key=lambda record: record["id"])["conclusion"] != "success":
        raise DistributionError("source main has no successful latest CI run")


def check_destination(component):
    info = github("repos/" + component["repository"])
    if info["full_name"] != component["repository"] or info["visibility"] != component["visibility"] or info["archived"] or info["fork"] or info["default_branch"] != "main":
        raise DistributionError("target repository identity/visibility/state mismatch")


def scan_exports(root, commit, exports):
    with tempfile.TemporaryDirectory(prefix="cxt-distribution-scan-") as directory:
        scan_root = Path(directory)
        config = scan_root / "gitleaks.toml"
        config.write_bytes(git(root, "show", f"{commit}:.gitleaks.toml"))
        for name, files in exports.items():
            target = scan_root / name
            write_export(files, target)
            run(["gitleaks", "dir", "--redact", "--no-banner", "--max-decode-depth=2", "--max-archive-depth=1", "--config", str(config), str(target)])


def publish(source, commit):
    check_source_eligibility(commit)
    config = config_at(source, commit)
    exports = {name: export_files(source, commit, name) for name in COMPONENTS}
    for component in config["components"].values():
        check_destination(component)
    scan_exports(source, commit, exports)
    for name, component in config["components"].items():
        remote = "https://github.com/" + component["repository"] + ".git"
        with tempfile.TemporaryDirectory(prefix="cxt-distribution-preflight-") as directory:
            git(directory, "init", "--template=")
            head = remote_head(directory, remote)
            if head:
                git(directory, "fetch", "--no-tags", remote, "refs/heads/main")
                check_previous(source, commit, name, remote_files(directory, revision(directory, "FETCH_HEAD")))
    heads = {}
    for name, component in config["components"].items():
        check_source_eligibility(commit)
        remote = "https://github.com/" + component["repository"] + ".git"
        heads[name] = publish_component(source, commit, name, remote, exports[name])
    check_source_eligibility(commit)
    for name, component in config["components"].items():
        check_destination(component)
        remote = "https://github.com/" + component["repository"] + ".git"
        if remote_head(source, remote) != heads[name]:
            raise DistributionError("target advanced before release-set verification; do not deploy")
    return {"source_repository": SOURCE, "source_commit": commit, "components": {name: {"repository": config["components"][name]["repository"], "commit": head} for name, head in heads.items()}, "status": "published", "deployment": "not_performed", "component_ci": "must_be_checked_before_deployment"}


def verify(source, commit):
    config = config_at(source, commit)
    heads = {}
    for name, component in config["components"].items():
        check_destination(component)
        remote = "https://github.com/" + component["repository"] + ".git"
        with tempfile.TemporaryDirectory(prefix="cxt-distribution-verify-") as directory:
            git(directory, "init", "--template=")
            git(directory, "fetch", "--no-tags", remote, "refs/heads/main")
            head = revision(directory, "FETCH_HEAD")
            if remote_files(directory, head) != export_files(source, commit, name):
                raise DistributionError("component does not match the requested release set")
            runs = github(f"repos/{component['repository']}/actions/workflows/ci.yml/runs?head_sha={head}&event=push&branch=main&per_page=100")["workflow_runs"]
            runs = [record for record in runs if record["head_sha"] == head and record["event"] == "push" and record["head_branch"] == "main" and record["head_repository"]["full_name"] == component["repository"]]
            if not runs or max(runs, key=lambda record: record["id"])["conclusion"] != "success":
                raise DistributionError(f"{name} CI is missing, pending or failed; do not deploy")
            heads[name] = {"repository": component["repository"], "commit": head}
    for component in heads.values():
        if remote_head(source, "https://github.com/" + component["repository"] + ".git") != component["commit"]:
            raise DistributionError("component changed during verification; retry")
    return {"source_commit": commit, "components": heads, "status": "ready_for_manual_deployment", "deployment": "not_performed"}


def main():
    parser = argparse.ArgumentParser(description="Export committed source or publish a CI-verified release set. Never deploys.")
    parser.add_argument("command", choices=["export", "publish", "verify"])
    parser.add_argument("--source", type=Path, default=Path(__file__).resolve().parent.parent)
    parser.add_argument("--ref", required=True)
    parser.add_argument("--component", choices=COMPONENTS)
    parser.add_argument("--output", type=Path)
    args = parser.parse_args()
    commit = revision(args.source, args.ref)
    if args.command == "export":
        if not args.component or not args.output:
            parser.error("export requires --component and a new --output directory")
        write_export(export_files(args.source, commit, args.component), args.output)
        print(json.dumps({"component": args.component, "source_commit": commit, "output": str(args.output)}))
    else:
        if args.component or args.output:
            parser.error("publish/verify operate on all components and emit the release set to stdout")
        operation = publish if args.command == "publish" else verify
        print(json.dumps(operation(args.source, commit), indent=2))


if __name__ == "__main__":
    try:
        main()
    except (DistributionError, OSError, ValueError, KeyError) as error:
        sys.exit(f"[distribution] {error}")
