-- codex team settings (.codex folder) snapshot pointer -- symmetric with claude/agents.
-- (Corrected FS store only reflecting and missing PG column in parity work: review found #2)
ALTER TABLE snapshots ADD COLUMN IF NOT EXISTS codex_settings TEXT;
