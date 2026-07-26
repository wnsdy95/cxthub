-- Session device label: Displays which device's session/token (CLI uses hostname, web uses UA summary).
-- For display/identification only — do not use for access control.
ALTER TABLE sessions ADD COLUMN IF NOT EXISTS label TEXT NOT NULL DEFAULT '';
