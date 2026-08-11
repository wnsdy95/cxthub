#!/usr/bin/env bash
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT
bin="$tmp/bin"
mkdir -p "$bin"
log="$tmp/gh.log"
state="$tmp/state"

cat >"$bin/curl" <<'SH'
#!/usr/bin/env bash
case " $* " in
  *" --write-out "*) printf '%s' "${TEST_HTTP_STATUS:-200}" ;;
  *) printf '%s\n' "${TEST_HEALTH_BODY:-{\"status\":\"ok\"}}" ;;
esac
SH
cat >"$bin/gcloud" <<'SH'
#!/usr/bin/env bash
case " $* " in
  *" versions list "*) printf '7\n' ;;
  *" versions access "*) printf 'test-signing-secret\n' ;;
  *) exit 2 ;;
esac
SH
cat >"$bin/sleep" <<'SH'
#!/usr/bin/env bash
exit 0
SH
cat >"$bin/gh" <<'SH'
#!/usr/bin/env bash
printf '%q ' "$@" >>"$TEST_GH_LOG"
printf '\n' >>"$TEST_GH_LOG"
if [ "$1" = repo ]; then
  printf 'acme/project\n'
  exit
fi
path=""
for arg in "$@"; do
  case "$arg" in repos/*) path="$arg" ;; esac
done
case "$path" in
  'repos/acme/project/hooks?per_page=100')
    if [ -s "$TEST_HOOK_STATE" ]; then
      printf '[{"id":123,"active":true,"events":["pull_request"],"config":{"url":"https://example.com/api/v1/hooks/github","content_type":"json","insecure_ssl":"0"}}]\n'
    else
      printf '[]\n'
    fi
    ;;
  'repos/acme/project/hooks')
    printf x >"$TEST_HOOK_STATE"
    printf '123\n'
    ;;
  'repos/acme/project/hooks/123')
    printf '{"id":123,"active":true,"events":["pull_request"],"config":{"url":"https://example.com/api/v1/hooks/github","content_type":"json","insecure_ssl":"0"}}\n'
    ;;
  'repos/acme/project/hooks/123/pings') printf '{}\n' ;;
  'repos/acme/project/hooks/123/deliveries?per_page=10') printf '204\n' ;;
  *) printf 'unexpected gh path: %s\n' "$path" >&2; exit 2 ;;
esac
SH
chmod +x "$bin"/* "$ROOT/scripts/github-webhook.sh"

run_webhook() {
  PATH="$bin:$PATH" TEST_GH_LOG="$log" TEST_HOOK_STATE="$state" \
    TF_VAR_gcp_project=test-project TF_VAR_domain=example.com \
    CXT_GITHUB_REPOSITORY=acme/project \
    "$ROOT/scripts/github-webhook.sh" "$@"
}

: >"$log"
if TEST_HTTP_STATUS=404 run_webhook apply >"$tmp/out" 2>"$tmp/err"; then
  echo "health failure unexpectedly succeeded" >&2
  exit 1
fi
grep -q 'health check failed' "$tmp/err"
[ ! -s "$log" ] || { echo "health failure mutated GitHub" >&2; exit 1; }

: >"$log"
if TEST_HEALTH_BODY='{"status":"wrong"}' run_webhook apply >"$tmp/out" 2>"$tmp/err"; then
  echo "invalid health body unexpectedly succeeded" >&2
  exit 1
fi
grep -q 'not cxtd status=ok' "$tmp/err"
[ ! -s "$log" ] || { echo "invalid health body mutated GitHub" >&2; exit 1; }

: >"$log"
if run_webhook check >"$tmp/out" 2>"$tmp/err"; then
  echo "missing hook check unexpectedly succeeded" >&2
  exit 1
fi
grep -q 'not registered' "$tmp/err"
! grep -q -- '--method' "$log"

: >"$log"
run_webhook apply >"$tmp/out" 2>"$tmp/err"
grep -q 'Created GitHub webhook 123' "$tmp/out"
grep -q 'Signed GitHub ping delivery returned HTTP 204' "$tmp/out"
grep -q -- '--method POST repos/acme/project/hooks ' "$log"
grep -q -- '--method POST repos/acme/project/hooks/123/pings ' "$log"
! grep -R -q 'test-signing-secret' "$tmp/out" "$tmp/err" "$log"

: >"$log"
run_webhook apply >"$tmp/out" 2>"$tmp/err"
grep -q 'Reconciled GitHub webhook 123' "$tmp/out"
grep -q -- '--method PATCH repos/acme/project/hooks/123 ' "$log"
! grep -R -q 'test-signing-secret' "$tmp/out" "$tmp/err" "$log"

: >"$log"
run_webhook check >"$tmp/out" 2>"$tmp/err"
grep -q 'active and correctly configured' "$tmp/out"
! grep -q -- '--method' "$log"

printf '  ✓ GitHub webhook operator check/apply fail-closed tests\n'
