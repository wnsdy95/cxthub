#!/usr/bin/env python3
"""Validate the product's deployment contract without contacting Render."""
import pathlib
import sys

try:
    import yaml
except ImportError:
    sys.exit("Install deploy/requirements-preflight.txt in a venv and set CXT_PREFLIGHT_PYTHON to its python")

root = pathlib.Path(__file__).resolve().parents[1]
blueprint = yaml.safe_load((root / "render.yaml").read_text())
services = blueprint["services"]
assert len(services) == 2, "Exactly two independent services required"
by_name = {s["name"]: s for s in services}
assert set(by_name) == {"cxthub-api", "cxthub-mcp"}
assert len(blueprint["databases"]) == 1
db = blueprint["databases"][0]
assert db["name"] == "cxthub-postgres" and db["ipAllowList"] == []
for name, port, dockerfile in (
    ("cxthub-api", "8907", "./deploy/Dockerfile"),
    ("cxthub-mcp", "8908", "./deploy/Dockerfile.mcp"),
):
    service = by_name[name]
    assert service["runtime"] == "docker" and service["type"] == "web"
    assert service["plan"] != "free", "Workers must not sleep"
    assert service["region"] == db["region"]
    assert service["dockerfilePath"] == dockerfile and service["dockerContext"] == "."
    assert service["healthCheckPath"] == "/healthz"
    env = {e["key"]: e for e in service["envVars"]}
    assert len(env) == len(service["envVars"]), "Duplicate environment keys"
    assert env["PORT"]["value"] == port and env["CXT_AUTH"]["value"] == "firebase"
    assert env["CXT_POSTGRES_DSN"]["fromDatabase"] == {
        "name": db["name"], "property": "connectionString"
    }
    for key in ("CXT_PUBLIC_URL", "CXT_FIREBASE_PROJECT"):
        if name == "cxthub-api":
            assert env[key] == {"key": key, "sync": False}
        else:
            assert env[key]["fromService"] == {
                "type": "web", "name": "cxthub-api", "envVarKey": key
            }
    if name == "cxthub-mcp":
        assert set(env) == {"PORT", "CXT_AUTH", "CXT_PUBLIC_URL", "CXT_FIREBASE_PROJECT", "CXT_POSTGRES_DSN"}
print("  ✓ Render service isolation, shared database, health and identity configuration")
