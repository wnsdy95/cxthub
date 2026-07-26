-- In-progress context pointer (session-based upsert — CLI hook auto-capture mirror).
-- Points to the latest hook capture snapshot that does not move the branch ref, and is resolved by deletion on commit.
CREATE TABLE IF NOT EXISTS pending_contexts (
    repo_id    TEXT NOT NULL REFERENCES repos(id) ON DELETE CASCADE,
    session_id TEXT NOT NULL,
    data       JSONB NOT NULL,
    PRIMARY KEY (repo_id, session_id)
);
