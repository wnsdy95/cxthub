-- 0006: repo About(description/website/topics) + team basic settings bundle(.claude/.agents)
ALTER TABLE repos ADD COLUMN IF NOT EXISTS description TEXT;
ALTER TABLE repos ADD COLUMN IF NOT EXISTS website TEXT;
ALTER TABLE repos ADD COLUMN IF NOT EXISTS topics TEXT;  -- JSON array text

CREATE TABLE IF NOT EXISTS repo_settings (
    repo_id TEXT NOT NULL REFERENCES repos (id),
    kind    TEXT NOT NULL CHECK (kind IN ('claude','agents')),
    data    JSONB NOT NULL,
    PRIMARY KEY (repo_id, kind)
);
COMMENT ON TABLE repo_settings IS 'Team basic agent settings bundle — applied to local .claude/.agents on web upload, cxt settings pull.';
