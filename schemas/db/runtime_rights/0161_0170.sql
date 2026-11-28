-- Rights of the server's own database accounts on the tables that migrations
-- 0161 to 0170 create. The migrations make the account that applies them the
-- owner of these tables; the API, MCP and worker accounts may not use a table
-- until they are given rights on it, and without them every request that
-- reads sessions answers "internal server error" (the server's log says
-- "permission denied for table ...").
--
-- Each account gets, on each of these tables, the rights it already has on
-- public.repos, of SELECT, INSERT, UPDATE and DELETE, and nothing else: no
-- ownership and no DDL. An account that only reads repos only reads them.
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
  account record;
  name text;
  accounts int := 0;
BEGIN
  FOREACH name IN ARRAY new_tables LOOP
    IF to_regclass('public.' || quote_ident(name)) IS NULL THEN
      RAISE EXCEPTION 'table public.% is missing: apply migrations 0161 to 0170 first', name;
    END IF;
  END LOOP;
  FOR account IN
    SELECT grant_entry.grantee::regrole AS role,
           string_agg(grant_entry.privilege_type, ', ' ORDER BY grant_entry.privilege_type) AS rights
    FROM pg_class AS repos
    CROSS JOIN LATERAL aclexplode(repos.relacl) AS grant_entry
    WHERE repos.oid = 'public.repos'::regclass
      AND grant_entry.grantee <> 0
      AND grant_entry.grantee <> repos.relowner
      AND grant_entry.privilege_type IN ('SELECT', 'INSERT', 'UPDATE', 'DELETE')
    GROUP BY grant_entry.grantee
    ORDER BY 1
  LOOP
    FOREACH name IN ARRAY new_tables LOOP
      EXECUTE format('GRANT %s ON TABLE public.%I TO %s', account.rights, name, account.role);
    END LOOP;
    RAISE NOTICE 'account % now has % on the % new tables', account.role, account.rights, array_length(new_tables, 1);
    accounts := accounts + 1;
  END LOOP;
  IF accounts = 0 THEN
    RAISE EXCEPTION 'no account other than the owner has rights on public.repos: nothing was granted';
  END IF;
END
$grant$;
COMMIT;
