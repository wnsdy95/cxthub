#!/usr/bin/env bash
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT

mkdir -p "$TMP/repo/scripts" "$TMP/repo/frontend/web" "$TMP/home"
cp "$ROOT/scripts/dogfood-daemon.sh" "$TMP/repo/scripts/dogfood-daemon.sh"

run_config() {
  env -u CXT_AUTH -u CXT_FIREBASE_PROJECT HOME="$TMP/home" \
    bash "$TMP/repo/scripts/dogfood-daemon.sh" config
}

assert_line() {
  local output="$1" expected="$2"
  if ! grep -Fxq "$expected" <<<"$output"; then
    echo "missing '$expected' in:" >&2
    echo "$output" >&2
    exit 1
  fi
}

out="$(run_config)"
assert_line "$out" "auth=dev"
assert_line "$out" "firebase_project="
assert_line "$out" "source=default"

printf 'VITE_FIREBASE_API_KEY=local-key\n' > "$TMP/repo/frontend/web/.env"
if run_config >/dev/null 2>&1; then
  echo "partial Firebase web config unexpectedly succeeded" >&2
  exit 1
fi

printf 'VITE_FIREBASE_PROJECT_ID=from-base-1\n' > "$TMP/repo/frontend/web/.env"
out="$(run_config)"
assert_line "$out" "auth=dev"
assert_line "$out" "firebase_project="

printf 'VITE_FIREBASE_API_KEY=local-key\nVITE_FIREBASE_PROJECT_ID=from-base-1\n' > "$TMP/repo/frontend/web/.env"
out="$(run_config)"
assert_line "$out" "auth=firebase"
assert_line "$out" "firebase_project=from-base-1"
assert_line "$out" "source=Vite development env"

printf 'export VITE_FIREBASE_PROJECT_ID="from-local-2" # local override\r\n' > "$TMP/repo/frontend/web/.env.development.local"
out="$(run_config)"
assert_line "$out" "firebase_project=from-local-2"

out="$(VITE_FIREBASE_API_KEY=process-key VITE_FIREBASE_PROJECT_ID=from-process-3 \
  run_config)"
assert_line "$out" "auth=firebase"
assert_line "$out" "firebase_project=from-process-3"

out="$(CXT_AUTH=dev HOME="$TMP/home" bash "$TMP/repo/scripts/dogfood-daemon.sh" config)"
assert_line "$out" "auth=dev"
assert_line "$out" "firebase_project="
assert_line "$out" "source=CXT_AUTH"

out="$(CXT_AUTH=firebase CXT_FIREBASE_PROJECT=explicit-project-3 HOME="$TMP/home" \
  bash "$TMP/repo/scripts/dogfood-daemon.sh" config)"
assert_line "$out" "auth=firebase"
assert_line "$out" "firebase_project=explicit-project-3"
assert_line "$out" "source=CXT_AUTH"

if CXT_AUTH=firebase CXT_FIREBASE_PROJECT=bad HOME="$TMP/home" \
  bash "$TMP/repo/scripts/dogfood-daemon.sh" config >/dev/null 2>&1; then
  echo "invalid Firebase project unexpectedly succeeded" >&2
  exit 1
fi

mkdir -p "$TMP/bin"
printf '#!/bin/sh\nexit 0\n' > "$TMP/bin/launchctl"
chmod +x "$TMP/bin/launchctl"
run_restart() {
  PATH="$TMP/bin:$PATH" CXT_AUTH=dev HOME="$TMP/home" \
    bash "$TMP/repo/scripts/dogfood-daemon.sh" restart
}
out="$(CXT_GITHUB_TOKEN='test-only<&credential' run_restart)"
if [[ "$out" == *credential* ]]; then
  echo "daemon output exposed its credential" >&2
  exit 1
fi
# A normal restart must retain the configured token, without consulting a
# developer keychain or changing cloud-server authentication behavior.
unset CXT_GITHUB_TOKEN
run_restart >/dev/null
python3 - "$TMP/home/Library/LaunchAgents/com.cxthub.cxtd.plist" <<'PY'
import os, plistlib, stat, sys
p = sys.argv[1]
with open(p, 'rb') as source:
    config = plistlib.load(source)
assert config['EnvironmentVariables']['CXT_GITHUB_TOKEN'] == 'test-only<&credential', 'credential not preserved'
assert stat.S_IMODE(os.stat(p).st_mode) == 0o600, 'launch agent must be private'
assert not any('credential' in a for a in config['ProgramArguments']), 'credential in argv'
PY
CXT_GITHUB_TOKEN='' run_restart >/dev/null
python3 - "$TMP/home/Library/LaunchAgents/com.cxthub.cxtd.plist" <<'PY'
import plistlib, sys
with open(sys.argv[1], 'rb') as source:
    assert 'CXT_GITHUB_TOKEN' not in plistlib.load(source)['EnvironmentVariables'], 'explicit clear was ignored'
PY

echo "dogfood daemon auth resolution and credential persistence: passed"
