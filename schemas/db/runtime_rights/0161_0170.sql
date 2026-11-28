-- Rights of the server's own database accounts on the tables that migrations
-- 0161 to 0170 create. The migrations make the account that applies them the
-- owner of these tables; the API, MCP and worker accounts may not use a table
-- until they are given rights on it, and without them every request that
-- reads sessions answers "internal server error" (the server's log says
-- "permission denied for table ...").
--
-- The rights follow what each account may do on public.repos: an account
-- that reads repos reads the new tables; an account that writes repos
-- (INSERT) inserts and updates rows in them, and deletes from the ones the
-- server deletes from. Nothing else is granted: no ownership and no DDL.
-- Run it once, after migrate --apply, as the account that applied the
-- migrations, with psql -v ON_ERROR_STOP=1 -f. Running it again changes
-- nothing. See docs/DATABASE_MIGRATIONS.md.
BEGIN;
DO $grant$
DECLARE
  new_tables text[] := ARRAY[
    'anchored_sessions', 'anchored_session_commits',
    'branch_lifetimes', 'branch_ledger_ids',
    'anchored_notes', 'reported_worktrees',
    'remote_git_state', 'remote_git_commits', 'remote_git_branches', 'remote_git_pulls', 'remote_git_pull_commits',
    'anchored_session_publications', 'remote_git_pushes',
    'anchored_session_summaries',
    'anchored_session_removals',
    'reported_collectors', 'claimed_pushes'];
  -- The tables the server deletes rows from.
  deleted_from text[] := ARRAY[
    'anchored_sessions', 'anchored_session_commits', 'anchored_notes', 'anchored_session_removals',
    'remote_git_commits', 'remote_git_branches', 'remote_git_pulls', 'remote_git_pull_commits', 'remote_git_pushes'];
  account record;
  name text;
  rights text;
  accounts int := 0;
BEGIN
  FOREACH name IN ARRAY new_tables LOOP
    IF to_regclass('public.' || quote_ident(name)) IS NULL THEN
      RAISE EXCEPTION 'table public.% is missing: apply migrations 0161 to 0170 first', name;
    END IF;
  END LOOP;
  FOR account IN
    SELECT grant_entry.grantee::regrole AS role,
           bool_or(grant_entry.privilege_type = 'SELECT') AS reads,
           bool_or(grant_entry.privilege_type = 'INSERT') AS writes
    FROM pg_class AS repos
    CROSS JOIN LATERAL aclexplode(repos.relacl) AS grant_entry
    WHERE repos.oid = 'public.repos'::regclass
      AND grant_entry.grantee <> 0
      AND grant_entry.grantee <> repos.relowner
      AND grant_entry.privilege_type IN ('SELECT', 'INSERT')
    GROUP BY grant_entry.grantee
    ORDER BY 1
  LOOP
    FOREACH name IN ARRAY new_tables LOOP
      rights := concat_ws(', ',
        CASE WHEN account.reads THEN 'SELECT' END,
        CASE WHEN account.writes THEN 'INSERT, UPDATE' END,
        CASE WHEN account.writes AND name = ANY (deleted_from) THEN 'DELETE' END);
      EXECUTE format('GRANT %s ON TABLE public.%I TO %s', rights, name, account.role);
    END LOOP;
    IF account.writes THEN
      RAISE NOTICE 'account % now reads and writes the % new tables, and deletes from the % the server deletes from',
        account.role, array_length(new_tables, 1), array_length(deleted_from, 1);
    ELSE
      RAISE NOTICE 'account % now reads the % new tables', account.role, array_length(new_tables, 1);
    END IF;
    accounts := accounts + 1;
  END LOOP;
  IF accounts = 0 THEN
    RAISE EXCEPTION 'no account other than the owner has rights on public.repos: nothing was granted';
  END IF;
END
$grant$;
COMMIT;
