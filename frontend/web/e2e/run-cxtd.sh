#!/usr/bin/env bash
set -euo pipefail

web_root="$(cd "$(dirname "$0")/.." && pwd)"
repo_root="$(cd "$web_root/../.." && pwd)"
data_root="$(mktemp -d)"
server_pid=""

cleanup() {
  if [ -n "$server_pid" ]; then
    kill "$server_pid" 2>/dev/null || true
    wait "$server_pid" 2>/dev/null || true
  fi
  rm -rf "$data_root"
}
trap cleanup EXIT INT TERM

cd "$repo_root/backend"
CXT_AUTH=dev CXT_PUBLIC_URL=http://127.0.0.1:4174 go run ./cmd/cxtd serve \
  --addr 127.0.0.1:18907 \
  --data "$data_root/store" &
server_pid=$!
wait "$server_pid"
