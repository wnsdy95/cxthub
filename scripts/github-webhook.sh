#!/usr/bin/env bash
# Verify or reconcile the signed GitHub pull-request webhook used by cxtd.
# The secret is read into a mode-0700 temporary directory and is never printed.
set -euo pipefail

MODE="${1:-check}"
case "$MODE" in check|apply) ;; *) echo "usage: $0 [check|apply]" >&2; exit 2 ;; esac

die() { printf '  ✗ %s\n' "$1" >&2; exit 1; }
pass() { printf '  ✓ %s\n' "$1"; }
need() { command -v "$1" >/dev/null 2>&1 || die "Missing required command: $1"; }
require_env() { [ -n "${!1:-}" ] || die "Missing required environment variable: $1"; }

for cmd in curl gcloud gh jq python3; do need "$cmd"; done
require_env TF_VAR_gcp_project
require_env TF_VAR_domain

PROJECT="$TF_VAR_gcp_project"
DOMAIN="$TF_VAR_domain"
SECRET_ID="${TF_VAR_github_webhook_secret_id:-cxt-github-webhook-secret}"
WEBHOOK_URL="https://${DOMAIN}/api/v1/hooks/github"
HEALTH_URL="https://${DOMAIN}/api/v1/health"
REPO="${CXT_GITHUB_REPOSITORY:-$(gh repo view --json nameWithOwner --jq .nameWithOwner)}"

case "$DOMAIN" in
  *[!a-z0-9.-]*|.*|*..*|*.) die "TF_VAR_domain is not a valid lowercase DNS name" ;;
esac
[[ "$PROJECT" =~ ^[a-z][a-z0-9-]{4,28}[a-z0-9]$ ]] || die "TF_VAR_gcp_project is invalid"
[[ "$SECRET_ID" =~ ^[A-Za-z0-9_-]{1,255}$ ]] || die "TF_VAR_github_webhook_secret_id is invalid"
[[ "$REPO" =~ ^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$ ]] || die "CXT_GITHUB_REPOSITORY must be owner/repo"

tmp="$(mktemp -d)"
chmod 700 "$tmp"
trap 'rm -rf "$tmp"' EXIT
health="$tmp/health.json"
status="$(curl --silent --show-error --output /dev/null --write-out '%{http_code}' \
  --max-time 10 "$HEALTH_URL")"
[ "$status" = "200" ] || die "API health check failed: $HEALTH_URL returned HTTP $status"
curl --silent --show-error --fail --max-time 10 "$HEALTH_URL" >"$health"
jq -e '.status == "ok"' "$health" >/dev/null || die "API health response is not cxtd status=ok"
pass "Public API health endpoint returned 200"

version="$(gcloud secrets versions list "$SECRET_ID" --project "$PROJECT" \
  --filter='state=ENABLED' --limit=1 --format='value(name)')"
[ -n "$version" ] || die "No ENABLED version in Secret Manager secret $SECRET_ID"
pass "GitHub webhook secret has an enabled version"

hooks="$tmp/hooks.json"
gh api "repos/$REPO/hooks?per_page=100" >"$hooks"

matches="$(jq --arg url "$WEBHOOK_URL" '[.[] | select(.config.url == $url)] | length' "$hooks")"
[ "$matches" -le 1 ] || die "Multiple GitHub webhooks use $WEBHOOK_URL; reconcile manually"

hook_id="$(jq -r --arg url "$WEBHOOK_URL" '.[] | select(.config.url == $url) | .id' "$hooks")"
valid=false
if [ -n "$hook_id" ]; then
  valid="$(jq -r --arg url "$WEBHOOK_URL" '.[] | select(.config.url == $url) |
    (.active == true and .config.content_type == "json" and .config.insecure_ssl == "0" and
     (.events | sort) == ["pull_request"])' "$hooks")"
fi

if [ "$MODE" = check ]; then
  [ -n "$hook_id" ] || die "GitHub pull-request webhook is not registered for $REPO"
  [ "$valid" = true ] || die "GitHub webhook $hook_id has drifted from the required configuration"
  pass "GitHub webhook $hook_id is active and correctly configured"
  exit 0
fi

secret_file="$tmp/secret"
payload="$tmp/payload.json"
gcloud secrets versions access latest --secret "$SECRET_ID" --project "$PROJECT" >"$secret_file"
[ -s "$secret_file" ] || die "Webhook secret version is empty"
python3 - "$secret_file" "$payload" "$WEBHOOK_URL" <<'PY'
import json, pathlib, sys
secret_path = pathlib.Path(sys.argv[1])
payload_path = pathlib.Path(sys.argv[2])
url = sys.argv[3]
secret = secret_path.read_text().rstrip("\r\n")
if not secret:
    raise SystemExit("webhook secret is empty")
payload_path.write_text(json.dumps({
    "name": "web", "active": True, "events": ["pull_request"],
    "config": {"url": url, "content_type": "json", "secret": secret, "insecure_ssl": "0"},
}))
secret_path.write_text("")
PY

if [ -n "$hook_id" ]; then
  gh api --method PATCH "repos/$REPO/hooks/$hook_id" --input "$payload" >/dev/null
  pass "Reconciled GitHub webhook $hook_id"
else
  hook_id="$(gh api --method POST "repos/$REPO/hooks" --input "$payload" --jq .id)"
  [ -n "$hook_id" ] || die "GitHub webhook creation returned no id"
  pass "Created GitHub webhook $hook_id"
fi

gh api "repos/$REPO/hooks/$hook_id" >"$tmp/result.json"
jq -e --arg url "$WEBHOOK_URL" '
  .active == true and .config.url == $url and .config.content_type == "json" and
  .config.insecure_ssl == "0" and (.events | sort) == ["pull_request"]
' "$tmp/result.json" >/dev/null || die "GitHub webhook verification failed after reconciliation"
pass "GitHub webhook configuration verified"

# Creation sends a ping automatically; updates need an explicit ping. A successful
# 2xx proves routing and HMAC configuration without mutating context refs.
gh api --method POST "repos/$REPO/hooks/$hook_id/pings" >/dev/null
for _ in 1 2 3 4 5; do
  sleep 2
  code="$(gh api "repos/$REPO/hooks/$hook_id/deliveries?per_page=10" \
    --jq '[.[] | select(.event == "ping")][0].status_code // 0')"
  case "$code" in 2??) pass "Signed GitHub ping delivery returned HTTP $code"; exit 0 ;; esac
done
die "Signed GitHub ping delivery did not return 2xx; inspect hook $hook_id deliveries"
