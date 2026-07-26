-- 0009: Account alias (nickname) + workspace visibility.
--
-- nickname  — Display alias (free to change, URL irrelevant). Different from username (handle).
-- visibility — 'private' (default) | 'public'. Empty value/NULL is interpreted as private.
--              Changing visibility is only possible for the owner (app layer enforced).

ALTER TABLE users
  ADD COLUMN IF NOT EXISTS nickname TEXT NOT NULL DEFAULT '';

ALTER TABLE workspaces
  ADD COLUMN IF NOT EXISTS visibility TEXT NOT NULL DEFAULT 'private';
