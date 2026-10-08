#!/usr/bin/env python3
"""Manage independent local API/MCP launch agents; never migrate or delete data."""
import argparse
import ipaddress
import json
import os
from pathlib import Path
import plistlib
import re
import socket
import stat
import subprocess
import sys
import tempfile
import time
import urllib.error
import urllib.parse
import urllib.request

SERVICES = (("mcp", "com.cxthub.mcp", 8908), ("api", "com.cxthub.api", 8907))
MCP_KEYS = {"CXT_AUTH", "CXT_FIREBASE_PROJECT", "CXT_PUBLIC_URL", "CXT_POSTGRES_DSN"}


class ConfigurationError(Exception):
    """Operator-safe diagnostic, never includes credential values."""


def loopback(host):
    if host == "localhost":
        return True
    try:
        return ipaddress.ip_address(host).is_loopback
    except ValueError:
        return False


def load_config(path):
    fd = os.open(path, os.O_RDONLY | os.O_NOFOLLOW)
    with os.fdopen(fd) as source:
        info = os.fstat(source.fileno())
        if not stat.S_ISREG(info.st_mode) or info.st_uid != os.getuid() or info.st_mode & 0o077:
            raise ConfigurationError("Configuration must be an owner-only regular file")
        cfg = json.load(source)
    env = cfg.get("environment", {})
    if not isinstance(env, dict) or any(not isinstance(k, str) or not isinstance(v, str) or "\0" in k + v for k, v in env.items()):
        raise ConfigurationError("Invalid environment configuration")
    if any(not k.startswith("CXT_") and k != "RESEND_API_KEY" for k in env):
        raise ConfigurationError("Only server configuration variables are accepted")
    try:
        dsn = urllib.parse.urlsplit(env.get("CXT_POSTGRES_DSN", ""))
        options = urllib.parse.parse_qs(dsn.query, strict_parsing=True)
        valid = dsn.scheme in ("postgres", "postgresql") and loopback(dsn.hostname) and dsn.path not in ("", "/", "/postgres") and dsn.port is not None and not dsn.fragment
        valid = valid and set(options) <= {"sslmode", "connect_timeout", "application_name"}
    except (ValueError, TypeError):
        valid = False
    if not valid:
        raise ConfigurationError("A named loopback PostgreSQL database with an explicit port is required; connection routing overrides are forbidden")
    origin = urllib.parse.urlsplit(env.get("CXT_PUBLIC_URL", ""))
    if origin.scheme not in ("http", "https") or not loopback(origin.hostname) or origin.username or origin.path or origin.query or origin.fragment:
        raise ConfigurationError("CXT_PUBLIC_URL must be the loopback frontend origin")
    if env.get("CXT_AUTH") not in ("firebase", "dev"):
        raise ConfigurationError("Set CXT_AUTH explicitly; authentication must not silently change")
    if env["CXT_AUTH"] == "firebase" and not env.get("CXT_FIREBASE_PROJECT"):
        raise ConfigurationError("Firebase project is required")
    for key in ("root", "runtime", "api_binary", "mcp_binary"):
        value = cfg.get(key, "")
        if not isinstance(value, str) or not Path(value).is_absolute():
            raise ConfigurationError("Root, runtime and binary paths must be absolute")
    if not (Path(cfg["root"]) / "schemas/db/migrations").is_dir():
        raise ConfigurationError("Repository migrations are missing")
    for key in ("api_binary", "mcp_binary"):
        if not os.access(cfg[key], os.X_OK) or not Path(cfg[key]).is_file():
            raise ConfigurationError("An executable service binary is missing")
    return cfg


def service_plists(cfg):
    base = dict(cfg["environment"])
    migrations = str(Path(cfg["root"]) / "schemas/db/migrations")
    result = {}
    for kind, label, port in SERVICES:
        env = dict(base) if kind == "api" else {k: v for k, v in base.items() if k in MCP_KEYS}
        env["CXT_MIGRATIONS_DIR"] = migrations
        # MCP must not load the API's GitHub, Resend or other write credentials.
        if kind == "mcp":
            env["CXT_ENV_FILE"] = "/dev/null"
        else:
            env.setdefault("CXT_ENV_FILE", "/dev/null")
        logs = Path(cfg["runtime"]) / "logs"
        result[label] = {
            "Label": label,
            "ProgramArguments": [cfg[kind + "_binary"], "serve", "--addr", f"127.0.0.1:{port}"],
            "EnvironmentVariables": env,
            "WorkingDirectory": cfg["root"], "RunAtLoad": True, "KeepAlive": True,
            "StandardOutPath": str(logs / (kind + ".out.log")),
            "StandardErrorPath": str(logs / (kind + ".err.log")),
            # API/MCP serve user requests; retain launchd's default Standard
            # scheduling instead of forcing background CPU and I/O limits.
            "ExitTimeOut": 20,
        }
    return result


def atomic_private(path, raw):
    path.parent.mkdir(parents=True, exist_ok=True)
    if path.is_symlink():
        raise ConfigurationError("Refusing to replace a symlink")
    fd, temporary = tempfile.mkstemp(prefix=".cxthub-", dir=path.parent)
    try:
        with os.fdopen(fd, "wb") as output:
            output.write(raw)
            output.flush()
            os.fsync(output.fileno())
        os.replace(temporary, path)
        fd = os.open(path.parent, os.O_RDONLY)
        try:
            os.fsync(fd)
        finally:
            os.close(fd)
    finally:
        if os.path.exists(temporary):
            os.unlink(temporary)


def launch(*args, check=True):
    result = subprocess.run(["launchctl", *args], capture_output=True)
    if check and result.returncode:
        # launchctl print may contain credentials: never echo its output.
        raise ConfigurationError("launchctl operation failed; service data and private logs were preserved")
    return result.returncode == 0


def service_listens(gui, label, port):
    result = subprocess.run(["launchctl", "print", gui + "/" + label], capture_output=True)
    if result.returncode:
        return False
    pid = re.search(rb"^\s*pid = ([0-9]+)\s*$", result.stdout, re.MULTILINE)
    if not pid:
        return False
    # A response from an unrelated process racing for the same port must not
    # be mistaken for this launch agent's readiness.
    owner = subprocess.run(["/usr/sbin/lsof", "-nP", "-a", "-p", pid[1].decode(),
                            "-iTCP:" + str(port), "-sTCP:LISTEN", "-t"], capture_output=True)
    return owner.returncode == 0 and pid[1] in owner.stdout.splitlines()


def port_available(port):
    # Match the servers' restart semantics: closed accepted connections may
    # remain in TIME_WAIT, but a live listener must still prevent startup.
    try:
        with socket.socket() as probe:
            probe.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1)
            probe.bind(("127.0.0.1", port))
            probe.listen(1)
        return True
    except OSError:
        return False


def wait_stopped(gui, services):
    # bootout acknowledges the request before launchd has finished unloading.
    # Do not return success while an old worker or its listener can still run.
    deadline = time.monotonic() + 30
    while True:
        pending = [label for label, port in services
                   if launch("print", gui + "/" + label, check=False) or not port_available(port)]
        if not pending:
            return
        if time.monotonic() >= deadline:
            raise ConfigurationError("Service shutdown is still in progress or its port is occupied; no data was changed. Check status before restarting")
        time.sleep(0.2)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("action", choices=("check", "configure", "start", "stop", "status"))
    parser.add_argument("--config", type=Path, required=True, help="private JSON operator configuration")
    args = parser.parse_args()
    cfg = load_config(args.config)
    plists = service_plists(cfg)
    agent_dir = Path.home() / "Library/LaunchAgents"
    gui = f"gui/{os.getuid()}"
    if args.action == "check":
        print("Configuration valid: independent loopback API/MCP, shared PostgreSQL and explicit authentication. No changes made.")
        return
    if sys.platform != "darwin":
        raise ConfigurationError("This launcher requires macOS launchd; use deploy/RENDER.md elsewhere")
    loaded = {label: launch("print", gui + "/" + label, check=False) for label in plists}
    if args.action == "configure":
        if any(loaded.values()):
            raise ConfigurationError("Stop the independent services before replacing their configuration")
        runtime = Path(cfg["runtime"])
        runtime.mkdir(parents=True, mode=0o700, exist_ok=True)
        if runtime.is_symlink() or runtime.stat().st_uid != os.getuid() or runtime.stat().st_mode & 0o077:
            raise ConfigurationError("Runtime directory must be private and owned by the current user")
        logs = runtime / "logs"
        logs.mkdir(mode=0o700, exist_ok=True)
        if logs.is_symlink() or logs.stat().st_uid != os.getuid() or logs.stat().st_mode & 0o077:
            raise ConfigurationError("Log directory must be private and must not be a symlink")
        for label, contents in plists.items():
            atomic_private(agent_dir / (label + ".plist"), plistlib.dumps(contents))
        print("Private API/MCP launch agents configured. Existing FS daemon and database unchanged.")
    elif args.action == "start":
        if any(loaded.values()):
            raise ConfigurationError("Independent services are already loaded; use status or stop first")
        for _, label, port in SERVICES:
            path = agent_dir / (label + ".plist")
            if path.is_symlink() or path.stat().st_mode & 0o077 or plistlib.loads(path.read_bytes()) != plists[label]:
                raise ConfigurationError("Private launch-agent configuration differs; run configure before start")
            if not port_available(port):
                raise ConfigurationError("An API/MCP port has a live listener; stop it before starting these services")
        started = []
        try:
            for _, label, _ in SERVICES:
                launch("bootstrap", gui, str(agent_dir / (label + ".plist")))
                started.append(label)
            deadline = time.monotonic() + 30
            waiting = {port: label for _, label, port in SERVICES}
            opener = urllib.request.build_opener(urllib.request.ProxyHandler({}))
            while waiting and time.monotonic() < deadline:
                for port in list(waiting):
                    try:
                        if not service_listens(gui, waiting[port], port):
                            continue
                        with opener.open(f"http://127.0.0.1:{port}/healthz", timeout=1) as response:
                            if response.status == 200:
                                del waiting[port]
                    except (OSError, urllib.error.URLError):
                        pass
                if waiting:
                    time.sleep(0.2)
            if waiting:
                raise ConfigurationError("Readiness failed; private logs retained")
        except Exception:
            for label in reversed(started):
                launch("bootout", gui + "/" + label, check=False)
            wait_stopped(gui, [(label, port) for _, label, port in SERVICES if label in started])
            raise
        print("API :8907 and MCP :8908 are ready against PostgreSQL.")
    elif args.action == "stop":
        stopping = []
        for _, label, port in reversed(SERVICES):
            if loaded[label]:
                launch("bootout", gui + "/" + label)
                stopping.append((label, port))
        wait_stopped(gui, stopping)
        print("Independent services stopped. PostgreSQL, FS backups and provider sessions preserved.")
    else:
        for kind, label, _ in SERVICES:
            print(kind + ": " + ("loaded" if loaded[label] else "not loaded"))


if __name__ == "__main__":
    try:
        main()
    except ConfigurationError as error:
        print("Local services: " + str(error), file=sys.stderr)
        sys.exit(1)
    except (ValueError, OSError, RuntimeError) as error:
        # Deliberately do not print arbitrary exception bodies or configuration.
        print("Local services operation failed (" + type(error).__name__ + "). Check the private configuration, available ports and service logs; no data was deleted.", file=sys.stderr)
        sys.exit(1)
