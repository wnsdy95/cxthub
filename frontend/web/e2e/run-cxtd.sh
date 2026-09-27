#!/usr/bin/env bash
set -euo pipefail

# Full-stack tests own two real processes and a dedicated PostgreSQL database.
# Never point CXT_E2E_DSN at a development or production database.
: "${CXT_E2E_DSN:?Set CXT_E2E_DSN to a disposable PostgreSQL database}"
web_root="$(cd "$(dirname "$0")/.." && pwd)"
repo_root="$(cd "$web_root/../.." && pwd)"
build_root="$(mktemp -d)"
api_pid=""
mcp_pid=""
cleanup() {
  for pid in "$api_pid" "$mcp_pid"; do
    if [ -n "$pid" ]; then kill "$pid" 2>/dev/null || true; fi
  done
  for pid in "$api_pid" "$mcp_pid"; do
    if [ -n "$pid" ]; then wait "$pid" 2>/dev/null || true; fi
  done
  rm -rf "$build_root"
}
trap cleanup EXIT
trap 'exit 130' INT TERM

cd "$repo_root/backend"
go build -tags postgres -o "$build_root/cxtd" ./cmd/cxtd
go build -tags postgres -o "$build_root/cxt-mcp" ./cmd/cxt-mcp
export CXT_POSTGRES_DSN="$CXT_E2E_DSN" CXT_REQUIRE_POSTGRES=1
export RESEND_API_KEY= CXT_ENV_FILE=/dev/null CXT_AUTH=dev
export CXT_PUBLIC_URL=http://127.0.0.1:4174
export CXT_MIGRATIONS_DIR="$repo_root/schemas/db/migrations"
# MCP starts first; API readiness means the browser can use both services.
"$build_root/cxt-mcp" serve --addr 127.0.0.1:18908 &
mcp_pid=$!
ready=0
for ((i=0; i<300; i++)); do
  kill -0 "$mcp_pid" 2>/dev/null || exit 1
  if curl -fsS --max-time 2 http://127.0.0.1:18908/healthz >/dev/null 2>&1; then ready=1; break; fi
  sleep 0.1
done
[ "$ready" = 1 ] || exit 1
"$build_root/cxtd" serve --addr 127.0.0.1:18907 &
api_pid=$!
wait "$api_pid"
