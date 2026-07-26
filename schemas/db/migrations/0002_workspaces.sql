-- cxt PostgreSQL DDL — Migration 0002: User · Workspace · Membership · Invite
-- Apply order: psql -f 0002_workspaces.sql (after 0001)
-- Auth: Firebase ID token → users.id = Firebase uid. Visibility boundary = workspace (replaces existing team).

-- ---------------------------------------------------------------------------
-- users — Firebase authenticated user cache (based on uid).
-- ---------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS users (
    id          TEXT        PRIMARY KEY,              -- Firebase uid (or dev:<email>)
    email       TEXT        NOT NULL,
    name        TEXT        NOT NULL DEFAULT '',
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS users_email_idx ON users (email);

-- ---------------------------------------------------------------------------
-- workspaces — Multi-tenancy/visibility boundary. repo belongs to exactly 1 workspace.
-- ---------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS workspaces (
    id          TEXT        PRIMARY KEY,              -- Random id (ws_<hex>)
    name        TEXT        NOT NULL,
    owner_id    TEXT        NOT NULL REFERENCES users (id),
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS workspaces_owner_idx ON workspaces (owner_id);

-- ---------------------------------------------------------------------------
-- memberships — User ↔ Workspace (role: owner | member).
-- ---------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS memberships (
    workspace_id TEXT       NOT NULL REFERENCES workspaces (id),
    user_id      TEXT       NOT NULL REFERENCES users (id),
    role         TEXT       NOT NULL CHECK (role IN ('owner', 'member')),
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (workspace_id, user_id)
);
CREATE INDEX IF NOT EXISTS memberships_user_idx ON memberships (user_id);

-- ---------------------------------------------------------------------------
-- invites — Shared invite link/code. Join (accept) by token. Email is for display/matching.
-- ---------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS invites (
    token        TEXT        PRIMARY KEY,             -- Shared code (inv_<hex>)
    workspace_id TEXT        NOT NULL REFERENCES workspaces (id),
    email        TEXT        NOT NULL DEFAULT '',     -- Optional: specific email target (empty if anyone)
    role         TEXT        NOT NULL DEFAULT 'member' CHECK (role IN ('owner', 'member')),
    status       TEXT        NOT NULL DEFAULT 'pending' CHECK (status IN ('pending', 'accepted', 'revoked')),
    created_by   TEXT        NOT NULL REFERENCES users (id),
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    expires_at   TIMESTAMPTZ
);
CREATE INDEX IF NOT EXISTS invites_workspace_idx ON invites (workspace_id);

-- ---------------------------------------------------------------------------
-- repos: Workspace ownership (replaces existing team column).
-- ---------------------------------------------------------------------------
ALTER TABLE repos ADD COLUMN IF NOT EXISTS workspace_id TEXT REFERENCES workspaces (id);
CREATE INDEX IF NOT EXISTS repos_workspace_idx ON repos (workspace_id);

COMMENT ON TABLE workspaces IS 'cxt visibility boundary. repo·snapshot unit of multi-tenancy. owner invites members.';
COMMENT ON TABLE invites IS 'Shared invitation token. owner creates→link/code share→invited user accepts→membership created.';
