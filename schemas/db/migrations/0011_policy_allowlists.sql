-- 0011: Policy list user list -- array of allowed user IDs when policy='list'.
-- owner is always allowed (app layer PolicyAllows), regardless of the list.

ALTER TABLE workspaces
  ADD COLUMN IF NOT EXISTS secrets_allowed TEXT[] NOT NULL DEFAULT '{}',
  ADD COLUMN IF NOT EXISTS settings_allowed TEXT[] NOT NULL DEFAULT '{}';
