-- 0027: User UI display language personal setting (ko|en, '' = not set). The web switch stores this value,
-- The client applies this value at login to maintain consistent language settings across devices.
ALTER TABLE users ADD COLUMN IF NOT EXISTS locale text;
