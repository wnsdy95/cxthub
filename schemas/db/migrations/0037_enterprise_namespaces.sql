-- 0037: Global namespaces and Enterprise organization/access foundations.
-- Enterprise membership does not grant Workspace membership. Exceptional
-- private-context reads require an explicit, expiring break-glass grant.

CREATE TABLE IF NOT EXISTS namespaces (
    id             TEXT PRIMARY KEY,
    slug           TEXT NOT NULL UNIQUE,
    kind           TEXT NOT NULL CHECK (kind IN ('user', 'enterprise')),
    user_id        TEXT UNIQUE REFERENCES users (id),
    enterprise_id  TEXT UNIQUE,
    created_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    CHECK (
        (kind = 'user' AND user_id IS NOT NULL AND enterprise_id IS NULL) OR
        (kind = 'enterprise' AND user_id IS NULL AND enterprise_id IS NOT NULL)
    )
);

-- Preserve every existing personal URL before allowing Enterprise slugs.
INSERT INTO namespaces (id, slug, kind, user_id)
SELECT 'ns_' || md5('user:' || id), username, 'user', id
FROM users
WHERE username IS NOT NULL AND username <> ''
ON CONFLICT DO NOTHING;

CREATE TABLE IF NOT EXISTS enterprises (
    id             TEXT PRIMARY KEY,
    namespace_id   TEXT NOT NULL UNIQUE REFERENCES namespaces (id) DEFERRABLE INITIALLY DEFERRED,
    name           TEXT NOT NULL,
    slug           TEXT NOT NULL UNIQUE,
    logo           TEXT NOT NULL DEFAULT '',
    created_by     TEXT NOT NULL REFERENCES users (id),
    created_at     TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS namespace_aliases (
    slug          TEXT PRIMARY KEY,
    namespace_id  TEXT NOT NULL REFERENCES namespaces (id) ON DELETE CASCADE,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- Cross-table global uniqueness cannot be expressed by a normal UNIQUE
-- constraint. Every writer for a current user slug, current Namespace slug,
-- or historical alias therefore takes the same transaction-scoped advisory
-- lock and rejects ownership by a different subject. This closes the race in
-- which user login and Enterprise creation both observe an unused slug.
CREATE OR REPLACE FUNCTION cxt_guard_user_namespace_slug()
RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF NEW.username IS NULL OR NEW.username = '' THEN
        RETURN NEW;
    END IF;
    PERFORM pg_advisory_xact_lock(hashtextextended(NEW.username, 0));
    IF EXISTS (
        SELECT 1
        FROM (
            SELECT n.kind, n.user_id
            FROM namespaces n
            WHERE n.slug = NEW.username
            UNION ALL
            SELECT n.kind, n.user_id
            FROM namespace_aliases a
            JOIN namespaces n ON n.id = a.namespace_id
            WHERE a.slug = NEW.username
        ) claimed
        WHERE NOT (claimed.kind = 'user' AND claimed.user_id = NEW.id)
    ) THEN
        RAISE EXCEPTION 'namespace slug % is already claimed', NEW.username
            USING ERRCODE = 'unique_violation';
    END IF;
    RETURN NEW;
END;
$$;

DROP TRIGGER IF EXISTS users_namespace_slug_guard ON users;
CREATE TRIGGER users_namespace_slug_guard
BEFORE INSERT OR UPDATE OF username ON users
FOR EACH ROW EXECUTE FUNCTION cxt_guard_user_namespace_slug();

CREATE OR REPLACE FUNCTION cxt_guard_namespace_slug()
RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    PERFORM pg_advisory_xact_lock(hashtextextended(NEW.slug, 0));
    IF EXISTS (
        SELECT 1 FROM namespace_aliases
        WHERE slug = NEW.slug AND namespace_id <> NEW.id
    ) THEN
        RAISE EXCEPTION 'namespace slug % is retained as an alias', NEW.slug
            USING ERRCODE = 'unique_violation';
    END IF;
    IF NEW.kind = 'enterprise' AND EXISTS (
        SELECT 1 FROM users WHERE username = NEW.slug
    ) THEN
        RAISE EXCEPTION 'namespace slug % is already claimed by a user', NEW.slug
            USING ERRCODE = 'unique_violation';
    END IF;
    IF NEW.kind = 'user' AND NOT EXISTS (
        SELECT 1 FROM users WHERE id = NEW.user_id AND username = NEW.slug
    ) THEN
        RAISE EXCEPTION 'personal namespace slug must match its user handle'
            USING ERRCODE = 'check_violation';
    END IF;
    RETURN NEW;
END;
$$;

DROP TRIGGER IF EXISTS namespaces_global_slug_guard ON namespaces;
CREATE TRIGGER namespaces_global_slug_guard
BEFORE INSERT OR UPDATE OF slug ON namespaces
FOR EACH ROW EXECUTE FUNCTION cxt_guard_namespace_slug();

CREATE OR REPLACE FUNCTION cxt_guard_namespace_alias_slug()
RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    PERFORM pg_advisory_xact_lock(hashtextextended(NEW.slug, 0));
    IF EXISTS (
        SELECT 1 FROM namespaces
        WHERE slug = NEW.slug AND id <> NEW.namespace_id
    ) THEN
        RAISE EXCEPTION 'namespace alias % is a current namespace', NEW.slug
            USING ERRCODE = 'unique_violation';
    END IF;
    IF EXISTS (
        SELECT 1
        FROM users u
        WHERE u.username = NEW.slug
          AND NOT EXISTS (
              SELECT 1 FROM namespaces n
              WHERE n.id = NEW.namespace_id AND n.kind = 'user' AND n.user_id = u.id
          )
    ) THEN
        RAISE EXCEPTION 'namespace alias % is a current user handle', NEW.slug
            USING ERRCODE = 'unique_violation';
    END IF;
    RETURN NEW;
END;
$$;

DROP TRIGGER IF EXISTS namespace_aliases_global_slug_guard ON namespace_aliases;
CREATE TRIGGER namespace_aliases_global_slug_guard
BEFORE INSERT OR UPDATE OF slug, namespace_id ON namespace_aliases
FOR EACH ROW EXECUTE FUNCTION cxt_guard_namespace_alias_slug();

ALTER TABLE namespaces
    DROP CONSTRAINT IF EXISTS namespaces_enterprise_fk;
ALTER TABLE namespaces
    ADD CONSTRAINT namespaces_enterprise_fk
    FOREIGN KEY (enterprise_id) REFERENCES enterprises (id)
    DEFERRABLE INITIALLY DEFERRED;

ALTER TABLE workspaces ADD COLUMN IF NOT EXISTS owner_namespace_id TEXT REFERENCES namespaces (id);
UPDATE workspaces w
SET owner_namespace_id = n.id
FROM namespaces n
WHERE w.owner_namespace_id IS NULL AND n.kind = 'user' AND n.user_id = w.owner_id;

DROP INDEX IF EXISTS workspaces_owner_slug_key;
CREATE UNIQUE INDEX IF NOT EXISTS workspaces_namespace_slug_key
    ON workspaces (owner_namespace_id, slug)
    WHERE owner_namespace_id IS NOT NULL AND slug IS NOT NULL AND slug <> '';
CREATE UNIQUE INDEX IF NOT EXISTS workspaces_legacy_owner_slug_key
    ON workspaces (owner_id, slug)
    WHERE owner_namespace_id IS NULL AND slug IS NOT NULL AND slug <> '';

CREATE TABLE IF NOT EXISTS enterprise_memberships (
    enterprise_id  TEXT NOT NULL REFERENCES enterprises (id) ON DELETE CASCADE,
    user_id        TEXT NOT NULL REFERENCES users (id),
    role           TEXT NOT NULL CHECK (role IN ('owner', 'admin', 'member')),
    created_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (enterprise_id, user_id)
);
CREATE INDEX IF NOT EXISTS enterprise_memberships_user_idx ON enterprise_memberships (user_id);

-- Enterprise ownership is transferable, but never optional. The deferred
-- check observes the transaction's final membership state, so one owner can
-- be replaced by another in the same transaction. The advisory lock closes
-- the concurrent two-owner race where each transaction tries to remove a
-- different owner after observing the other one.
CREATE OR REPLACE FUNCTION cxt_require_enterprise_owner()
RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE
    checked_enterprise_id TEXT;
BEGIN
    IF TG_OP = 'DELETE' THEN
        checked_enterprise_id := OLD.enterprise_id;
    ELSE
        checked_enterprise_id := NEW.enterprise_id;
    END IF;

    PERFORM pg_advisory_xact_lock(hashtextextended(checked_enterprise_id, 1));
    -- Cascading Enterprise deletion removes memberships intentionally.
    IF NOT EXISTS (SELECT 1 FROM enterprises WHERE id = checked_enterprise_id) THEN
        RETURN NULL;
    END IF;
    IF NOT EXISTS (
        SELECT 1 FROM enterprise_memberships
        WHERE enterprise_id = checked_enterprise_id AND role = 'owner'
    ) THEN
        RAISE EXCEPTION 'enterprise % must retain at least one owner', checked_enterprise_id
            USING ERRCODE = 'unique_violation';
    END IF;
    RETURN NULL;
END;
$$;

DROP TRIGGER IF EXISTS enterprise_memberships_owner_required ON enterprise_memberships;
CREATE CONSTRAINT TRIGGER enterprise_memberships_owner_required
AFTER INSERT OR UPDATE OF role OR DELETE ON enterprise_memberships
DEFERRABLE INITIALLY DEFERRED
FOR EACH ROW EXECUTE FUNCTION cxt_require_enterprise_owner();

CREATE TABLE IF NOT EXISTS enterprise_policies (
    enterprise_id               TEXT PRIMARY KEY REFERENCES enterprises (id) ON DELETE CASCADE,
    workspace_creation          TEXT NOT NULL DEFAULT 'admins' CHECK (workspace_creation IN ('admins', 'members')),
    default_workspace_visibility TEXT NOT NULL DEFAULT 'private' CHECK (default_workspace_visibility IN ('private', 'public')),
    allow_public_workspaces      BOOLEAN NOT NULL DEFAULT true,
    break_glass_enabled          BOOLEAN NOT NULL DEFAULT true,
    break_glass_max_minutes      INTEGER NOT NULL DEFAULT 60 CHECK (break_glass_max_minutes BETWEEN 5 AND 240),
    updated_by                   TEXT REFERENCES users (id),
    updated_at                   TIMESTAMPTZ NOT NULL DEFAULT now(),
    CHECK (allow_public_workspaces OR default_workspace_visibility <> 'public')
);

CREATE TABLE IF NOT EXISTS enterprise_audit_events (
    id             TEXT PRIMARY KEY,
    enterprise_id  TEXT NOT NULL REFERENCES enterprises (id) ON DELETE CASCADE,
    actor_id       TEXT NOT NULL REFERENCES users (id),
    action         TEXT NOT NULL,
    target_type    TEXT NOT NULL DEFAULT '',
    target_id      TEXT NOT NULL DEFAULT '',
    reason         TEXT NOT NULL DEFAULT '',
    created_at     TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS enterprise_audit_events_lookup_idx
    ON enterprise_audit_events (enterprise_id, created_at DESC);

CREATE TABLE IF NOT EXISTS enterprise_break_glass_grants (
    id             TEXT PRIMARY KEY,
    enterprise_id  TEXT NOT NULL REFERENCES enterprises (id) ON DELETE CASCADE,
    workspace_id   TEXT NOT NULL REFERENCES workspaces (id) ON DELETE CASCADE,
    user_id        TEXT NOT NULL REFERENCES users (id),
    reason         TEXT NOT NULL,
    created_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    expires_at     TIMESTAMPTZ NOT NULL,
    CHECK (expires_at > created_at)
);
CREATE INDEX IF NOT EXISTS enterprise_break_glass_active_idx
    ON enterprise_break_glass_grants (enterprise_id, workspace_id, user_id, expires_at DESC);

COMMENT ON TABLE namespaces IS 'Global URL namespace registry shared by users and Enterprises.';
COMMENT ON TABLE enterprise_memberships IS 'Enterprise administration only; never implies Workspace or Repository context access.';
COMMENT ON TABLE enterprise_break_glass_grants IS 'Owner-only exceptional read access, reason-bound and time-limited.';
