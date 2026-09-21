-- Preserve stable IDs while moving the active product vocabulary to repositories
-- and organizations. Content flattening is performed by the deterministic
-- application migration before this server accepts requests.
ALTER TABLE workspaces RENAME TO repositories;
ALTER TABLE enterprises RENAME TO organizations;
ALTER TABLE enterprise_memberships RENAME TO organization_memberships;
ALTER TABLE enterprise_policies RENAME TO organization_policies;
ALTER TABLE enterprise_audit_events RENAME TO organization_audit_events;
ALTER TABLE enterprise_break_glass_grants RENAME TO organization_break_glass_grants;

DO $$
DECLARE col record;
BEGIN
    FOR col IN SELECT table_name, column_name FROM information_schema.columns
        WHERE table_schema = current_schema()
        AND (column_name LIKE '%workspace%' OR column_name = 'enterprise_id')
    LOOP
        EXECUTE format('ALTER TABLE %I RENAME COLUMN %I TO %I', col.table_name,
            col.column_name, replace(replace(replace(col.column_name,
            'workspaces','repositories'),'workspace','repository'),'enterprise_id','organization_id'));
    END LOOP;
END $$;

-- The registry retains its existing ID and aliases; its company subject is now
-- an Organization. An upper Enterprise is not created implicitly.
DO $$
DECLARE con record;
BEGIN
    FOR con IN SELECT conname FROM pg_constraint
        WHERE conrelid = 'namespaces'::regclass AND contype = 'c'
    LOOP
        EXECUTE format('ALTER TABLE namespaces DROP CONSTRAINT %I', con.conname);
    END LOOP;
END $$;
UPDATE namespaces SET kind='organization' WHERE kind='enterprise';
ALTER TABLE namespaces ADD CONSTRAINT namespace_owner_kind CHECK (
    (kind='user' AND user_id IS NOT NULL AND organization_id IS NULL) OR
    (kind='organization' AND user_id IS NULL AND organization_id IS NOT NULL)
);

-- Rename the live schema's supporting objects too. ALTER ... RENAME preserves
-- OIDs and trigger dependencies; creating a new function under a new name would
-- leave existing triggers pointing at the old function body.
DO $$
DECLARE obj record; next_name text;
BEGIN
    FOR obj IN SELECT c.conname, r.relname FROM pg_constraint c
        JOIN pg_class r ON r.oid=c.conrelid
        JOIN pg_namespace n ON n.oid=r.relnamespace
        WHERE n.nspname=current_schema()
        AND (c.conname LIKE '%workspace%' OR c.conname LIKE '%enterprise%')
    LOOP
        next_name := replace(replace(replace(obj.conname,'workspaces','repositories'),'workspace','repository'),'enterprise','organization');
        EXECUTE format('ALTER TABLE %I RENAME CONSTRAINT %I TO %I', obj.relname, obj.conname, next_name);
    END LOOP;
    FOR obj IN SELECT c.relname FROM pg_class c
        JOIN pg_namespace n ON n.oid=c.relnamespace
        WHERE n.nspname=current_schema() AND c.relkind='i'
        AND (c.relname LIKE '%workspace%' OR c.relname LIKE '%enterprise%')
    LOOP
        next_name := replace(replace(replace(obj.relname,'workspaces','repositories'),'workspace','repository'),'enterprise','organization');
        EXECUTE format('ALTER INDEX %I RENAME TO %I', obj.relname, next_name);
    END LOOP;
    FOR obj IN SELECT t.tgname, c.relname FROM pg_trigger t
        JOIN pg_class c ON c.oid=t.tgrelid
        JOIN pg_namespace n ON n.oid=c.relnamespace
        WHERE n.nspname=current_schema() AND NOT t.tgisinternal
        AND (t.tgname LIKE '%workspace%' OR t.tgname LIKE '%enterprise%')
    LOOP
        next_name := replace(replace(replace(obj.tgname,'workspaces','repositories'),'workspace','repository'),'enterprise','organization');
        EXECUTE format('ALTER TRIGGER %I ON %I RENAME TO %I', obj.tgname, obj.relname, next_name);
    END LOOP;
END $$;
ALTER FUNCTION cxt_require_enterprise_owner() RENAME TO cxt_require_organization_owner;

-- Recompile guards against the renamed columns. SQL migration history remains
-- immutable, including the original definitions in 0037.
DO $$
DECLARE fn record;
BEGIN
    FOR fn IN SELECT pg_get_functiondef(p.oid) AS definition FROM pg_proc p
        JOIN pg_namespace n ON n.oid=p.pronamespace
        WHERE n.nspname=current_schema() AND p.proname LIKE 'cxt_%' AND p.prokind='f'
    LOOP
        EXECUTE replace(replace(replace(replace(replace(replace(fn.definition, 'enterprise_id', 'organization_id'),
            '''enterprise''', '''organization'''), 'workspaces','repositories'),'workspace','repository'),
            'enterprise_memberships','organization_memberships'),'enterprises','organizations');
    END LOOP;
END $$;

CREATE TABLE repository_path_aliases (
    namespace_key TEXT NOT NULL,
    owner_handle TEXT NOT NULL,
    path TEXT NOT NULL,
    repository_id TEXT NOT NULL REFERENCES repositories(id),
    context_repo_id TEXT REFERENCES repos(id),
    PRIMARY KEY(namespace_key,path)
);
CREATE TABLE repository_invite_targets (
    token TEXT NOT NULL REFERENCES invites(token),
    repository_id TEXT NOT NULL REFERENCES repositories(id),
    PRIMARY KEY(token,repository_id)
);
CREATE TABLE ownership_migrations (
    version TEXT PRIMARY KEY,
    report JSONB NOT NULL,
    completed_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
