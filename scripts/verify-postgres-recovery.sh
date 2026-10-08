#!/usr/bin/env bash
# Dedicated offline validation databases only. Keep clients and writers stopped
# on both databases for the entire rehearsal. The restore target must be empty.
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
FROM pg_tables WHERE schemaname='public'
  AND (:'include_catalog'::boolean OR tablename NOT IN ('repository_catalog_state','repository_catalog_changes'))
ORDER BY tablename
\gexec
SQL
psql "$CXT_RECOVERY_DSN" -XqAt -v ON_ERROR_STOP=1 -v include_catalog=true -f "$validation_dir/check.sql" > "$validation_dir/source"
psql "$CXT_RECOVERY_RESTORE_DSN" -XqAt -v ON_ERROR_STOP=1 -v include_catalog=true -f "$validation_dir/check.sql" > "$validation_dir/restore"
diff -u "$validation_dir/source" "$validation_dir/restore"
table_count="$(wc -l < "$validation_dir/source" | tr -d ' ')"

catalog_reset_available="$(psql "$CXT_RECOVERY_RESTORE_DSN" -XqAt -v ON_ERROR_STOP=1 -c "SELECT to_regprocedure('public.cxt_reset_catalog(text)') IS NOT NULL")"
if [ "$catalog_reset_available" != 't' ]; then
  printf 'Backup/restore content verified: %s tables have identical row counts and content digests.\n' "$table_count"
  printf 'catalog epoch rotation not available; migrate before serving\n'
  exit 0
fi

cat > "$validation_dir/rotate-catalog.sql" <<'SQL'
BEGIN;
SET LOCAL TIME ZONE 'UTC';
SET LOCAL search_path=public,pg_temp;
LOCK TABLE repos, snapshots, refs, context_history IN SHARE ROW EXCLUSIVE MODE;
LOCK TABLE repository_catalog_state, repository_catalog_changes IN SHARE ROW EXCLUSIVE MODE;
-- The all-table digest comparison proved these restored epochs equal the source
-- epochs. Save them before reset so every repository must receive a new epoch.
CREATE TEMP TABLE recovery_catalog_epochs ON COMMIT DROP AS
SELECT repo_id,epoch FROM repository_catalog_state;
DO $$
DECLARE repository record;
BEGIN
    FOR repository IN SELECT id FROM repos ORDER BY id COLLATE "C" LOOP
        PERFORM cxt_reset_catalog(repository.id);
    END LOOP;

    IF (SELECT count(*) FROM repository_catalog_state) <> (SELECT count(*) FROM repos)
       OR EXISTS (
           SELECT 1 FROM repos r
           LEFT JOIN repository_catalog_state st ON st.repo_id=r.id
           LEFT JOIN recovery_catalog_epochs old ON old.repo_id=r.id
           WHERE st.repo_id IS NULL OR old.repo_id IS NULL OR st.epoch=old.epoch
              OR st.head_seq<>0 OR st.floor_seq<>0 OR st.writer_xid IS NOT NULL
       ) THEN
        RAISE EXCEPTION 'catalog recovery failed: missing state, reused epoch or nonzero generation';
    END IF;

    IF EXISTS (
        SELECT 1 FROM repository_catalog_changes c
        LEFT JOIN repository_catalog_state st ON st.repo_id=c.repo_id
        WHERE c.epoch IS DISTINCT FROM st.epoch OR c.seq<>0 OR c.deleted
    ) THEN
        RAISE EXCEPTION 'catalog recovery failed: stale or non-baseline journal rows';
    END IF;

    IF EXISTS (
        WITH source_rows AS (
            SELECT id AS repo_id,'protocol' AS kind,to_jsonb(r) AS row FROM repos r
            UNION ALL SELECT repo_id,'snapshot',to_jsonb(s) FROM snapshots s
            UNION ALL SELECT repo_id,'ref',to_jsonb(r) FROM refs r
            UNION ALL SELECT repo_id,'history',to_jsonb(h) FROM context_history h
        ), expected AS (
            SELECT repo_id,kind,cxt_catalog_key(kind,row) AS key,cxt_catalog_image(kind,row) AS image
            FROM source_rows
        ), actual AS (
            SELECT repo_id,entity_kind AS kind,entity_key AS key,after_image AS image
            FROM repository_catalog_changes
        )
        SELECT 1 FROM (
            (SELECT * FROM expected EXCEPT SELECT * FROM actual)
            UNION ALL
            (SELECT * FROM actual EXCEPT SELECT * FROM expected)
        ) mismatches
    ) THEN
        RAISE EXCEPTION 'catalog recovery failed: incomplete or incorrect baseline images';
    END IF;
END
$$;
COMMIT;
SELECT count(*) FROM repository_catalog_state;
SQL
rotated_repositories="$(psql "$CXT_RECOVERY_RESTORE_DSN" -XqAt -v ON_ERROR_STOP=1 -f "$validation_dir/rotate-catalog.sql")"

# Verify the source stayed frozen and reset changed only the restored catalog.
psql "$CXT_RECOVERY_DSN" -XqAt -v ON_ERROR_STOP=1 -v include_catalog=true -f "$validation_dir/check.sql" > "$validation_dir/source-after"
diff -u "$validation_dir/source" "$validation_dir/source-after"
awk -F '|' '$1 != "repository_catalog_state" && $1 != "repository_catalog_changes"' "$validation_dir/source" > "$validation_dir/source-rows"
psql "$CXT_RECOVERY_RESTORE_DSN" -XqAt -v ON_ERROR_STOP=1 -v include_catalog=false -f "$validation_dir/check.sql" > "$validation_dir/restore-after"
diff -u "$validation_dir/source-rows" "$validation_dir/restore-after"
printf 'Backup/restore verified: %s tables matched before rotation; %s repository epochs rotated with complete generation-zero baselines; source rows unchanged.\n' "$table_count" "$rotated_repositories"
