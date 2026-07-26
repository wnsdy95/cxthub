#!/usr/bin/env bash
# Run Gitleaks against either the publishable working tree or Git history.
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
MODE="${1:-tree}"
GITLEAKS_VERSION="v8.30.1"

run_gitleaks() {
  if command -v gitleaks >/dev/null 2>&1; then
    gitleaks "$@"
    return
  fi
  if ! command -v go >/dev/null 2>&1; then
    echo "[secret-scan] gitleaks or Go is required." >&2
    exit 1
  fi
  go run "github.com/zricethezav/gitleaks/v8@${GITLEAKS_VERSION}" "$@"
}

case "$MODE" in
  tree)
    scan_dir="$(mktemp -d)"
    file_list="$(mktemp)"
    cleanup() {
      find "$scan_dir" -type f -delete 2>/dev/null || true
      find "$scan_dir" -depth -type d -exec rmdir {} \; 2>/dev/null || true
      unlink "$file_list" 2>/dev/null || true
    }
    trap cleanup EXIT

    cd "$ROOT"
    while IFS= read -r -d '' file; do
      if [ -e "$file" ] || [ -L "$file" ]; then
        printf '%s\0' "$file"
      fi
    done < <(git ls-files --cached --others --exclude-standard -z) >"$file_list"
    tar --null -T "$file_list" -cf - | tar -xf - -C "$scan_dir"
    run_gitleaks dir --redact --no-banner --max-decode-depth=2 \
      --max-archive-depth=1 "$scan_dir"
    ;;
  history)
    cd "$ROOT"
    run_gitleaks git --redact --no-banner --max-decode-depth=2 \
      --max-archive-depth=1 "$ROOT"
    ;;
  *)
    echo "Usage: $0 {tree|history}" >&2
    exit 2
    ;;
esac

echo "[secret-scan] passed: $MODE"
