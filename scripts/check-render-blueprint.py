#!/usr/bin/env python3
"""Validate the product's deployment contract without contacting Render."""
import pathlib
import re
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

def memory_mib(plan):
    legacy = {"starter": 512, "standard": 2048, "pro": 4096,
              "pro_plus": 8192, "pro_max": 16384, "pro_ultra": 32768}
    if plan in legacy:
        return legacy[plan]
    match = re.fullmatch(r"[0-9.]+c-([0-9]+)(g|mb)", plan)
    assert match, "Unknown compute plan; review its memory specification"
    return int(match[1]) * (1024 if match[2] == "g" else 1)

for name, port, dockerfile in (
    ("cxthub-api", "8907", "./deploy/Dockerfile"),
    ("cxthub-mcp", "8908", "./deploy/Dockerfile.mcp"),
):
    service = by_name[name]
    assert service["runtime"] == "docker" and service["type"] == "web"
    assert service["plan"] != "free", "Workers must not sleep"
    required = 4096 if name == "cxthub-api" else 2048
    assert memory_mib(service["plan"]) >= required, "Plan is below the configured paid-profile memory budget"
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
    if name == "cxthub-api":
        assert env["CXT_COOKIE_SECURE"] == {"key": "CXT_COOKIE_SECURE", "value": "1"}
        assert env["CXT_CORS_ORIGINS"] == {
            "key": "CXT_CORS_ORIGINS",
            "fromService": {"type": "web", "name": "cxthub-api", "envVarKey": "CXT_PUBLIC_URL"},
        }, "API must trust the exact public origin used by the Vercel proxy"
    if name == "cxthub-mcp":
        assert env["CXT_MCP_PUBLIC_URL"] == {"key": "CXT_MCP_PUBLIC_URL", "sync": False}
        assert set(env) == {"PORT", "CXT_AUTH", "CXT_PUBLIC_URL", "CXT_MCP_PUBLIC_URL", "CXT_FIREBASE_PROJECT", "CXT_POSTGRES_DSN"}
distribution = yaml.safe_load((root / "deploy/render-repositories.yaml").read_text())
assert set(distribution) == set(blueprint), "Split Blueprint changed top-level resources"
assert len(distribution["databases"]) == 1
distributed_db = dict(distribution["databases"][0])
assert distributed_db.pop("diskSizeGB") == 20, "Budget deployment requires the approved 20 GB disk"
assert distributed_db.pop("storageAutoscalingEnabled") is False, "Storage growth requires separate approval"
assert distributed_db["plan"] == "0.1c-256mb", "Budget deployment requires the approved 256 MB database"
assert distributed_db == db, "Split services must retain database identity and network isolation"
assert len(distribution["services"]) == len(services)
distributed_services = {service["name"]: service for service in distribution["services"]}
assert set(distributed_services) == set(by_name)
for name, component in (("cxthub-api", "backend"), ("cxthub-mcp", "mcp")):
    actual = dict(distributed_services[name])
    assert actual.pop("repo") == f"https://github.com/wnsdy95/cxthub-{component}"
    assert actual.pop("branch") == "main"
    assert actual.pop("autoDeployTrigger") == "off", "Partial publication must never deploy automatically"
    assert actual["plan"] == "free", "Budget deployment must not provision paid API or MCP services"
    actual["plan"] = by_name[name]["plan"]
    assert actual["dockerfilePath"] == "./Dockerfile"
    actual["dockerfilePath"] = by_name[name]["dockerfilePath"]
    assert actual == by_name[name], "Split deployment changed the source environment, identity, or service contract"

print("  ✓ Render isolation, shared database, identity, secure cookies, proxy origin and budget/manual-only repository handoff")
