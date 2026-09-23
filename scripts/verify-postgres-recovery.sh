#!/usr/bin/env bash
# Dedicated validation databases only. The restore target must be empty.
set -euo pipefail
: "${CXT_RECOVERY_DSN:?Set source validation database DSN}"
: "${CXT_RECOVERY_RESTORE_DSN:?Set an EMPTY disposable restore database DSN}"
if [ "$CXT_RECOVERY_DSN" = "$CXT_RECOVERY_RESTORE_DSN" ]; then
  echo 'Source and restore target must differ.' >&2; exit 1
fi
if [ "$(psql "$CXT_RECOVERY_RESTORE_DSN" -XAtc "SELECT count(*) FROM pg_tables WHERE schemaname='public'")" != '0' ]; then
  echo 'Restore target is not empty; no changes made.' >&2; exit 1
fi
validation_dir="$(mktemp -d)"
trap 'rm -rf "$validation_dir"' EXIT
chmod 700 "$validation_dir"
pg_dump "$CXT_RECOVERY_DSN" --format=custom --no-owner --no-acl --file="$validation_dir/backup.dump"
pg_restore --dbname="$CXT_RECOVERY_RESTORE_DSN" --no-owner --no-acl --exit-on-error "$validation_dir/backup.dump"
cat > "$validation_dir/check.sql" <<'SQL'
SET TIME ZONE 'UTC';
SELECT format('SELECT %L, count(*), md5(COALESCE(string_agg(row_hash, %L ORDER BY row_hash), %L)) FROM (SELECT md5(row_to_json(t)::text) AS row_hash FROM %I.%I t) rows', tablename, '', '', schemaname, tablename)
FROM pg_tables WHERE schemaname='public' ORDER BY tablename
\gexec
SQL
psql "$CXT_RECOVERY_DSN" -XqAt -v ON_ERROR_STOP=1 -f "$validation_dir/check.sql" > "$validation_dir/source"
psql "$CXT_RECOVERY_RESTORE_DSN" -XqAt -v ON_ERROR_STOP=1 -f "$validation_dir/check.sql" > "$validation_dir/restore"
diff -u "$validation_dir/source" "$validation_dir/restore"
printf 'Backup/restore verified: %s tables have identical row counts and content digests.\n' "$(wc -l < "$validation_dir/source" | tr -d ' ')"
