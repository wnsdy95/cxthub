-- 0007: Commit attachment settings folder snapshot (.claude/.agents, content-addressed)
ALTER TABLE snapshots ADD COLUMN IF NOT EXISTS claude_settings TEXT;
ALTER TABLE snapshots ADD COLUMN IF NOT EXISTS agents_settings TEXT;

CREATE TABLE IF NOT EXISTS settings_objects (
    repo_id TEXT NOT NULL REFERENCES repos (id),
    hash    TEXT NOT NULL,
    data    JSONB NOT NULL,
    PRIMARY KEY (repo_id, hash)
);
COMMENT ON TABLE settings_objects IS 'Agent settings folder bundle at commit time — references the snapshot of claude_settings/agents_settings.';
