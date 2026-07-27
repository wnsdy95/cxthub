#!/usr/bin/env bash
# Fail-closed checks for the file set that will become the public repository.
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
MODE="${1:-tree}"
cd "$ROOT"

case "$MODE" in
  tree|full) ;;
  *) echo "Usage: $0 {tree|full}" >&2; exit 2 ;;
esac

required=(
  LICENSE NOTICE THIRD_PARTY_NOTICES.md README.md
  SECURITY.md CONTRIBUTING.md CODE_OF_CONDUCT.md GOVERNANCE.md SUPPORT.md
  TRADEMARKS.md CHANGELOG.md docs/BRANCHING.md docs/CLI.md
  docs/INSTALLATION.md docs/RELEASING.md
  .github/workflows/ci.yml .github/workflows/release.yml
)
for file in "${required[@]}"; do
  if [ ! -f "$file" ]; then
    echo "[public-check] required file is missing: $file" >&2
    exit 1
  fi
done

if ! cmp -s LICENSE distrib/LICENSE; then
  echo "[public-check] root LICENSE differs from distrib/LICENSE." >&2
  exit 1
fi

# Lock the highest-risk ignore decisions so a later .gitignore cleanup cannot
# silently reopen the public export boundary.
for path in \
  .claude/settings.json .codex/config.toml .agents/settings.json .mcp.json \
  frontend/web/.env.production deploy/terraform/prod.tfvars private.pem \
  data.sqlite coverage.out cxt-data/refs.json nested/.cxt/HEAD \
  frontend/web/playwright-report/index.html frontend/web/node_modules/pkg/index.js \
  docs/internal/notes.md docs/PRIVATE.md
do
  if ! git check-ignore -q --no-index "$path"; then
    echo "[public-check] required ignore rule is missing for: $path" >&2
    exit 1
  fi
done
for path in \
  frontend/web/.env.example deploy/terraform/terraform.tfvars.example \
  frontend/web/package-lock.json deploy/terraform/.terraform.lock.hcl \
  docs/BRANCHING.md docs/CLI.md docs/INSTALLATION.md docs/RELEASING.md
do
  if git check-ignore -q --no-index "$path"; then
    echo "[public-check] reproducible source file is incorrectly ignored: $path" >&2
    exit 1
  fi
done

# Ignore rules are part of the public-tree contract. A tracked ignored path
# would still be included by git archive, so reject that state explicitly.
tracked_ignored="$(git ls-files -ci --exclude-standard)"
if [ -n "$tracked_ignored" ]; then
  echo "[public-check] tracked paths conflict with .gitignore:" >&2
  printf '  %s\n' "$tracked_ignored" >&2
  exit 1
fi

candidate_list="$(mktemp)"
cleanup() { unlink "$candidate_list" 2>/dev/null || true; }
trap cleanup EXIT
while IFS= read -r -d '' file; do
  # git ls-files --cached still reports tracked paths deleted in the working
  # tree. A public export represents the current tree, so omit those paths
  # while retaining real files and symlinks (which are rejected below).
  if [ -e "$file" ] || [ -L "$file" ]; then
    printf '%s\0' "$file"
  fi
done < <(git ls-files --cached --others --exclude-standard -z) >"$candidate_list"

while IFS= read -r -d '' file; do
  case "$file" in
    .git/*|.cxt/*|*/.cxt/*|.cxt-daemon/*|*/.cxt-daemon/*|cxt-data/*|*/cxt-data/*|\
    .claude/*|.codex/*|.agents/*|.cursor/*|.windsurf/*|.mcp.json|\
    bin/*|build/*|dist/*|node_modules/*|*/dist/*|*/build/*|*/node_modules/*|\
    .terraform/*|*/.terraform/*|.cache/*|.vite/*|.turbo/*|.parcel-cache/*|\
    */.cache/*|*/.vite/*|*/.turbo/*|*/.parcel-cache/*|\
    coverage/*|playwright-report/*|blob-report/*|test-results/*|\
    */playwright-report/*|*/blob-report/*|*/test-results/*)
      echo "[public-check] local data or build output included: $file" >&2
      exit 1
      ;;
  esac

  case "$file" in
    *.env.example|*.tfvars.example)
      ;;
    .DS_Store|*/.DS_Store|.env|*/.env|.env.*|*/.env.*|*.env|*.env.*|\
    *.secret|*.secrets|.secrets*|*/.secrets*|secrets/*|*/secrets/*|\
    credentials.json|*/credentials.json|auth.json|*/auth.json|\
    client_secret*.json|*/client_secret*.json|application_default_credentials.json|\
    */application_default_credentials.json|serviceAccount*.json|*/serviceAccount*.json|\
    *-firebase-adminsdk-*.json|firebase-service-account.json|*/firebase-service-account.json|\
    .npmrc|*/.npmrc|.netrc|*/.netrc|.pypirc|*/.pypirc|\
    *.pem|*.key|*.p12|*.pfx|*.jks|*.keystore|*.kdbx|id_rsa|*/id_rsa|id_ed25519|*/id_ed25519|\
    *.tfstate|*.tfstate.*|*.tfplan|*.tfvars|*.tfvars.json|\
    *settings.local.json|.cxtsecrets|*/.cxtsecrets)
      echo "[public-check] private configuration file included: $file" >&2
      exit 1
      ;;
  esac

  case "$file" in
    *.db|*.db-*|*.sqlite|*.sqlite-*|*.sqlite3|*.sqlite3-*|*.bolt|*.bbolt|\
    *.pid|*.sock|*.log|*.tmp|*.bak|*.test|*.prof|*.pprof|*.coverprofile|\
    coverage.out|*/coverage.out|coverage.html|*/coverage.html|*.tsbuildinfo|\
    .eslintcache|*/.eslintcache|.stylelintcache|*/.stylelintcache|\
    .nyc_output/*|*/.nyc_output/*|__pycache__/*|*/__pycache__/*)
      echo "[public-check] generated runtime or test artifact included: $file" >&2
      exit 1
      ;;
  esac

  if [ -L "$file" ]; then
    echo "[public-check] symbolic links are not allowed in the public export: $file" >&2
    exit 1
  fi
  if [ -f "$file" ] && [ "$(wc -c <"$file")" -gt 2097152 ]; then
    echo "[public-check] files larger than 2 MiB require explicit review: $file" >&2
    exit 1
  fi

  # Korean is intentionally retained only in the product's Korean locale.
  # Reject CJK scripts elsewhere so accidental untranslated or machine-generated
  # fragments cannot enter the English-only public source tree.
  if [ "$file" != "frontend/web/src/i18n/locales/ko.ts" ] &&
     [ -f "$file" ] && LC_ALL=C grep -Iq . -- "$file" &&
     rg -q '[\p{Hangul}\p{Han}\p{Hiragana}\p{Katakana}]' -- "$file"; then
    echo "[public-check] non-English CJK text outside the ko.ts locale: $file" >&2
    exit 1
  fi
done <"$candidate_list"

if git ls-files -s | awk '$1 == "160000" {found=1} END {exit !found}'; then
  echo "[public-check] submodules are not allowed in the public export." >&2
  exit 1
fi

for term in 's''wyg' 'rii''do' 'team-context-save-''fork' 'jy''min'; do
  if xargs -0 rg -i -l -- "$term" <"$candidate_list" >/dev/null 2>&1; then
    echo "[public-check] forbidden legacy identifier found: $term" >&2
    exit 1
  fi
done

if rg -q 'RELEASE_TOKEN|secrets\.RELEASE_TOKEN' .github .goreleaser.yaml; then
  echo "[public-check] a cross-repository release PAT reference remains." >&2
  exit 1
fi

git diff --check
git diff --cached --check
bash scripts/secret-scan.sh tree

if [ "$MODE" = full ]; then
  bash scripts/secret-scan.sh history
  legacy_re='s''wyg|rii''do|team-context-save-''fork|jy''min'
  if git log --all --format='%an%n%ae%n%cn%n%ce%n%B' | rg -i -q "$legacy_re"; then
    echo "[public-check] Git metadata history contains a forbidden identifier." >&2
    exit 1
  fi
  while IFS= read -r rev; do
    if git grep -I -i -q -E "$legacy_re" "$rev" -- 2>/dev/null; then
      echo "[public-check] Git blob history contains a forbidden identifier: $rev" >&2
      exit 1
    fi
  done < <(git rev-list --all)
fi

echo "[public-check] passed: $MODE"
