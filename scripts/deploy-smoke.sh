#!/usr/bin/env bash
# Smoke test for the same-origin path after deployment. Does not create or modify data/accounts.
set -euo pipefail

DOMAIN="${1:-}"
[ -n "$DOMAIN" ] || { echo "Usage: $0 <domain>" >&2; exit 2; }
[[ "$DOMAIN" =~ ^[a-z0-9][a-z0-9.-]+[a-z0-9]$ ]] || { echo "Invalid domain: $DOMAIN" >&2; exit 2; }

BASE="https://$DOMAIN"
TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT

curl --fail --silent --show-error --proto '=https' --tlsv1.2 \
  --dump-header "$TMP/health.headers" --output "$TMP/health.json" \
  "$BASE/api/v1/health"

python3 - "$TMP/health.json" <<'PY'
import json, pathlib, sys
body = json.loads(pathlib.Path(sys.argv[1]).read_text())
if body != {"status": "ok"}:
    raise SystemExit(f"unexpected health body: {body!r}")
PY

grep -Eiq '^x-content-type-options:[[:space:]]*nosniff' "$TMP/health.headers" \
  || { echo "No X-Content-Type-Options in health response" >&2; exit 1; }

code="$(curl --silent --show-error --proto '=https' --tlsv1.2 \
  --output "$TMP/me.json" --write-out '%{http_code}' "$BASE/api/v1/me")"
[ "$code" = "401" ] || { echo "Unauthenticated /me status=$code (want 401)" >&2; exit 1; }

curl --fail --silent --show-error --proto '=https' --tlsv1.2 \
  --output "$TMP/index.html" "$BASE/"
grep -qi '<div id="root"' "$TMP/index.html" \
  || { echo "Web root is not the expected SPA HTML" >&2; exit 1; }

echo "✓ $BASE: web · same-origin API · public health · auth boundary"
