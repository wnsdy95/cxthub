#!/usr/bin/env bash
# Fail-closed production deployment checks. This script never creates or modifies resources.
#
#   scripts/deploy-preflight.sh static    # Fast static/config check
#   scripts/deploy-preflight.sh full      # Full test + PG migration + Docker build
#   scripts/deploy-preflight.sh accounts  # GCP/Vercel account identity read-only check
#   scripts/deploy-preflight.sh ready     # Check before deploying bootstrap resources/images
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
MODE="${1:-static}"

pass() { printf '  ✓ %s\n' "$1"; }
die() { printf '  ✗ %s\n' "$1" >&2; exit 1; }
need() { command -v "$1" >/dev/null 2>&1 || die "Missing required command: $1"; }
require_env() {
  local name="$1"
  [ -n "${!name:-}" ] || die "Missing required environment variable: $name"
}
require_true() {
  local name="$1"
  require_env "$name"
  [ "${!name}" = "true" ] || die "Explicit confirmation required: $name=true"
}
reject_placeholder() {
  local name="$1"
  local normalized
  require_env "$name"
  normalized="$(printf '%s' "${!name}" | tr '[:upper:]' '[:lower:]')"
  case "$normalized" in
    replace-with-*) die "$name cannot use an example placeholder" ;;
  esac
}

terraform_check() (
  need terraform
  local tf_data
  tf_data="$(mktemp -d)"
  trap 'rm -rf "$tf_data"' EXIT
  terraform -chdir="$ROOT/deploy/terraform" fmt -check -recursive
  TF_DATA_DIR="$tf_data" terraform -chdir="$ROOT/deploy/terraform" init -backend=false -input=false -lockfile=readonly >/dev/null
  TF_DATA_DIR="$tf_data" terraform -chdir="$ROOT/deploy/terraform" validate >/dev/null
  TF_DATA_DIR="$tf_data" terraform -chdir="$ROOT/deploy/terraform" test >/dev/null
  pass "Terraform formatting, provider lock, schema, and input validation"
)

static_check() {
  need git
  need go
  need node
  need npm
  need python3

  cd "$ROOT"
  git diff --check
  pass "Git whitespace check"

  python3 - "$ROOT/.dockerignore" <<'PY'
import pathlib, sys

path = pathlib.Path(sys.argv[1])
actual = [line.strip() for line in path.read_text().splitlines() if line.strip().startswith("!")]
expected = [
    "!deploy/",
    "!deploy/Dockerfile",
    "!backend/",
    "!backend/go.mod",
    "!backend/go.sum",
    "!backend/cmd/",
    "!backend/cmd/**/",
    "!backend/cmd/**/*.go",
    "!backend/internal/",
    "!backend/internal/**/",
    "!backend/internal/**/*.go",
    "!schemas/",
    "!schemas/db/",
    "!schemas/db/migrations/",
    "!schemas/db/migrations/*.sql",
]
if actual != expected:
    raise SystemExit(f"Docker context allowlist drift: {actual!r}")
print("  ✓ Minimal Docker context allowlist")
PY

  if (TF_VAR_firebase_web_api_key='replace-with-firebase-web-api-key' \
      reject_placeholder TF_VAR_firebase_web_api_key) >/dev/null 2>&1; then
    die "Firebase placeholder rejection gate regression"
  fi
  (TF_VAR_firebase_web_api_key='AIzaSyExampleFirebaseWebApiKey123456' \
    reject_placeholder TF_VAR_firebase_web_api_key)
  pass "Firebase placeholder rejection gate"

  terraform_check

  (
    cd frontend/web
    npm test
    npm run check:i18n
    npm run check:vercel
    npm run typecheck
  )
  pass "Web unit, i18n, Vercel, and type checks"

  python3 - "$ROOT/schemas/db/migrations" <<'PY'
import pathlib, re, sys

root = pathlib.Path(sys.argv[1])
files = sorted(root.glob("*.sql"))
if not files:
    raise SystemExit("No migration files found")
numbers = []
for path in files:
    match = re.match(r"^(\d{4})_[a-z0-9_]+\.sql$", path.name)
    if not match:
        raise SystemExit(f"Invalid migration file name: {path.name}")
    numbers.append(int(match.group(1)))
expected = list(range(1, len(numbers) + 1))
if numbers != expected:
    raise SystemExit(f"Migration numbers are not consecutive: {numbers}")
print(f"  ✓ DB migration 0001–{numbers[-1]:04d} continuity")
PY
}

postgres_smoke() (
  need docker
  local name="cxt-preflight-pg-$$"
  local port=""
  cleanup_postgres() { docker rm -f "$name" >/dev/null 2>&1 || true; }
  trap cleanup_postgres EXIT

  docker run -d --name "$name" \
    -e POSTGRES_USER=cxt -e POSTGRES_PASSWORD=cxt -e POSTGRES_DB=cxthub_test \
    -p 127.0.0.1::5432 postgres:16 >/dev/null
  for _ in $(seq 1 60); do
    if docker exec "$name" pg_isready -U cxt -d cxthub_test >/dev/null 2>&1; then
      break
    fi
    sleep 0.5
  done
  docker exec "$name" pg_isready -U cxt -d cxthub_test >/dev/null 2>&1 || die "Postgres is not ready"
  port="$(docker port "$name" 5432/tcp | awk -F: 'NR==1 {print $NF}')"
  [ -n "$port" ] || die "Postgres temporary port not found"

  (
    cd "$ROOT/backend"
    CXT_TEST_DSN="postgres://cxt:cxt@127.0.0.1:${port}/cxthub_test?sslmode=disable" \
      go test -tags postgres ./internal/adapters/store/ -run TestPGSmoke -count=1 -v
  )
  pass "All migrations apply idempotently on a real PostgreSQL instance"
)

image_smoke() (
  need curl
  need docker
  local name="cxt-preflight-app-$$"
  local port=""
  cleanup_app() { docker rm -f "$name" >/dev/null 2>&1 || true; }
  trap cleanup_app EXIT

  docker run -d --name "$name" \
    -e CXT_AUTH=firebase -e CXT_FIREBASE_PROJECT=example-firebase-project \
    -p 127.0.0.1::8907 cxtd:preflight --data /tmp/cxt-data >/dev/null
  port="$(docker port "$name" 8907/tcp | awk -F: 'NR==1 {print $NF}')"
  [ -n "$port" ] || die "cxtd temporary port not found"
  for _ in $(seq 1 60); do
    if curl -fsS "http://127.0.0.1:${port}/api/v1/health" 2>/dev/null \
      | grep -q '"status":"ok"'; then
      pass "Production image start/health"
      return
    fi
    sleep 0.25
  done
  docker logs "$name" >&2 || true
  die "Production image health failure"
)

full_check() {
  static_check
  need docker

  (cd "$ROOT/backend" && go test ./... && go test -tags postgres ./... && go vet ./...)
  pass "Backend FS/Postgres test + vet"
  (cd "$ROOT/cli" && go test ./... && go vet ./...)
  pass "CLI test + vet"
  (cd "$ROOT/frontend/web" && npm run build)
  pass "Web production build"

  postgres_smoke
  docker build -f "$ROOT/deploy/Dockerfile" -t cxtd:preflight "$ROOT"
  pass "Production Docker image build (no push)"
  image_smoke
}

account_identity_check() {
  need git
  need gcloud
  need node

  require_env CXT_EXPECTED_GCP_ACCOUNT
  require_env CXT_EXPECTED_VERCEL_ACCOUNT
  require_env VERCEL_API_TOKEN
  require_env TF_VAR_gcp_project

  local account adc_account adc_token billing expected_adc vercel_account vercel_team
  account="$(gcloud auth list --filter=status:ACTIVE --format='value(account)' | head -1)"
  [ "$account" = "$CXT_EXPECTED_GCP_ACCOUNT" ] || die "GCP active account mismatch: ${account:-none}"
  gcloud projects describe "$TF_VAR_gcp_project" --format='value(projectId)' >/dev/null
  adc_token="$(gcloud auth application-default print-access-token)"
  adc_account="$(CXT_ADC_TOKEN="$adc_token" node <<'NODE'
const url = new URL('https://oauth2.googleapis.com/tokeninfo');
url.searchParams.set('access_token', process.env.CXT_ADC_TOKEN);
const response = await fetch(url);
if (!response.ok) {
  process.stderr.write(`Google ADC token identity lookup failed (${response.status})\n`);
  process.exit(1);
}
const data = await response.json();
process.stdout.write(data.email ?? '');
NODE
)"
  unset adc_token
  expected_adc="${CXT_EXPECTED_GCP_ADC_ACCOUNT:-$CXT_EXPECTED_GCP_ACCOUNT}"
  [ "$adc_account" = "$expected_adc" ] \
    || die "GCP ADC account mismatch: ${adc_account:-not verifiable} (expected $expected_adc)"
  billing="$(gcloud billing projects describe "$TF_VAR_gcp_project" --format='value(billingEnabled)')"
  { [ "$billing" = "True" ] || [ "$billing" = "true" ]; } \
    || die "GCP project billing not activated"
  pass "GCP CLI and ADC identities, project access, and billing verified ($account / $adc_account / $TF_VAR_gcp_project)"

  vercel_account="$(node <<'NODE'
const response = await fetch('https://api.vercel.com/v2/user', {
  headers: { Authorization: `Bearer ${process.env.VERCEL_API_TOKEN}` },
});
if (!response.ok) {
  process.stderr.write(`Vercel token rejected (${response.status})\n`);
  process.exit(1);
}
const data = await response.json();
process.stdout.write(data.user?.username ?? data.username ?? '');
NODE
)"
  [ "$vercel_account" = "$CXT_EXPECTED_VERCEL_ACCOUNT" ] \
    || die "Vercel token account mismatch: ${vercel_account:-not verifiable}"
  if [ -n "${TF_VAR_vercel_team_id:-}" ]; then
    vercel_team="$(node <<'NODE'
const teamId = process.env.TF_VAR_vercel_team_id;
const response = await fetch(`https://api.vercel.com/v2/teams/${encodeURIComponent(teamId)}`, {
  headers: { Authorization: `Bearer ${process.env.VERCEL_API_TOKEN}` },
});
if (!response.ok) {
  process.stderr.write(`Vercel team lookup failed (${response.status})\n`);
  process.exit(1);
}
const data = await response.json();
if (data.id !== teamId) {
  process.stderr.write('Vercel team identity mismatch\n');
  process.exit(1);
}
process.stdout.write(`${data.id} (${data.slug ?? data.name ?? 'unknown'})`);
NODE
)"
    pass "Vercel token account and team access verified ($vercel_account / $vercel_team)"
  else
    pass "Vercel token personal account verified ($vercel_account)"
  fi

  terraform_check
  pass "Account identity preflight completed — no resources modified"
}

ready_check() {
  # Revoke any previously exposed credential at its issuer; deleting a local file is insufficient.
  # Do not begin final deployment checks without explicit operator confirmation.
  require_true CXT_CONFIRMED_LEAKED_PAT_REVOKED
  account_identity_check
  require_env CXT_TF_STATE_BUCKET
  require_env TF_VAR_domain
  require_env TF_VAR_image_tag
  require_env TF_VAR_firebase_project
  require_env TF_VAR_firebase_web_api_key
  reject_placeholder TF_VAR_firebase_web_api_key

  local head remote required enabled api postgres_secret github_webhook_secret region image
  head="$(git -C "$ROOT" rev-parse HEAD)"
  [ "$TF_VAR_image_tag" = "$head" ] || die "TF_VAR_image_tag is not the current HEAD"
  [ -z "$(git -C "$ROOT" status --porcelain)" ] || die "Working tree is not clean"
  remote="$(git -C "$ROOT" ls-remote origin refs/heads/main | awk '{print $1}')"
  [ "$remote" = "$head" ] || die "HEAD differs from origin/main — Vercel and the image could use different sources"
  pass "Source HEAD/origin/image_tag match ($head)"

  required="run.googleapis.com artifactregistry.googleapis.com secretmanager.googleapis.com iam.googleapis.com"
  enabled="$(gcloud services list --project "$TF_VAR_gcp_project" --enabled --format='value(config.name)')"
  for api in $required; do
    grep -qx "$api" <<<"$enabled" || die "GCP API not enabled: $api"
  done
  gcloud storage buckets describe "gs://$CXT_TF_STATE_BUCKET" >/dev/null
  postgres_secret="${TF_VAR_postgres_secret_id:-cxt-postgres-dsn}"
  gcloud secrets describe "$postgres_secret" --project "$TF_VAR_gcp_project" >/dev/null
  [ -n "$(gcloud secrets versions list "$postgres_secret" --project "$TF_VAR_gcp_project" --filter='state=ENABLED' --limit=1 --format='value(name)')" ] \
    || die "No ENABLED version in $postgres_secret"
  github_webhook_secret="${TF_VAR_github_webhook_secret_id:-cxt-github-webhook-secret}"
  gcloud secrets describe "$github_webhook_secret" --project "$TF_VAR_gcp_project" >/dev/null
  [ -n "$(gcloud secrets versions list "$github_webhook_secret" --project "$TF_VAR_gcp_project" --filter='state=ENABLED' --limit=1 --format='value(name)')" ] \
    || die "No ENABLED version in $github_webhook_secret"
  pass "GCP API/state bucket/Postgres and GitHub webhook secret metadata verified"

  region="${TF_VAR_gcp_region:-asia-northeast3}"
  image="${region}-docker.pkg.dev/${TF_VAR_gcp_project}/cxthub/cxtd:${TF_VAR_image_tag}"
  gcloud artifacts docker images describe "$image" --project "$TF_VAR_gcp_project" >/dev/null
  pass "Artifact Registry image verified ($image)"

  pass "Final deployment preflight complete — a Terraform plan can now be created and reviewed"
}

case "$MODE" in
  static) static_check ;;
  full) full_check ;;
  accounts) account_identity_check ;;
  ready) ready_check ;;
  *)
    echo "Usage: $0 {static|full|accounts|ready}" >&2
    exit 2
    ;;
esac
