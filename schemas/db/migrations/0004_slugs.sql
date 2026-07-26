-- 0004: Name-based slug URL (/<username>/<workspace-slug>)
-- users.username  — Global unique handle (automatically generated from email localpart on first login)
-- workspaces.slug — Unique URL segment for owner (+ owner_username normalization)

ALTER TABLE users ADD COLUMN IF NOT EXISTS username TEXT;
CREATE UNIQUE INDEX IF NOT EXISTS users_username_key
    ON users (username) WHERE username IS NOT NULL AND username <> '';
COMMENT ON COLUMN users.username IS 'URL first segment handle. Automatically generated on first login (conflicts result in -2, -3, ...).';

ALTER TABLE workspaces ADD COLUMN IF NOT EXISTS slug TEXT;
ALTER TABLE workspaces ADD COLUMN IF NOT EXISTS owner_username TEXT;
CREATE UNIQUE INDEX IF NOT EXISTS workspaces_owner_slug_key
    ON workspaces (owner_id, slug) WHERE slug IS NOT NULL AND slug <> '';
COMMENT ON COLUMN workspaces.slug IS 'URL second segment. Automatically generated from name, unique within owner.';
COMMENT ON COLUMN workspaces.owner_username IS 'Normalized owner handle (for URL assembly). Legacy rows backfilled on query.';
