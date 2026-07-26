#!/usr/bin/env bash
# Export only tracked and non-ignored candidate files into a brand-new Git repo.
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
DEST="${1:-}"

if [ -z "$DEST" ]; then
  echo "Usage: CXT_CONFIRMED_LEAKED_PAT_REVOKED=true $0 <new-directory>" >&2
  exit 2
fi
if [ "${CXT_CONFIRMED_LEAKED_PAT_REVOKED:-}" != true ]; then
  echo "[public-export] Confirm the suspected leaked PAT is revoked in GitHub," >&2
  echo "then set CXT_CONFIRMED_LEAKED_PAT_REVOKED=true." >&2
  exit 1
fi

parent="$(cd "$(dirname "$DEST")" && pwd)"
dest_abs="$parent/$(basename "$DEST")"
case "$dest_abs/" in
  "$ROOT/"*) echo "[public-export] destination must be outside the source repository." >&2; exit 1 ;;
esac
if [ -e "$dest_abs" ]; then
  echo "[public-export] destination already exists: $dest_abs" >&2
  exit 1
fi

cd "$ROOT"
if ! git rev-parse --verify HEAD >/dev/null 2>&1; then
  echo "[public-export] source repository has no committed HEAD." >&2
  exit 1
fi
if [ -n "$(git status --porcelain --untracked-files=normal)" ]; then
  echo "[public-export] source repository must be clean; commit or remove every non-ignored change first." >&2
  git status --short >&2
  exit 1
fi
bash scripts/public-preflight.sh tree

mkdir "$dest_abs"
# Export the reviewed commit exactly. Untracked working files, ignored local
# data, reflogs, remotes, and every object from the private history stay behind.
git archive --format=tar HEAD | tar -xf - -C "$dest_abs"

git -C "$dest_abs" init -b main >/dev/null
git -C "$dest_abs" add -A
git -C "$dest_abs" diff --cached --check
(cd "$dest_abs" && bash scripts/public-preflight.sh tree)
if git -C "$dest_abs" rev-parse --verify HEAD >/dev/null 2>&1; then
  echo "[public-export] fresh repository unexpectedly contains commit history." >&2
  exit 1
fi

echo "[public-export] ready: $dest_abs"
echo "The old .git directory, reflogs, remotes, and ignored local data were not copied."
echo "Next steps:"
echo "  cd '$dest_abs'"
echo "  git config user.name   # verify the intended public author"
echo "  git config user.email  # verify the intended public email"
echo "  git diff --cached --stat"
echo "  git commit -S -m 'Initial public release'"
echo "  test \"\$(git rev-list --count --all)\" -eq 1"
echo "  make public-check-full"
echo "  git remote add origin git@github.com:wnsdy95/cxthub.git"
echo "  git push -u origin main"
