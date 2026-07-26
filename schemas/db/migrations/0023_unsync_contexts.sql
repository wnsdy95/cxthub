-- Push wait (unsync) pointer — (user, branch) local ahead commit chain tip.
-- Snapshot object is reached first by shadow push and is resolved by deletion during git push (ref forward).
CREATE TABLE IF NOT EXISTS unsync_contexts (
    repo_id  TEXT NOT NULL REFERENCES repos(id) ON DELETE CASCADE,
    username TEXT NOT NULL,
    branch   TEXT NOT NULL,
    data     JSONB NOT NULL,
    PRIMARY KEY (repo_id, username, branch)
);
