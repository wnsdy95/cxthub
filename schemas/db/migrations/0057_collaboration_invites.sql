CREATE TABLE IF NOT EXISTS collaboration_invites (
    id TEXT PRIMARY KEY,
    kind TEXT NOT NULL CHECK (kind IN ('organization', 'enterprise')),
    space_id TEXT NOT NULL,
    email TEXT NOT NULL,
    record JSONB NOT NULL
);
CREATE INDEX IF NOT EXISTS collaboration_invites_space ON collaboration_invites(kind, space_id);
CREATE INDEX IF NOT EXISTS collaboration_invites_recipient ON collaboration_invites(email);
